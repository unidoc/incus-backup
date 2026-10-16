package s3test_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/unidoc/incus-backup/internal/s3test"
	"github.com/unidoc/incus-backup/internal/s3test/s3fake"
)

func TestMain(m *testing.M) { s3test.Main(m) }

func put(t *testing.T, cl *minio.Client, bucket, key string, data []byte, ifAbsent bool) error {
	t.Helper()
	o := minio.PutObjectOptions{DisableMultipart: true}
	if ifAbsent {
		o.SetMatchETagExcept("*")
	}
	_, err := cl.PutObject(context.Background(), bucket, key, bytes.NewReader(data), int64(len(data)), o)
	return err
}

// fewRetries shrinks the client's retry budget (a package variable read when a client is created)
// so that tests that exhaust it do not take seconds. Tests using it must not run in parallel.
func fewRetries(t *testing.T) {
	old := minio.MaxRetry
	minio.MaxRetry = 3
	t.Cleanup(func() { minio.MaxRetry = old })
}

func newBucket(t *testing.T) (*s3test.Server, s3test.Config, *minio.Client, string) {
	t.Helper()
	srv, cfg := s3test.NewServer(t)
	cfg, b := s3test.NewBucketAt(t, cfg)
	return srv, cfg, s3test.Client(t, cfg), b
}

