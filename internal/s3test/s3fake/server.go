// Package s3fake is an in-process S3 server for tests and the demo: pure Go, no dependencies beyond
// the standard library, no network beyond one loopback listener.
//
// It implements exactly (and strictly) the S3 behaviour incus-backup relies on, as AWS S3 behaves:
//
//   - path-style addressing, AWS Signature V4 (header form) verified from the specification,
//     including aws-chunked bodies with per-chunk signatures; bad signatures, unknown access keys,
//     tampered headers and clock skew are refused with the S3 error codes;
//   - buckets: create (optionally with object lock), head, location, delete, list;
//   - objects: single-part PutObject with atomic If-None-Match: * (412 PreconditionFailed), GetObject
//     with Range and If-(None-)Match, HeadObject, DeleteObject, DeleteObjects, CopyObject, user
//     metadata, ETag (MD5), Last-Modified;
//   - ListObjectsV2 (prefix, delimiter, max-keys, continuation tokens, start-after, url encoding)
//     and ListObjectVersions;
//   - versioning, lifecycle and object-lock configuration (stored and answered; retention is not
//     enforced);
//   - multipart upload, tagging, ACLs, policies and everything else answer 501 NotImplemented, so a
//     code change that starts using them fails loudly instead of being half-served.
//
// For tests it can inject faults (error on the Nth request, SlowDown/5xx bursts, latency, dropped
// connections mid-body or instead of the answer), logs every request, and counts them. It can keep
// its data in a directory so that a restarted process finds it again (the demo).
package s3fake

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const serverName = "incus-backup-s3fake"

func itoa(n int) string { return strconv.Itoa(n) }

// Defaults.
const (
	DefaultAccess = "testaccess"
	DefaultSecret = "testsecret-12345"
	DefaultRegion = "us-east-1"
)

// Quirk deliberately breaks one behaviour of the fake. They exist so that the conformance suite
// can be shown to FAIL against a wrong server (mutation tests): a conformance suite that cannot
// tell a broken server from a good one proves nothing.
type Quirk string

const (
	QuirkIgnoreIfNoneMatch  Quirk = "ignore-if-none-match" // If-None-Match: * overwrites
	QuirkNonAtomicPut       Quirk = "non-atomic-put"       // exists-check and write are not one step
	QuirkAcceptAnySignature Quirk = "accept-any-signature" // nobody is ever refused
	QuirkNoContinuation     Quirk = "no-continuation"      // pages are cut at max-keys without IsTruncated
	QuirkUnsortedListing    Quirk = "unsorted-listing"     // keys come back in reverse order
	QuirkIgnoreRange        Quirk = "ignore-range"         // Range is answered with the whole object
	QuirkTornPutVisible     Quirk = "torn-put-visible"     // a torn upload leaves the partial object behind
	QuirkIgnoreDelimiter    Quirk = "ignore-delimiter"     // no common prefixes
	QuirkDeleteMissing404   Quirk = "delete-missing-404"   // deleting a missing key is an error
	QuirkIgnorePrefix       Quirk = "ignore-prefix"        // listing ignores the prefix
	QuirkWrongETag          Quirk = "wrong-etag"           // ETag is not the MD5
	QuirkMissingKeyIs200    Quirk = "missing-key-is-empty" // GET of a missing key returns an empty object
	QuirkNoVersioningMarker Quirk = "no-delete-marker"     // versioned delete removes the data
)

// Options configure a Server. The zero value is a usable in-memory server on 127.0.0.1.
type Options struct {
	Addr   string // listen address, default 127.0.0.1:0
	Region string // default us-east-1
	Access string // root access key, default DefaultAccess
	Secret string // root secret key, default DefaultSecret
	// Dir keeps buckets and objects on disk (and loads what is there); "" = memory only.
	Dir     string
	Quirks  []Quirk
	Latency time.Duration // added to every request
}

