package s3fake

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

var (
	bucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	ipRE         = regexp.MustCompile(`^(\d+\.){3}\d+$`)
)

const maxObjectSize = 5 << 30

func writeXML(w http.ResponseWriter, status int, v any) {
	b, _ := xml.Marshal(v)
	out := append([]byte(xml.Header), b...)
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", itoa(len(out)))
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func quoteETag(e string) string { return `"` + e + `"` }

// dispatch authenticates and routes one request. w may be a mute writer (DropResponse).
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, rc *reqCtx, dropAfter int64) {
	w.Header().Set("Server", serverName)
	w.Header().Set("x-amz-request-id", rc.id)
	w.Header().Set("Accept-Ranges", "bytes")
	a, aerr := s.verifySigV4(r, rc.op == "GetBucketLocation")
	if aerr != nil {
		writeError(w, r, rc, aerr)
		return
	}
	if strings.HasPrefix(rc.op, "Unsupported:") || rc.op == "ListObjectsV1" || strings.HasSuffix(rc.op, "Part") || strings.HasSuffix(rc.op, "MultipartUpload") ||
		rc.op == "ListMultipartUploads" || rc.op == "ListParts" {
		writeError(w, r, rc, apiErrf("NotImplemented", rc.op+" is not implemented by the test server (the code under test must not use it)"))
		return
	}
	if r.Header.Get("If-Modified-Since") != "" || r.Header.Get("If-Unmodified-Since") != "" {
		writeError(w, r, rc, apiErrf("NotImplemented", "If-(Un)Modified-Since is not implemented by the test server"))
		return
	}
	if rc.key != "" && len(rc.key) > 1024 {
		writeError(w, r, rc, apiErr("KeyTooLongError"))
		return
	}
	var err *apiError
	switch rc.op {
	case "ListBuckets":
		err = s.listBuckets(w, r, rc)
	case "CreateBucket":
		err = s.createBucket(w, r, rc, a)
	case "DeleteBucket":
		err = s.deleteBucket(w, r, rc)
	case "HeadBucket", "GetBucketLocation", "GetBucketVersioning", "PutBucketVersioning", "GetBucketLifecycle", "PutBucketLifecycle",
		"DeleteBucketLifecycle", "GetObjectLockConfiguration", "PutObjectLockConfiguration", "ListObjectsV2", "ListObjectVersions", "DeleteObjects":
		err = s.bucketOp(w, r, rc, a)
	case "PutObject":
		err = s.putObject(w, r, rc, a, dropAfter)
	case "CopyObject":
		err = s.copyObject(w, r, rc, a)
	case "GetObject", "HeadObject":
		err = s.getObject(w, r, rc)
	case "DeleteObject":
		err = s.deleteObject(w, r, rc)
	default:
		err = apiErr("MethodNotAllowed")
	}
	if err != nil {
		if err.Code == "dropped" {
			abortConn(w)
			return
		}
		writeError(w, r, rc, err)
	}
}

func (s *Server) lookup(rc *reqCtx) (*bucket, *apiError) {
	b := s.st.bucket(rc.bucket)
	if b == nil {
		return nil, apiErr("NoSuchBucket")
	}
	return b, nil
}

// readSmall reads a small request body (XML), verifying the payload hash and Content-MD5.
func readSmall(r *http.Request, a *authInfo, s *Server) ([]byte, *apiError) {
	br, aerr := s.openBody(r, a)
	if aerr != nil {
		return nil, aerr
	}
	data, err := io.ReadAll(io.LimitReader(br.r, 4<<20+1))
	if err != nil {
		return nil, chunkAPIError(err)
	}
	if len(data) > 4<<20 {
		return nil, apiErr("EntityTooLarge")
	}
	if _, aerr := br.finish(); aerr != nil {
		return nil, aerr
	}
	if md := r.Header.Get("Content-MD5"); md != "" {
		sum := md5.Sum(data)
		if base64.StdEncoding.EncodeToString(sum[:]) != md {
			return nil, apiErr("BadDigest")
		}
	}
	return data, nil
}