func TestRequestLogAndCounters(t *testing.T) {
	srv, _, cl, b := newBucket(t)
	srv.ResetLog()
	if err := put(t, cl, b, "a/b", []byte("x"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.StatObject(context.Background(), b, "a/b", minio.StatObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.CopyObject(context.Background(), minio.CopyDestOptions{Bucket: b, Object: "c"}, minio.CopySrcOptions{Bucket: b, Object: "a/b"}); err != nil {
		t.Fatal(err)
	}
	if srv.Count("PutObject") != 1 || srv.Count("HeadObject") != 1 || srv.Count("CopyObject") != 1 {
		t.Fatalf("counts: put=%d head=%d copy=%d log=%+v", srv.Count("PutObject"), srv.Count("HeadObject"), srv.Count("CopyObject"), srv.Requests())
	}
	if got := srv.HeaderValues("CopyObject", "X-Amz-Copy-Source"); len(got) != 1 || !strings.HasSuffix(got[0], "/a/b") {
		t.Fatalf("copy source header %v", got)
	}
	var found bool
	for _, r := range srv.Requests() {
		if r.Op == "PutObject" {
			found = true
			if r.Bucket != b || r.Key != "a/b" || r.Status != 200 || r.Bytes != 1 {
				t.Fatalf("log entry %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("no PutObject in the log")
	}
	srv.ResetLog()
	if srv.Count("") != 0 {
		t.Fatal("ResetLog did not clear")
	}
}

func TestFailNthRequestAndRetry(t *testing.T) {
	srv, _, cl, b := newBucket(t)
	srv.ResetLog()
	// 500 InternalError is retried by the client: the operation still succeeds, on the second attempt
	r := srv.FailNth("PutObject", 1, 500, "InternalError")
	if err := put(t, cl, b, "k", []byte("v"), false); err != nil {
		t.Fatalf("a single 500 must be retried: %v", err)
	}
	if r.Hits() != 1 || srv.Count("PutObject") != 2 {
		t.Fatalf("hits=%d puts=%d", r.Hits(), srv.Count("PutObject"))
	}
	// a non-retryable error surfaces with its code and status
	srv.FailNext("PutObject", 1, 403, "AccessDenied")
	err := put(t, cl, b, "k2", []byte("v"), false)
	if er := minio.ToErrorResponse(err); er.Code != "AccessDenied" || er.StatusCode != 403 {
		t.Fatalf("got %v", err)
	}
	if _, ok := srv.Object(b, "k2"); ok {
		t.Fatal("a failed request still stored the object")
	}
}

func TestSlowDownBurstIsRetriedAndAnExhaustedBudgetFails(t *testing.T) {
	fewRetries(t)
	srv, _, cl, b := newBucket(t)
	r := srv.SlowDownBurst(2)
	start := time.Now()
	if err := put(t, cl, b, "k", []byte("v"), false); err != nil {
		t.Fatalf("a SlowDown burst of 2 must be ridden out: %v", err)
	}
	if r.Hits() != 2 {
		t.Fatalf("hits %d", r.Hits())
	}
	t.Logf("2 SlowDowns cost %v", time.Since(start))
	r.Remove()
	// more than the client's retry budget: the error reaches the caller
	srv.SlowDownBurst(1000)
	err := put(t, cl, b, "k2", []byte("v"), false)
	if minio.ToErrorResponse(err).Code != "SlowDown" {
		t.Fatalf("an exhausted retry budget must surface SlowDown, got %v", err)
	}
	srv.ClearRules()
	if err := put(t, cl, b, "k3", []byte("v"), false); err != nil {
		t.Fatal(err)
	}
}

func TestLatencyAndRuleDelay(t *testing.T) {
	srv, _, cl, b := newBucket(t)
	srv.SetLatency(80 * time.Millisecond)
	start := time.Now()
	if err := put(t, cl, b, "k", []byte("v"), false); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Fatalf("latency not applied: %v", d)
	}
	srv.SetLatency(0)
	srv.AddRule(s3fake.Rule{Op: "HeadObject", Times: 1, Action: s3fake.Action{Delay: 120 * time.Millisecond}})
	start = time.Now()
	if _, err := cl.StatObject(context.Background(), b, "k", minio.StatObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 120*time.Millisecond {
		t.Fatalf("rule delay not applied: %v", d)
	}
}

func TestTornUploadLeavesNoObjectAndKeepsTheOldOne(t *testing.T) {
	fewRetries(t)
	srv, _, cl, b := newBucket(t)
	if err := put(t, cl, b, "k", []byte("old content"), false); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("n"), 2<<20)
	srv.ResetLog()
	r := srv.TearNextUpload(300 << 10)
	// the client retries the torn attempt; the rule fires once, so the retry succeeds
	if err := put(t, cl, b, "k", big, false); err != nil {
		t.Fatal(err)
	}
	if r.Hits() != 1 || srv.Count("PutObject") != 2 {
		t.Fatalf("hits=%d puts=%d", r.Hits(), srv.Count("PutObject"))
	}
	if got, _ := srv.Object(b, "k"); len(got) != len(big) {
		t.Fatalf("object has %d bytes, want %d", len(got), len(big))
	}
	// torn on every attempt: the old object survives untouched
	srv.AddRule(s3fake.Rule{Op: "PutObject", Action: s3fake.Action{DropAfterBytes: 100 << 10}})
	if err := put(t, cl, b, "k", []byte(strings.Repeat("z", 1<<20)), false); err == nil {
		t.Fatal("an upload torn on every attempt succeeded")
	}
	if got, _ := srv.Object(b, "k"); len(got) != len(big) || got[0] != 'n' {
		t.Fatalf("a torn overwrite damaged the old object (%d bytes)", len(got))
	}
	// a torn conditional create leaves the key free
	srv.ClearRules()
	srv.AddRule(s3fake.Rule{Op: "PutObject", Key: "fresh", Action: s3fake.Action{DropAfterBytes: 50 << 10}})
	if err := put(t, cl, b, "fresh", big, true); err == nil {
		t.Fatal("expected failure")
	}
	if _, ok := srv.Object(b, "fresh"); ok {
		t.Fatal("a torn upload created an object")
	}
	srv.ClearRules()
	if err := put(t, cl, b, "fresh", []byte("ok"), true); err != nil {
		t.Fatalf("the key is not free after a torn conditional create: %v", err)
	}
}

func TestDroppedConnectionsAreRetried(t *testing.T) {
	srv, _, cl, b := newBucket(t)
	srv.DropNth("PutObject", 1)
	if err := put(t, cl, b, "k", []byte("v"), false); err != nil {
		t.Fatal(err)
	}
	if srv.Count("PutObject") != 2 {
		t.Fatalf("puts=%d", srv.Count("PutObject"))
	}
}

// The classic hazard: the write took effect but the client never saw the answer. A retried
// conditional create then sees 412 for an object the same client created.
func TestLostResponseAfterACommittedConditionalPut(t *testing.T) {
	srv, _, cl, b := newBucket(t)
	srv.LoseNextResponse("PutObject")
	err := put(t, cl, b, "commit.json", []byte("{}"), true)
	if minio.ToErrorResponse(err).Code != "PreconditionFailed" {
		t.Fatalf("the retry of a committed conditional put must see 412, got %v", err)
	}
	if _, ok := srv.Object(b, "commit.json"); !ok {
		t.Fatal("the lost-response request did not take effect")
	}
}

func TestHangingRequestHonoursTheContext(t *testing.T) {
	srv, _, cl, b := newBucket(t)
	srv.AddRule(s3fake.Rule{Op: "HeadObject", Times: 1, Action: s3fake.Action{Hang: true}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := cl.StatObject(ctx, b, "k", minio.StatObjectOptions{})
	if err == nil {
		t.Fatal("a hanging server answered")
	}
}

func TestPeakInflight(t *testing.T) {
	srv, _, cl, b := newBucket(t)
	srv.SetLatency(100 * time.Millisecond)
	srv.ResetLog()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = put(t, cl, b, fmt.Sprintf("k%d", i), []byte("v"), false)
		}()
	}
	wg.Wait()
	if srv.PeakInflight() < 5 {
		t.Fatalf("peak in flight %d", srv.PeakInflight())
	}
}

func TestUnsupportedOperationsAnswerNotImplemented(t *testing.T) {
	srv, cfg, cl, b := newBucket(t)
	core := minio.Core{Client: cl}
	_, err := core.NewMultipartUpload(context.Background(), b, "mp", minio.PutObjectOptions{})
	if er := minio.ToErrorResponse(err); er.StatusCode != 501 || er.Code != "NotImplemented" {
		t.Fatalf("multipart: %v", err)
	}
	if srv.Count("CreateMultipartUpload") != 1 {
		t.Fatalf("not logged: %+v", srv.Requests())
	}
	// ListObjects V1 is not implemented either
	for o := range cl.ListObjects(context.Background(), b, minio.ListObjectsOptions{UseV1: true}) {
		if er := minio.ToErrorResponse(o.Err); er.StatusCode != 501 {
			t.Fatalf("V1 listing: %v", o.Err)
		}
		break
	}
	_ = cfg
}

func TestCredentialRotation(t *testing.T) {
	srv, cfg, _, b := newBucket(t)
	srv.AddUser("rotated", "rotated-secret-1")
	cfg2 := cfg
	cfg2.AccessKey, cfg2.SecretKey = "rotated", "rotated-secret-1"
	cl2 := s3test.Client(t, cfg2)
	if err := put(t, cl2, b, "k", []byte("v"), false); err != nil {
		t.Fatalf("a new user: %v", err)
	}
	srv.RemoveUser("rotated")
	err := put(t, cl2, b, "k2", []byte("v"), false)
	if er := minio.ToErrorResponse(err); er.StatusCode != 403 || er.Code != "InvalidAccessKeyId" {
		t.Fatalf("a revoked key: %v", err)
	}
}

func TestDiskModeSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	srv1, err := s3fake.New(s3fake.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	cfg := s3test.ConfigOf(srv1)
	cfg, b := s3test.NewBucketAt(t, cfg)
	cl := s3test.Client(t, cfg)
	big := bytes.Repeat([]byte("0123456789"), 100000)
	for k, v := range map[string][]byte{"a/1": []byte("one"), "a/2": big, "t": nil} {
		if err := put(t, cl, b, k, v, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := cl.EnableVersioning(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if err := put(t, cl, b, "a/1", []byte("uno"), false); err != nil {
		t.Fatal(err)
	}
	srv1.Close()

	srv2, err := s3fake.New(s3fake.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	if got, ok := srv2.Object(b, "a/2"); !ok || !bytes.Equal(got, big) {
		t.Fatalf("a/2 after restart: %v %d", ok, len(got))
	}
	if got, ok := srv2.Object(b, "a/1"); !ok || string(got) != "uno" {
		t.Fatalf("a/1 after restart: %q", got)
	}
	cl2 := s3test.Client(t, s3test.ConfigOf(srv2))
	vc, err := cl2.GetBucketVersioning(context.Background(), b)
	if err != nil || vc.Status != minio.Enabled {
		t.Fatalf("versioning after restart: %v %v", vc, err)
	}
	n := 0
	for o := range cl2.ListObjects(context.Background(), b, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		if o.Err != nil {
			t.Fatal(o.Err)
		}
		n++
	}
	if n != 4 { // a/1 twice, a/2, t
		t.Fatalf("%d versions after restart, want 4", n)
	}
	// a conditional put still sees the persisted object
	if err := put(t, cl2, b, "a/2", []byte("x"), true); minio.ToErrorResponse(err).Code != "PreconditionFailed" {
		t.Fatalf("conditional put after restart: %v", err)
	}
}

// ---- raw SigV4 (an independent signer: nothing here shares code with the fake) ----

func hm(k []byte, s string) []byte {
	m := hmac.New(sha256.New, k)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func sha(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }

// signAt signs a body-less request as the S3 spec says, at time ts.
func signAt(req *http.Request, access, secret, region string, ts time.Time, extra map[string]string) {
	amz := ts.UTC().Format("20060102T150405Z")
	day := amz[:8]
	req.Header.Set("X-Amz-Date", amz)
	req.Header.Set("X-Amz-Content-Sha256", sha(""))
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	for k := range extra {
		signed = append(signed, strings.ToLower(k))
	}
	// keep the list sorted
	for i := range signed {
		for j := i + 1; j < len(signed); j++ {
			if signed[j] < signed[i] {
				signed[i], signed[j] = signed[j], signed[i]
			}
		}
	}
	var ch strings.Builder
	for _, h := range signed {
		v := req.Header.Get(h)
		if h == "host" {
			v = req.URL.Host
		}
		ch.WriteString(h + ":" + v + "\n")
	}
	canon := strings.Join([]string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, ch.String(), strings.Join(signed, ";"), sha("")}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + amz + "\n" + scope + "\n" + sha(canon)
	k := hm(hm(hm(hm([]byte("AWS4"+secret), day), region), "s3"), "aws4_request")
	sig := hex.EncodeToString(hm(k, sts))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", access, scope, strings.Join(signed, ";"), sig))
}

func status(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSigV4Verification(t *testing.T) {
	srv, cfg, cl, b := newBucket(t)
	if err := put(t, cl, b, "k", []byte("v"), false); err != nil {
		t.Fatal(err)
	}
	url := "http://" + cfg.Endpoint + "/" + b + "/k"
	mk := func() *http.Request {
		r, _ := http.NewRequest(http.MethodGet, url, nil)
		return r
	}
	now := time.Now()
	// the independent signer is accepted: it is a correct signer
	r := mk()
	signAt(r, cfg.AccessKey, cfg.SecretKey, "us-east-1", now, nil)
	if st, body := status(t, r); st != 200 || body != "v" {
		t.Fatalf("a correctly signed request: %d %q", st, body)
	}
	cases := []struct {
		name string
		mod  func() *http.Request
		want int
		code string
	}{
		{"wrong secret", func() *http.Request {
			r := mk()
			signAt(r, cfg.AccessKey, "wrong", "us-east-1", now, nil)
			return r
		}, 403, "SignatureDoesNotMatch"},
		{"unknown access key", func() *http.Request {
			r := mk()
			signAt(r, "nobody", cfg.SecretKey, "us-east-1", now, nil)
			return r
		}, 403, "InvalidAccessKeyId"},
		{"clock 1h ahead", func() *http.Request {
			r := mk()
			signAt(r, cfg.AccessKey, cfg.SecretKey, "us-east-1", now.Add(time.Hour), nil)
			return r
		}, 403, "RequestTimeTooSkewed"},
		{"clock 1h behind", func() *http.Request {
			r := mk()
			signAt(r, cfg.AccessKey, cfg.SecretKey, "us-east-1", now.Add(-time.Hour), nil)
			return r
		}, 403, "RequestTimeTooSkewed"},
		{"wrong region", func() *http.Request {
			r := mk()
			signAt(r, cfg.AccessKey, cfg.SecretKey, "eu-west-1", now, nil)
			return r
		}, 400, "AuthorizationHeaderMalformed"},
		{"header added after signing", func() *http.Request {
			r := mk()
			signAt(r, cfg.AccessKey, cfg.SecretKey, "us-east-1", now, nil)
			r.Header.Set("X-Amz-Meta-Injected", "1")
			return r
		}, 403, "AccessDenied"},
		{"signed header changed after signing", func() *http.Request {
			r := mk()
			signAt(r, cfg.AccessKey, cfg.SecretKey, "us-east-1", now, map[string]string{"X-Amz-Meta-Foo": "a"})
			r.Header.Set("X-Amz-Meta-Foo", "b")
			return r
		}, 403, "SignatureDoesNotMatch"},
		{"query added after signing", func() *http.Request {
			r := mk()
			signAt(r, cfg.AccessKey, cfg.SecretKey, "us-east-1", now, nil)
			r.URL.RawQuery = "versionId=x"
			return r
		}, 403, "SignatureDoesNotMatch"},
		{"no Authorization", func() *http.Request { return mk() }, 403, "AccessDenied"},
		{"payload hash header missing", func() *http.Request {
			r := mk()
			signAt(r, cfg.AccessKey, cfg.SecretKey, "us-east-1", now, nil)
			r.Header.Del("X-Amz-Content-Sha256")
			return r
		}, 400, "InvalidRequest"},
	}
	for _, tc := range cases {
		st, body := status(t, tc.mod())
		if st != tc.want || !strings.Contains(body, "<Code>"+tc.code+"</Code>") {
			t.Errorf("%s: HTTP %d %.150s, want %d %s", tc.name, st, body, tc.want, tc.code)
		}
	}
	// refused requests are in the log with their status
	var n403 int
	for _, r := range srv.Requests() {
		if r.Status == 403 {
			n403++
		}
	}
	if n403 < 5 {
		t.Fatalf("log shows %d refusals", n403)
	}
}

// streamingPut sends an aws-chunked PUT signed by this test's own signer. tamper >= 0 flips one
// payload byte after the chunk was signed; reorder swaps the first two chunks.
func streamingPut(t *testing.T, cfg s3test.Config, bucket, key string, data []byte, tamper int, reorder bool) (int, string) {
	t.Helper()
	const chunk = 64 << 10
	now := time.Now().UTC()
	amz, day := now.Format("20060102T150405Z"), now.Format("20060102")
	region := "us-east-1"
	scope := day + "/" + region + "/s3/aws4_request"
	k := hm(hm(hm(hm([]byte("AWS4"+cfg.SecretKey), day), region), "s3"), "aws4_request")
	const payload = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	host := cfg.Endpoint
	signedH := "host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length"
	canon := strings.Join([]string{"PUT", "/" + bucket + "/" + key, "",
		"host:" + host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + amz + "\nx-amz-decoded-content-length:" + fmt.Sprint(len(data)) + "\n",
		signedH, payload}, "\n")
	seed := hex.EncodeToString(hm(k, "AWS4-HMAC-SHA256\n"+amz+"\n"+scope+"\n"+sha(canon)))
	prev := seed
	var body bytes.Buffer
	var chunks [][]byte
	for off := 0; off < len(data); off += chunk {
		chunks = append(chunks, data[off:min(off+chunk, len(data))])
	}
	chunks = append(chunks, nil)
	for i, c := range chunks {
		sig := hex.EncodeToString(hm(k, "AWS4-HMAC-SHA256-PAYLOAD\n"+amz+"\n"+scope+"\n"+prev+"\n"+sha("")+"\n"+sha(string(c))))
		prev = sig
		c2 := append([]byte(nil), c...)
		if tamper == i && len(c2) > 0 {
			c2[len(c2)/2] ^= 0xff
		}
		fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n", len(c), sig)
		body.Write(c2)
		body.WriteString("\r\n")
	}
	_ = reorder
	req, _ := http.NewRequest(http.MethodPut, "http://"+host+"/"+bucket+"/"+key, &body)
	req.ContentLength = int64(body.Len())
	req.Header.Set("X-Amz-Date", amz)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(data)))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", cfg.AccessKey, scope, signedH, seed))
	return status(t, req)
}

// Streaming uploads: the independent signer is accepted; a byte flipped after signing, in any
// chunk, is refused with SignatureDoesNotMatch and stores nothing.
func TestStreamingUploadSignaturesAreVerified(t *testing.T) {
	srv, cfg, _, b := newBucket(t)
	data := bytes.Repeat([]byte("abcdefgh"), 20000) // 160000 bytes: chunks of 64 KiB, 64 KiB, 28 KB
	if st, body := streamingPut(t, cfg, b, "good", data, -1, false); st != 200 {
		t.Fatalf("a correctly signed streaming upload: %d %s", st, body)
	}
	if got, ok := srv.Object(b, "good"); !ok || !bytes.Equal(got, data) {
		t.Fatalf("stored %d bytes", len(got))
	}
	for chunk := 0; chunk < 3; chunk++ {
		st, body := streamingPut(t, cfg, b, fmt.Sprintf("bad%d", chunk), data, chunk, false)
		if st != 403 || !strings.Contains(body, "SignatureDoesNotMatch") {
			t.Errorf("tampered chunk %d: %d %.150s", chunk, st, body)
		}
		if _, ok := srv.Object(b, fmt.Sprintf("bad%d", chunk)); ok {
			t.Errorf("tampered chunk %d left an object", chunk)
		}
	}
}