// Request is one entry of the request log.
type Request struct {
	Seq      int64
	Time     time.Time
	Method   string
	Path     string
	RawQuery string
	Op       string // S3 operation name, e.g. "PutObject", "ListObjectsV2"
	Bucket   string
	Key      string
	Header   http.Header
	Status   int           // HTTP status sent; 0 when the connection was dropped
	Bytes    int64         // payload bytes received
	Fault    string        // description of the injected fault that hit this request, if any
	Duration time.Duration // handling time
}

// Server is a running fake S3 service.
type Server struct {
	opt Options
	st  *store
	ln  net.Listener
	srv *http.Server

	mu       sync.Mutex
	users    map[string]string
	rules    []*Rule
	log      []Request
	counts   map[string]int
	quirks   map[Quirk]bool
	latency  time.Duration
	inflight int
	peak     int

	seq    atomic.Int64
	closed chan struct{}
	once   sync.Once
}

// New starts a server and returns once it listens.
func New(opt Options) (*Server, error) {
	if opt.Addr == "" {
		opt.Addr = "127.0.0.1:0"
	}
	if opt.Region == "" {
		opt.Region = DefaultRegion
	}
	if opt.Access == "" {
		opt.Access = DefaultAccess
	}
	if opt.Secret == "" {
		opt.Secret = DefaultSecret
	}
	st, err := newStore(opt.Dir)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", opt.Addr)
	if err != nil {
		return nil, err
	}
	s := &Server{opt: opt, st: st, ln: ln, users: map[string]string{opt.Access: opt.Secret}, counts: map[string]int{},
		quirks: map[Quirk]bool{}, latency: opt.Latency, closed: make(chan struct{})}
	for _, q := range opt.Quirks {
		s.quirks[q] = true
	}
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Endpoint is host:port (plain HTTP).
func (s *Server) Endpoint() string { return s.ln.Addr().String() }

// AccessKey and SecretKey are the root credentials.
func (s *Server) AccessKey() string { return s.opt.Access }
func (s *Server) SecretKey() string { return s.opt.Secret }

// Region is the region the server answers for.
func (s *Server) Region() string { return s.opt.Region }

// Close stops the server and drops its connections. Disk data stays.
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.closed)
		_ = s.srv.Close() // not Shutdown: a test server never waits for its clients
	})
}

// AddUser adds a credential; RemoveUser makes it unknown (a revoked key).
func (s *Server) AddUser(access, secret string) {
	s.mu.Lock()
	s.users[access] = secret
	s.mu.Unlock()
}

func (s *Server) RemoveUser(access string) {
	s.mu.Lock()
	delete(s.users, access)
	s.mu.Unlock()
}

func (s *Server) secretFor(access string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, ok := s.users[access]
	return sec, ok
}

// SetQuirk switches a deliberate defect on or off.
func (s *Server) SetQuirk(q Quirk, on bool) {
	s.mu.Lock()
	s.quirks[q] = on
	s.mu.Unlock()
}

func (s *Server) quirk(q Quirk) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quirks[q]
}

// SetLatency delays every request by d.
func (s *Server) SetLatency(d time.Duration) {
	s.mu.Lock()
	s.latency = d
	s.mu.Unlock()
}

// ---- request log and counters ----------------------------------------------------------------

// Requests returns a copy of the request log.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.log...)
}

// Count is the number of requests of operation op seen since the last ResetLog ("" = all).
func (s *Server) Count(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op == "" {
		return len(s.log)
	}
	return s.counts[op]
}

// ResetLog clears the log and the counters (not the rules).
func (s *Server) ResetLog() {
	s.mu.Lock()
	s.log, s.counts, s.peak = nil, map[string]int{}, s.inflight
	s.mu.Unlock()
}

// PeakInflight is the highest number of requests handled at the same time since the last ResetLog.
func (s *Server) PeakInflight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// HeaderValues returns, for every logged request of op, the value of header h (for example
// "X-Amz-Copy-Source").
func (s *Server) HeaderValues(op, h string) []string {
	var out []string
	for _, r := range s.Requests() {
		if (op == "" || r.Op == op) && r.Header.Get(h) != "" {
			out = append(out, r.Header.Get(h))
		}
	}
	return out
}