// ---- service and bucket level ----------------------------------------------------------------

type xmlListBuckets struct {
	XMLName xml.Name    `xml:"ListAllMyBucketsResult"`
	Xmlns   string      `xml:"xmlns,attr"`
	Owner   xmlOwner    `xml:"Owner"`
	Buckets []xmlBucket `xml:"Buckets>Bucket"`
}
type xmlOwner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}
type xmlBucket struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request, rc *reqCtx) *apiError {
	out := xmlListBuckets{Xmlns: xmlns, Owner: xmlOwner{ID: "s3fake", DisplayName: "s3fake"}}
	for _, n := range s.st.names() {
		if b := s.st.bucket(n); b != nil {
			out.Buckets = append(out.Buckets, xmlBucket{n, isoTime(b.conf.Created)})
		}
	}
	writeXML(w, 200, out)
	return nil
}

func validBucketName(n string) bool {
	return bucketNameRE.MatchString(n) && !ipRE.MatchString(n) && !strings.Contains(n, "..")
}

func (s *Server) createBucket(w http.ResponseWriter, r *http.Request, rc *reqCtx, a *authInfo) *apiError {
	if !validBucketName(rc.bucket) {
		return apiErr("InvalidBucketName")
	}
	body, aerr := readSmall(r, a, s)
	if aerr != nil {
		return aerr
	}
	loc := "us-east-1"
	if len(strings.TrimSpace(string(body))) > 0 {
		var cfg struct {
			XMLName    xml.Name `xml:"CreateBucketConfiguration"`
			Constraint string   `xml:"LocationConstraint"`
		}
		if err := xml.Unmarshal(body, &cfg); err != nil {
			return apiErr("MalformedXML")
		}
		if cfg.Constraint != "" {
			loc = cfg.Constraint
		}
	}
	if loc != s.opt.Region {
		return apiErr("IllegalLocationConstraintException")
	}
	lock := strings.EqualFold(r.Header.Get("x-amz-bucket-object-lock-enabled"), "true")
	_, created, err := s.st.create(rc.bucket, lock)
	if err != nil {
		return apiErrf("InternalError", err.Error())
	}
	if !created {
		return apiErr("BucketAlreadyOwnedByYou")
	}
	w.Header().Set("Location", "/"+rc.bucket)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(200)
	return nil
}

func (s *Server) deleteBucket(w http.ResponseWriter, r *http.Request, rc *reqCtx) *apiError {
	b, aerr := s.lookup(rc)
	if aerr != nil {
		return aerr
	}
	b.mu.RLock()
	empty := len(b.objs) == 0
	b.mu.RUnlock()
	if !empty {
		return apiErr("BucketNotEmpty")
	}
	if err := s.st.remove(rc.bucket); err != nil {
		return apiErrf("InternalError", err.Error())
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) bucketOp(w http.ResponseWriter, r *http.Request, rc *reqCtx, a *authInfo) *apiError {
	b, aerr := s.lookup(rc)
	if aerr != nil {
		return aerr
	}
	w.Header().Set("x-amz-bucket-region", s.opt.Region)
	switch rc.op {
	case "HeadBucket":
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(200)
	case "GetBucketLocation":
		loc := s.opt.Region
		if loc == "us-east-1" {
			loc = ""
		}
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"LocationConstraint"`
			Xmlns   string   `xml:"xmlns,attr"`
			Value   string   `xml:",chardata"`
		}{Xmlns: xmlns, Value: loc})
	case "GetBucketVersioning":
		b.mu.RLock()
		st := b.conf.Versioning
		b.mu.RUnlock()
		writeXML(w, 200, struct {
			XMLName xml.Name `xml:"VersioningConfiguration"`
			Xmlns   string   `xml:"xmlns,attr"`
			Status  string   `xml:"Status,omitempty"`
		}{Xmlns: xmlns, Status: st})
	case "PutBucketVersioning":
		body, aerr := readSmall(r, a, s)
		if aerr != nil {
			return aerr
		}
		var cfg struct {
			XMLName xml.Name `xml:"VersioningConfiguration"`
			Status  string   `xml:"Status"`
		}
		if err := xml.Unmarshal(body, &cfg); err != nil || cfg.Status != "Enabled" && cfg.Status != "Suspended" {
			return apiErr("MalformedXML")
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.conf.Lock && cfg.Status != "Enabled" {
			return apiErrf("InvalidBucketState", "versioning cannot be suspended on a bucket with object lock")
		}
		b.conf.Versioning = cfg.Status
		if err := b.saveConf(); err != nil {
			return apiErrf("InternalError", err.Error())
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(200)
	case "GetBucketLifecycle":
		b.mu.RLock()
		lc := b.conf.Lifecycle
		b.mu.RUnlock()
		if lc == nil {
			return apiErr("NoSuchLifecycleConfiguration")
		}
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Content-Length", itoa(len(lc)))
		w.WriteHeader(200)
		_, _ = w.Write(lc)
	case "PutBucketLifecycle":
		body, aerr := readSmall(r, a, s)
		if aerr != nil {
			return aerr
		}
		var cfg struct {
			XMLName xml.Name `xml:"LifecycleConfiguration"`
			Rules   []struct {
				Status string `xml:"Status"`
			} `xml:"Rule"`
		}
		if err := xml.Unmarshal(body, &cfg); err != nil || len(cfg.Rules) == 0 {
			return apiErr("MalformedXML")
		}
		for _, ru := range cfg.Rules {
			if ru.Status != "Enabled" && ru.Status != "Disabled" {
				return apiErr("MalformedXML")
			}
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.conf.Lifecycle = append([]byte(nil), body...)
		if err := b.saveConf(); err != nil {
			return apiErrf("InternalError", err.Error())
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(200)
	case "DeleteBucketLifecycle":
		b.mu.Lock()
		defer b.mu.Unlock()
		b.conf.Lifecycle = nil
		_ = b.saveConf()
		w.WriteHeader(204)
	case "GetObjectLockConfiguration":
		b.mu.RLock()
		lock, lx := b.conf.Lock, b.conf.LockXML
		b.mu.RUnlock()
		if !lock {
			return apiErr("ObjectLockConfigurationNotFoundError")
		}
		if lx == nil {
			lx = []byte(`<ObjectLockConfiguration xmlns="` + xmlns + `"><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Content-Length", itoa(len(lx)))
		w.WriteHeader(200)
		_, _ = w.Write(lx)
	case "PutObjectLockConfiguration":
		body, aerr := readSmall(r, a, s)
		if aerr != nil {
			return aerr
		}
		var cfg struct {
			XMLName xml.Name `xml:"ObjectLockConfiguration"`
			Enabled string   `xml:"ObjectLockEnabled"`
			Mode    string   `xml:"Rule>DefaultRetention>Mode"`
			Days    int      `xml:"Rule>DefaultRetention>Days"`
			Years   int      `xml:"Rule>DefaultRetention>Years"`
		}
		if err := xml.Unmarshal(body, &cfg); err != nil || cfg.Enabled != "Enabled" {
			return apiErr("MalformedXML")
		}
		if cfg.Mode != "" && (cfg.Mode != "GOVERNANCE" && cfg.Mode != "COMPLIANCE" || (cfg.Days > 0) == (cfg.Years > 0)) {
			return apiErr("MalformedXML")
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.conf.Lock {
			return apiErrf("InvalidBucketState", "Object Lock configuration cannot be enabled on existing buckets")
		}
		b.conf.LockXML = append([]byte(nil), body...)
		if err := b.saveConf(); err != nil {
			return apiErrf("InternalError", err.Error())
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(200)
	case "ListObjectsV2":
		return s.listV2(w, r, b, rc)
	case "ListObjectVersions":
		return s.listVersions(w, r, b, rc)
	case "DeleteObjects":
		return s.deleteObjects(w, r, b, rc, a)
	}
	return nil
}

// ---- listing ---------------------------------------------------------------------------------

type xmlContents struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}
type xmlCP struct {
	Prefix string `xml:"Prefix"`
}
type xmlListV2 struct {
	XMLName               xml.Name      `xml:"ListBucketResult"`
	Xmlns                 string        `xml:"xmlns,attr"`
	Name                  string        `xml:"Name"`
	Prefix                string        `xml:"Prefix"`
	Delimiter             string        `xml:"Delimiter,omitempty"`
	MaxKeys               int           `xml:"MaxKeys"`
	EncodingType          string        `xml:"EncodingType,omitempty"`
	KeyCount              int           `xml:"KeyCount"`
	IsTruncated           bool          `xml:"IsTruncated"`
	ContinuationToken     string        `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
	StartAfter            string        `xml:"StartAfter,omitempty"`
	Contents              []xmlContents `xml:"Contents"`
	CommonPrefixes        []xmlCP       `xml:"CommonPrefixes"`
}

func parseMaxKeys(q url.Values) (int, *apiError) {
	v := q.Get("max-keys")
	if v == "" {
		return 1000, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, apiErrf("InvalidArgument", "Provided max-keys not an integer or within integer range")
	}
	return min(n, 1000), nil
}

func encoder(q url.Values) (func(string) string, *apiError) {
	switch q.Get("encoding-type") {
	case "":
		return func(s string) string { return s }, nil
	case "url":
		return url.QueryEscape, nil
	}
	return nil, apiErrf("InvalidArgument", "Invalid Encoding Method specified in Request")
}

// skipBlock is the index of the first key at or after i that does not start with cp.
func skipBlock(keys []string, from int, cp string) int {
	return from + sort.Search(len(keys)-from, func(x int) bool { k := keys[from+x]; return k > cp && !strings.HasPrefix(k, cp) })
}

func (s *Server) listV2(w http.ResponseWriter, r *http.Request, b *bucket, rc *reqCtx) *apiError {
	q := r.URL.Query()
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	if s.quirk(QuirkIgnorePrefix) {
		prefix = ""
	}
	if s.quirk(QuirkIgnoreDelimiter) {
		delim = ""
	}
	maxKeys, aerr := parseMaxKeys(q)
	if aerr != nil {
		return aerr
	}
	enc, aerr := encoder(q)
	if aerr != nil {
		return aerr
	}
	startAfter, token := q.Get("start-after"), q.Get("continuation-token")
	var markerKind byte
	var marker string
	switch {
	case token != "":
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(raw) < 1 || raw[0] != 'k' && raw[0] != 'p' {
			return apiErrf("InvalidArgument", "The continuation token provided is incorrect")
		}
		markerKind, marker = raw[0], string(raw[1:])
	case startAfter != "":
		markerKind, marker = 'k', startAfter
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	keys := b.keys
	i := sort.SearchStrings(keys, prefix)
	if markerKind == 'k' {
		if j := sort.SearchStrings(keys, marker+"\x00"); j > i {
			i = j
		}
	} else if markerKind == 'p' {
		if j := skipBlock(keys, 0, marker); j > i {
			i = j
		}
	}
	out := xmlListV2{Xmlns: xmlns, Name: rc.bucket, Prefix: enc(prefix), MaxKeys: maxKeys, EncodingType: q.Get("encoding-type"),
		ContinuationToken: token, StartAfter: enc(startAfter)}
	if delim != "" {
		out.Delimiter = enc(delim)
	}
	count, truncated := 0, false
	var last string
	for i < len(keys) && strings.HasPrefix(keys[i], prefix) {
		k := keys[i]
		v := b.current(k)
		if v == nil {
			i++
			continue
		}
		if count >= maxKeys {
			truncated = maxKeys > 0 // max-keys=0 asks for nothing: never "truncated"
			break
		}
		if delim != "" {
			rest := k[len(prefix):]
			if x := strings.Index(rest, delim); x >= 0 {
				cp := prefix + rest[:x+len(delim)]
				out.CommonPrefixes = append(out.CommonPrefixes, xmlCP{enc(cp)})
				last = "p" + cp
				count++
				i = skipBlock(keys, i, cp)
				continue
			}
		}
		out.Contents = append(out.Contents, xmlContents{Key: enc(k), LastModified: isoTime(v.Mod), ETag: quoteETag(v.ETag), Size: v.Size, StorageClass: "STANDARD"})
		last = "k" + k
		count++
		i++
	}
	out.KeyCount = count
	if truncated && !s.quirk(QuirkNoContinuation) {
		out.IsTruncated = true
		out.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(last))
	}
	if s.quirk(QuirkUnsortedListing) {
		for x, y := 0, len(out.Contents)-1; x < y; x, y = x+1, y-1 {
			out.Contents[x], out.Contents[y] = out.Contents[y], out.Contents[x]
		}
	}
	writeXML(w, 200, out)
	return nil
}

type xmlVersionEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag,omitempty"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass,omitempty"`
}
type xmlListVersions struct {
	XMLName             xml.Name          `xml:"ListVersionsResult"`
	Xmlns               string            `xml:"xmlns,attr"`
	Name                string            `xml:"Name"`
	Prefix              string            `xml:"Prefix"`
	KeyMarker           string            `xml:"KeyMarker"`
	VersionIDMarker     string            `xml:"VersionIdMarker"`
	NextKeyMarker       string            `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string            `xml:"NextVersionIdMarker,omitempty"`
	Delimiter           string            `xml:"Delimiter,omitempty"`
	MaxKeys             int               `xml:"MaxKeys"`
	EncodingType        string            `xml:"EncodingType,omitempty"`
	IsTruncated         bool              `xml:"IsTruncated"`
	Versions            []xmlVersionEntry `xml:"Version"`
	DeleteMarkers       []xmlVersionEntry `xml:"DeleteMarker"`
	CommonPrefixes      []xmlCP           `xml:"CommonPrefixes"`
}

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request, b *bucket, rc *reqCtx) *apiError {
	q := r.URL.Query()
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	maxKeys, aerr := parseMaxKeys(q)
	if aerr != nil {
		return aerr
	}
	enc, aerr := encoder(q)
	if aerr != nil {
		return aerr
	}
	km, vm := q.Get("key-marker"), q.Get("version-id-marker")
	b.mu.RLock()
	defer b.mu.RUnlock()
	keys := b.keys
	i := sort.SearchStrings(keys, prefix)
	skipVersionsUpTo := "" // inside key km: resume after this version id
	if km != "" {
		switch {
		case delim != "" && strings.HasSuffix(km, delim) && vm == "":
			if j := skipBlock(keys, 0, km); j > i {
				i = j
			}
		case vm != "":
			if j := sort.SearchStrings(keys, km); j > i {
				i = j
			}
			skipVersionsUpTo = vm
		default:
			if j := sort.SearchStrings(keys, km+"\x00"); j > i {
				i = j
			}
		}
	}
	out := xmlListVersions{Xmlns: xmlns, Name: rc.bucket, Prefix: enc(prefix), KeyMarker: enc(km), VersionIDMarker: vm, MaxKeys: maxKeys, EncodingType: q.Get("encoding-type")}
	if delim != "" {
		out.Delimiter = enc(delim)
	}
	count := 0
	var lastKey, lastVer string
	truncated := false
loop:
	for i < len(keys) && strings.HasPrefix(keys[i], prefix) {
		k := keys[i]
		o := b.objs[k]
		if delim != "" {
			rest := k[len(prefix):]
			if x := strings.Index(rest, delim); x >= 0 {
				if count >= maxKeys {
					truncated = true
					break
				}
				cp := prefix + rest[:x+len(delim)]
				out.CommonPrefixes = append(out.CommonPrefixes, xmlCP{enc(cp)})
				lastKey, lastVer = cp, ""
				count++
				i = skipBlock(keys, i, cp)
				continue
			}
		}
		skipping := skipVersionsUpTo != "" && k == km
		for n, v := range o.versions {
			if skipping {
				if v.ID == skipVersionsUpTo {
					skipping = false
				}
				continue
			}
			if count >= maxKeys {
				truncated = true
				break loop
			}
			e := xmlVersionEntry{Key: enc(k), VersionID: v.ID, IsLatest: n == 0, LastModified: isoTime(v.Mod)}
			if v.DeleteMarker {
				out.DeleteMarkers = append(out.DeleteMarkers, e)
			} else {
				e.ETag, e.Size, e.StorageClass = quoteETag(v.ETag), v.Size, "STANDARD"
				out.Versions = append(out.Versions, e)
			}
			lastKey, lastVer = k, v.ID
			count++
		}
		skipVersionsUpTo = ""
		i++
	}
	if truncated {
		out.IsTruncated = true
		out.NextKeyMarker, out.NextVersionIDMarker = enc(lastKey), lastVer
	}
	writeXML(w, 200, out)
	return nil
}

// ---- multi-object delete ---------------------------------------------------------------------

type xmlDeleteReq struct {
	XMLName xml.Name `xml:"Delete"`
	Quiet   bool     `xml:"Quiet"`
	Objects []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
	} `xml:"Object"`
}
type xmlDeleted struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}
type xmlDelErr struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}
type xmlDeleteResult struct {
	XMLName xml.Name     `xml:"DeleteResult"`
	Xmlns   string       `xml:"xmlns,attr"`
	Deleted []xmlDeleted `xml:"Deleted"`
	Errors  []xmlDelErr  `xml:"Error"`
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, b *bucket, rc *reqCtx, a *authInfo) *apiError {
	if r.Header.Get("Content-MD5") == "" && !hasChecksumHeader(r.Header) {
		return apiErrf("InvalidRequest", "Missing required header for this request: Content-MD5 or x-amz-checksum-*")
	}
	body, aerr := readSmall(r, a, s)
	if aerr != nil {
		return aerr
	}
	var req xmlDeleteReq
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Objects) == 0 || len(req.Objects) > 1000 {
		return apiErr("MalformedXML")
	}
	out := xmlDeleteResult{Xmlns: xmlns}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, o := range req.Objects {
		if o.Key == "" || len(o.Key) > 1024 {
			out.Errors = append(out.Errors, xmlDelErr{Key: o.Key, Code: "KeyTooLongError", Message: "invalid key"})
			continue
		}
		id, marker, err := b.remove(o.Key, o.VersionID)
		if err != nil {
			out.Errors = append(out.Errors, xmlDelErr{Key: o.Key, VersionID: o.VersionID, Code: "InternalError", Message: err.Error()})
			continue
		}
		if req.Quiet {
			continue
		}
		d := xmlDeleted{Key: o.Key, VersionID: o.VersionID}
		if marker {
			d.DeleteMarker = true
			if o.VersionID == "" {
				d.DeleteMarkerVersionID = id
			} else {
				d.DeleteMarkerVersionID = o.VersionID
			}
		}
		out.Deleted = append(out.Deleted, d)
	}
	writeXML(w, 200, out)
	return nil
}

func hasChecksumHeader(h http.Header) bool {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-checksum-") {
			return true
		}
	}
	return false
}

// ---- objects ---------------------------------------------------------------------------------

// storedHeaders extracts the headers an object keeps.
func storedHeaders(h http.Header, copyFrom map[string]string) map[string]string {
	m := map[string]string{}
	for _, n := range []string{"Content-Type", "Content-Disposition", "Content-Language", "Cache-Control", "Expires"} {
		if v := h.Get(n); v != "" {
			m[strings.ToLower(n)] = v
		}
	}
	if ce := h.Get("Content-Encoding"); ce != "" {
		var keep []string
		for _, p := range strings.Split(ce, ",") {
			if p = strings.TrimSpace(p); p != "" && !strings.EqualFold(p, "aws-chunked") {
				keep = append(keep, p)
			}
		}
		if len(keep) > 0 {
			m["content-encoding"] = strings.Join(keep, ", ")
		}
	}
	for k, v := range h {
		if l := strings.ToLower(k); strings.HasPrefix(l, "x-amz-meta-") && len(v) > 0 {
			m[l] = strings.Join(v, ",")
		}
	}
	if m["content-type"] == "" {
		m["content-type"] = "binary/octet-stream"
	}
	return m
}

// checkIntegrity verifies Content-MD5 and any x-amz-checksum-* header or trailer against what was received.
type multiHash struct {
	md5    hash.Hash
	sha1   hash.Hash
	sha256 hash.Hash
	crc32  hash.Hash32
	crc32c hash.Hash32
}

func newMultiHash() *multiHash {
	return &multiHash{md5.New(), sha1.New(), sha256.New(), crc32.NewIEEE(), crc32.New(crc32.MakeTable(crc32.Castagnoli))}
}

func (m *multiHash) Write(p []byte) (int, error) {
	m.md5.Write(p)
	m.sha1.Write(p)
	m.sha256.Write(p)
	m.crc32.Write(p)
	m.crc32c.Write(p)
	return len(p), nil
}

func (m *multiHash) check(h http.Header, trailer http.Header) *apiError {
	if md := h.Get("Content-MD5"); md != "" {
		if base64.StdEncoding.EncodeToString(m.md5.Sum(nil)) != md {
			return apiErr("BadDigest")
		}
	}
	for _, src := range []http.Header{h, trailer} {
		for k, vs := range src {
			l := strings.ToLower(k)
			if !strings.HasPrefix(l, "x-amz-checksum-") || len(vs) == 0 {
				continue
			}
			var sum []byte
			switch strings.TrimPrefix(l, "x-amz-checksum-") {
			case "crc32":
				sum = m.crc32.Sum(nil)
			case "crc32c":
				sum = m.crc32c.Sum(nil)
			case "sha1":
				sum = m.sha1.Sum(nil)
			case "sha256":
				sum = m.sha256.Sum(nil)
			default:
				return apiErrf("NotImplemented", "checksum algorithm "+l+" is not implemented by the test server")
			}
			if base64.StdEncoding.EncodeToString(sum) != vs[0] {
				return apiErr("BadDigest")
			}
		}
	}
	return nil
}

func etagMatches(header, etag string) bool {
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimSpace(p)
		if p == "*" || strings.Trim(strings.TrimPrefix(p, "W/"), `"`) == etag {
			return true
		}
	}
	return false
}