// ---- state inspection (tests) -----------------------------------------------------------------

// Object returns the current body and ETag of bucket/key.
func (s *Server) Object(bucket, key string) (data []byte, ok bool) {
	b := s.st.bucket(bucket)
	if b == nil {
		return nil, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	v := b.current(key)
	if v == nil {
		return nil, false
	}
	rd, err := v.reader()
	if err != nil {
		return nil, false
	}
	defer rd.Close()
	buf := make([]byte, v.Size)
	n, _ := rd.Read(buf)
	for int64(n) < v.Size {
		m, err := rd.Read(buf[n:])
		n += m
		if err != nil {
			break
		}
	}
	return buf, true
}

// Keys lists the live keys of a bucket, sorted.
func (s *Server) Keys(bucket string) []string {
	b := s.st.bucket(bucket)
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []string
	for _, k := range b.keys {
		if b.current(k) != nil {
			out = append(out, k)
		}
	}
	return out
}

// EnsureBucket creates the bucket if it does not exist (a no-op otherwise).
func (s *Server) EnsureBucket(name string) error {
	if !validBucketName(name) {
		return fmt.Errorf("s3fake: invalid bucket name %q", name)
	}
	_, _, err := s.st.create(name, false)
	return err
}

// Buckets lists the bucket names.
func (s *Server) Buckets() []string { return s.st.names() }

// ---- fault injection --------------------------------------------------------------------------

// Rule injects a fault into the requests it matches. A matching request is counted; the first Skip
// matches pass untouched, the next Times matches (0 = all of them) get the Action.
type Rule struct {
	// Match: empty fields match everything. Key is a prefix of the object key. Func, if set, decides too.
	Op, Method, Bucket, Key string
	Func                    func(*Request) bool

	Skip  int
	Times int

	Action Action

	s     *Server
	seen  int
	fired int
}

// Action is what a Rule does to a request.
type Action struct {
	// Delay before anything else happens (interrupted by Close).
	Delay time.Duration
	// Status and Code answer with that S3 error instead of doing the operation, for example
	// 503 SlowDown or 500 InternalError (both retried by minio-go) or 403 AccessDenied.
	Status int
	Code   string
	// Drop resets the TCP connection before the request is looked at.
	Drop bool
	// DropAfterBytes (> 0) resets the connection once that many body bytes arrived: a torn upload.
	// The object is not stored (unless QuirkTornPutVisible).
	DropAfterBytes int64
	// DropResponse does the whole operation and then resets the connection instead of answering:
	// the client does not know whether its request took effect.
	DropResponse bool
	// Hang never answers (until the client gives up or the server closes).
	Hang bool
}

// AddRule installs a rule and returns it; it stays active until Remove.
func (s *Server) AddRule(r Rule) *Rule {
	r.s = s
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &r
	s.rules = append(s.rules, p)
	return p
}

// Hits is how many requests the rule acted on.
func (r *Rule) Hits() int {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.fired
}

// Seen is how many requests matched the rule (acted on or not).
func (r *Rule) Seen() int {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.seen
}

// Remove uninstalls the rule.
func (r *Rule) Remove() {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for i, x := range r.s.rules {
		if x == r {
			r.s.rules = append(r.s.rules[:i], r.s.rules[i+1:]...)
			return
		}
	}
}

// ClearRules removes every rule.
func (s *Server) ClearRules() {
	s.mu.Lock()
	s.rules = nil
	s.mu.Unlock()
}

// FailNth makes the nth request of operation op ("" = any) fail with an S3 error.
func (s *Server) FailNth(op string, n int, status int, code string) *Rule {
	return s.AddRule(Rule{Op: op, Skip: n - 1, Times: 1, Action: Action{Status: status, Code: code}})
}

// FailNext fails the next `times` requests of operation op with an S3 error.
func (s *Server) FailNext(op string, times int, status int, code string) *Rule {
	return s.AddRule(Rule{Op: op, Times: times, Action: Action{Status: status, Code: code}})
}

// SlowDownBurst answers the next n requests (any operation) with 503 SlowDown. Keep n below the
// client's retry budget (minio-go: 10 retries) unless the point is to exhaust it.
func (s *Server) SlowDownBurst(n int) *Rule { return s.FailNext("", n, 503, "SlowDown") }

// DropNthConnection resets the connection of the nth request of operation op.
func (s *Server) DropNth(op string, n int) *Rule {
	return s.AddRule(Rule{Op: op, Skip: n - 1, Times: 1, Action: Action{Drop: true}})
}

// TearNextUpload resets the connection of the next PutObject after `bytes` body bytes.
func (s *Server) TearNextUpload(bytes int64) *Rule {
	return s.AddRule(Rule{Op: "PutObject", Times: 1, Action: Action{DropAfterBytes: bytes}})
}

// LoseNextResponse performs the next request of op fully and then drops the connection.
func (s *Server) LoseNextResponse(op string) *Rule {
	return s.AddRule(Rule{Op: op, Times: 1, Action: Action{DropResponse: true}})
}

func (s *Server) matchRules(rq *Request) (Action, *Rule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rules {
		if r.Op != "" && r.Op != rq.Op || r.Method != "" && r.Method != rq.Method || r.Bucket != "" && r.Bucket != rq.Bucket ||
			r.Key != "" && !strings.HasPrefix(rq.Key, r.Key) || r.Func != nil && !r.Func(rq) {
			continue
		}
		r.seen++
		if r.seen <= r.Skip || r.Times > 0 && r.fired >= r.Times {
			continue
		}
		r.fired++
		return r.Action, r, true
	}
	return Action{}, nil, false
}

// ---- HTTP -------------------------------------------------------------------------------------

type reqCtx struct {
	id      string
	bucket  string
	key     string
	op      string
	rec     *Request
	hitDrop bool
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.status == 0 {
		w.status = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// mute swallows the answer of a request whose connection is dropped afterwards.
type mute struct {
	h      http.Header
	status int
}

func (m *mute) Header() http.Header { return m.h }
func (m *mute) Write(p []byte) (int, error) {
	if m.status == 0 {
		m.status = 200
	}
	return len(p), nil
}
func (m *mute) WriteHeader(c int) {
	if m.status == 0 {
		m.status = c
	}
}

var errDropped = errors.New("s3fake: connection dropped by fault injection")

// abortConn resets the TCP connection (RST) without answering.
func abortConn(w http.ResponseWriter) {
	if sw, ok := w.(*statusWriter); ok {
		w = sw.ResponseWriter
	}
	if hj, ok := w.(http.Hijacker); ok {
		if c, _, err := hj.Hijack(); err == nil {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			_ = c.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

func (s *Server) ServeHTTP(w0 http.ResponseWriter, r *http.Request) {
	start := time.Now()
	seq := s.seq.Add(1)
	rec := &Request{Seq: seq, Time: start, Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone()}
	rc := &reqCtx{id: fmt.Sprintf("%016X", seq), rec: rec}
	rc.bucket, rc.key = splitPath(r.URL.Path)
	rec.Bucket, rec.Key = rc.bucket, rc.key
	rc.op = classify(r, rc)
	rec.Op = rc.op
	sw := &statusWriter{ResponseWriter: w0}
	var w http.ResponseWriter = sw

	s.mu.Lock()
	s.inflight++
	if s.inflight > s.peak {
		s.peak = s.inflight
	}
	lat := s.latency
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		rec.Status = sw.status
		rec.Duration = time.Since(start)
		s.log = append(s.log, *rec)
		s.counts[rec.Op]++
		s.mu.Unlock()
	}()

	if lat > 0 && !s.sleep(r, lat) {
		return
	}
	act, rule, hit := s.matchRules(rec)
	var dropAfter int64
	if hit {
		rec.Fault = fmt.Sprintf("%+v", act)
		_ = rule
		if act.Delay > 0 && !s.sleep(r, act.Delay) {
			return
		}
		switch {
		case act.Drop:
			abortConn(w)
			return
		case act.Hang:
			select {
			case <-r.Context().Done():
			case <-s.closed:
			}
			abortConn(w)
			return
		case act.Status != 0 || act.Code != "":
			st := act.Status
			if st == 0 {
				st = apiErr(act.Code).Status
			}
			e := apiErr(act.Code)
			e.Status = st
			writeError(w, r, rc, e)
			return
		}
		dropAfter = act.DropAfterBytes
		if act.DropResponse {
			m := &mute{h: http.Header{}}
			s.dispatch(m, r, rc, dropAfter)
			rec.Status = m.status
			abortConn(w)
			return
		}
	}
	s.dispatch(w, r, rc, dropAfter)
}

// sleep waits d; false if the client left or the server closed meanwhile.
func (s *Server) sleep(r *http.Request, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	case <-s.closed:
		return false
	}
}

func splitPath(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	b, k, _ := strings.Cut(p, "/")
	return b, k
}

// subresources that select a different operation than the plain one; the ones the fake does not
// implement are answered with 501.
var unsupportedSub = []string{"acl", "policy", "policyStatus", "tagging", "cors", "encryption", "replication", "notification", "website",
	"logging", "accelerate", "analytics", "inventory", "metrics", "requestPayment", "ownershipControls", "publicAccessBlock",
	"intelligent-tiering", "retention", "legal-hold", "attributes", "restore", "select", "torrent", "uploads", "uploadId", "partNumber"}

func classify(r *http.Request, rc *reqCtx) string {
	q := r.URL.Query()
	has := func(k string) bool { _, ok := q[k]; return ok }
	for _, k := range unsupportedSub {
		if has(k) && rc.bucket != "" {
			switch {
			case k == "uploads" && r.Method == http.MethodPost:
				return "CreateMultipartUpload"
			case k == "uploads":
				return "ListMultipartUploads"
			case k == "uploadId" && r.Method == http.MethodPut || k == "partNumber":
				return "UploadPart"
			case k == "uploadId" && r.Method == http.MethodPost:
				return "CompleteMultipartUpload"
			case k == "uploadId" && r.Method == http.MethodDelete:
				return "AbortMultipartUpload"
			case k == "uploadId":
				return "ListParts"
			}
			return "Unsupported:" + k
		}
	}
	switch {
	case rc.bucket == "":
		if r.Method == http.MethodGet {
			return "ListBuckets"
		}
		return "Unsupported:service"
	case rc.key == "":
		switch r.Method {
		case http.MethodGet:
			switch {
			case has("location"):
				return "GetBucketLocation"
			case has("versioning"):
				return "GetBucketVersioning"
			case has("lifecycle"):
				return "GetBucketLifecycle"
			case has("object-lock"):
				return "GetObjectLockConfiguration"
			case has("versions"):
				return "ListObjectVersions"
			case q.Get("list-type") == "2":
				return "ListObjectsV2"
			}
			return "ListObjectsV1"
		case http.MethodPut:
			switch {
			case has("versioning"):
				return "PutBucketVersioning"
			case has("lifecycle"):
				return "PutBucketLifecycle"
			case has("object-lock"):
				return "PutObjectLockConfiguration"
			}
			return "CreateBucket"
		case http.MethodDelete:
			if has("lifecycle") {
				return "DeleteBucketLifecycle"
			}
			return "DeleteBucket"
		case http.MethodHead:
			return "HeadBucket"
		case http.MethodPost:
			if has("delete") {
				return "DeleteObjects"
			}
		}
		return "Unsupported:bucket-" + r.Method
	}
	switch r.Method {
	case http.MethodGet:
		return "GetObject"
	case http.MethodHead:
		return "HeadObject"
	case http.MethodPut:
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			return "CopyObject"
		}
		return "PutObject"
	case http.MethodDelete:
		return "DeleteObject"
	}
	return "Unsupported:object-" + r.Method
}