// receive reads the request body into a sink, verifying signatures, sizes and checksums.
func (s *Server) receive(r *http.Request, rc *reqCtx, a *authInfo, dropAfter int64) (*sink, *apiError) {
	br, aerr := s.openBody(r, a)
	if aerr != nil {
		return nil, aerr
	}
	if br.declared < 0 && r.Header.Get("X-Amz-Decoded-Content-Length") == "" && r.ContentLength < 0 {
		return nil, apiErr("MissingContentLength")
	}
	if br.declared > maxObjectSize {
		return nil, apiErr("EntityTooLarge")
	}
	k, err := s.newSink()
	if err != nil {
		return nil, apiErrf("InternalError", err.Error())
	}
	mh := newMultiHash()
	var src io.Reader = br.r
	if dropAfter > 0 {
		src = &dropReader{r: src, left: dropAfter}
	}
	_, cerr := io.Copy(io.MultiWriter(k, mh), io.LimitReader(src, maxObjectSize+1))
	rc.rec.Bytes = k.n
	if cerr != nil {
		if s.quirk(QuirkTornPutVisible) {
			// mutation: the torn upload becomes an object
			if bk := s.st.bucket(rc.bucket); bk != nil {
				bk.mu.Lock()
				v := &version{Key: rc.key, Size: k.n, ETag: k.etag(), Mod: time.Now().UTC().Truncate(time.Millisecond), Hdr: storedHeaders(r.Header, nil)}
				_ = bk.put(v, k)
				bk.mu.Unlock()
			}
		}
		k.discard()
		if errors.Is(cerr, errDropped) {
			return nil, &apiError{Status: 0, Code: "dropped", Message: errDropped.Error()}
		}
		var ce *chunkError
		if errors.As(cerr, &ce) {
			return nil, apiErrf(ce.code, ce.msg)
		}
		// the client went away or the framing broke: nothing was stored
		return nil, apiErrf("IncompleteBody", cerr.Error())
	}
	if k.n > maxObjectSize {
		k.discard()
		return nil, apiErr("EntityTooLarge")
	}
	if br.declared >= 0 && k.n != br.declared {
		k.discard()
		return nil, apiErr("IncompleteBody")
	}
	trailer, aerr := br.finish()
	if aerr != nil {
		k.discard()
		return nil, aerr
	}
	if aerr := mh.check(r.Header, trailer); aerr != nil {
		k.discard()
		return nil, aerr
	}
	return k, nil
}

type dropReader struct {
	r    io.Reader
	left int64
}

func (d *dropReader) Read(p []byte) (int, error) {
	if d.left <= 0 {
		return 0, errDropped
	}
	if int64(len(p)) > d.left {
		p = p[:d.left]
	}
	n, err := d.r.Read(p)
	d.left -= int64(n)
	return n, err
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, rc *reqCtx, a *authInfo, dropAfter int64) *apiError {
	if r.Header.Get("If-Match") != "" {
		return apiErrf("NotImplemented", "If-Match on PutObject is not implemented by the test server")
	}
	inm := r.Header.Get("If-None-Match")
	if inm != "" && inm != "*" {
		return apiErrf("NotImplemented", "only If-None-Match: * is implemented on PutObject")
	}
	for k := range r.Header {
		if l := strings.ToLower(k); l == "x-amz-tagging" || strings.HasPrefix(l, "x-amz-object-lock-") || strings.HasPrefix(l, "x-amz-server-side-encryption-customer") {
			return apiErrf("NotImplemented", l+" is not implemented by the test server")
		}
	}
	b, aerr := s.lookup(rc)
	if aerr != nil {
		return aerr
	}
	k, aerr := s.receive(r, rc, a, dropAfter)
	if aerr != nil {
		return aerr
	}
	defer k.discard()
	v := &version{Key: rc.key, Size: k.n, ETag: k.etag(), Mod: time.Now().UTC().Truncate(time.Millisecond), Hdr: storedHeaders(r.Header, nil)}
	if s.quirk(QuirkWrongETag) {
		v.ETag = strings.Repeat("0", 32)
	}
	cond := inm == "*" && !s.quirk(QuirkIgnoreIfNoneMatch)
	if cond && s.quirk(QuirkNonAtomicPut) {
		b.mu.RLock()
		exists := b.current(rc.key) != nil
		b.mu.RUnlock()
		if exists {
			return apiErr("PreconditionFailed")
		}
		time.Sleep(3 * time.Millisecond)
		cond = false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.st.bucket(rc.bucket) != b {
		return apiErr("NoSuchBucket")
	}
	if cond && b.current(rc.key) != nil {
		return apiErr("PreconditionFailed")
	}
	if err := b.put(v, k); err != nil {
		return apiErrf("InternalError", err.Error())
	}
	w.Header().Set("ETag", quoteETag(v.ETag))
	if b.conf.Versioning == "Enabled" {
		w.Header().Set("x-amz-version-id", v.ID)
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(200)
	return nil
}
