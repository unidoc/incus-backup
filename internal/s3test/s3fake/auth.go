package s3fake

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	algoV4         = "AWS4-HMAC-SHA256"
	unsignedBody   = "UNSIGNED-PAYLOAD"
	streamSigned   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamSignedTr = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	streamUnsigTr  = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	emptySHA256    = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	maxSkew        = 15 * time.Minute
	maxChunk       = 16 << 20
)

// authInfo is what a verified request carries on: who signed it and the seed for chunk signatures.
type authInfo struct {
	access, secret string
	seed           string // the request signature (the first "previous signature" of a chunk chain)
	date           time.Time
	scope          string // date/region/s3/aws4_request
	region         string
	payloadHash    string // the x-amz-content-sha256 header
	noVerify       bool   // mutation: signatures are not checked
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func signingKey(secret, day, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// uriEncode is the AWS canonical encoding: everything but A-Za-z0-9-_.~ is %XX; '/' stays when keepSlash.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var kvs []kv
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		uk, err1 := url.QueryUnescape(k)
		uv, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			uk, uv = k, v
		}
		kvs = append(kvs, kv{uriEncode(uk, false), uriEncode(uv, false)})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].k != kvs[j].k {
			return kvs[i].k < kvs[j].k
		}
		return kvs[i].v < kvs[j].v
	})
	out := make([]string, len(kvs))
	for i, e := range kvs {
		out[i] = e.k + "=" + e.v
	}
	return strings.Join(out, "&")
}

func trimSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

// verifySigV4 checks the Authorization header of r against the server's users. Everything is
// recomputed from the AWS specification (decoded path re-encoded, query re-sorted, signed headers
// as named), never copied from a client.
func (s *Server) verifySigV4(r *http.Request, location bool) (*authInfo, *apiError) {
	if s.quirk(QuirkAcceptAnySignature) {
		// mutation: treat everybody as the root user
		return &authInfo{access: s.opt.Access, secret: s.opt.Secret, payloadHash: r.Header.Get("x-amz-content-sha256"), date: time.Now(), noVerify: true}, nil
	}
	if r.URL.Query().Has("X-Amz-Algorithm") || r.URL.Query().Has("X-Amz-Signature") {
		return nil, apiErrf("NotImplemented", "presigned URLs are not implemented by the test server")
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, apiErrf("AccessDenied", "Access Denied.")
	}
	rest, ok := strings.CutPrefix(auth, algoV4+" ")
	if !ok {
		return nil, apiErrf("AuthorizationHeaderMalformed", "only AWS4-HMAC-SHA256 is supported")
	}
	fields := map[string]string{}
	for _, p := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok {
			return nil, apiErr("AuthorizationHeaderMalformed")
		}
		fields[k] = v
	}
	cred, signedList, sig := fields["Credential"], fields["SignedHeaders"], fields["Signature"]
	if cred == "" || signedList == "" || sig == "" {
		return nil, apiErr("AuthorizationHeaderMalformed")
	}
	cp := strings.Split(cred, "/")
	if len(cp) != 5 || cp[4] != "aws4_request" || cp[3] != "s3" {
		return nil, apiErrf("AuthorizationHeaderMalformed", "the credential scope is malformed")
	}
	access, day, region := cp[0], cp[1], cp[2]
	secret, known := s.secretFor(access)
	if !known {
		return nil, apiErr("InvalidAccessKeyId")
	}
	if !location && region != s.opt.Region {
		return nil, apiErrf("AuthorizationHeaderMalformed", fmt.Sprintf("the region '%s' is wrong; expecting '%s'", region, s.opt.Region))
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		amzDate = r.Header.Get("Date")
	}
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		if t, err = http.ParseTime(amzDate); err != nil {
			return nil, apiErrf("AccessDenied", "missing or malformed request date")
		}
	}
	if t.UTC().Format("20060102") != day {
		return nil, apiErrf("AuthorizationHeaderMalformed", "the credential date does not match the request date")
	}
	if d := time.Since(t); d > maxSkew || d < -maxSkew {
		return nil, apiErr("RequestTimeTooSkewed")
	}
	signed := strings.Split(signedList, ";")
	isSigned := map[string]bool{}
	for _, h := range signed {
		isSigned[h] = true
	}
	if !isSigned["host"] {
		return nil, apiErrf("AccessDenied", "the host header must be signed")
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		return nil, apiErrf("InvalidRequest", "missing required header x-amz-content-sha256")
	}
	if !isSigned["x-amz-content-sha256"] {
		return nil, apiErrf("AccessDenied", "x-amz-content-sha256 must be signed")
	}
	// like S3: every x-amz-* header that is present must be signed (a header added in flight is rejected)
	for name := range r.Header {
		l := strings.ToLower(name)
		if strings.HasPrefix(l, "x-amz-") && !isSigned[l] {
			return nil, apiErrf("AccessDenied", "there were headers present in the request which were not signed: "+l)
		}
	}
	var ch strings.Builder
	for _, h := range signed {
		var v string
		switch h {
		case "host":
			v = r.Host
		case "content-length":
			v = strconv.FormatInt(r.ContentLength, 10)
		default:
			vals, present := r.Header[http.CanonicalHeaderKey(h)]
			if !present {
				return nil, apiErrf("SignatureDoesNotMatch", "a signed header is missing: "+h)
			}
			parts := make([]string, len(vals))
			for i, x := range vals {
				parts[i] = trimSpaces(x)
			}
			v = strings.Join(parts, ",")
		}
		ch.WriteString(h + ":" + v + "\n")
	}
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{r.Method, uriEncode(path, true), canonicalQuery(r.URL.RawQuery), ch.String(), signedList, payload}, "\n")
	scope := strings.Join(cp[1:], "/")
	sts := strings.Join([]string{algoV4, t.UTC().Format("20060102T150405Z"), scope, sha256Hex([]byte(canonical))}, "\n")
	want := hex.EncodeToString(hmacSHA256(signingKey(secret, day, region), sts))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(sig))) {
		return nil, apiErr("SignatureDoesNotMatch")
	}
	return &authInfo{access: access, secret: secret, seed: want, date: t.UTC(), scope: scope, region: region, payloadHash: payload}, nil
}

// ---- request bodies -------------------------------------------------------------------------

// bodyReader returns a reader of the object bytes of r: the aws-chunked framing removed and every
// chunk signature verified (the chain starts at the request signature), or the raw body verified
// against a hex x-amz-content-sha256. The returned verify func must be called after the reader hit
// EOF: it checks the payload hash and the trailer, and returns any trailing checksum headers.
type bodyResult struct {
	r        io.Reader
	declared int64 // number of payload bytes announced, or -1
	finish   func() (http.Header, *apiError)
}

func (s *Server) openBody(r *http.Request, a *authInfo) (*bodyResult, *apiError) {
	dec := int64(-1)
	if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, apiErrf("InvalidRequest", "x-amz-decoded-content-length is not a number")
		}
		dec = n
	}
	switch a.payloadHash {
	case streamSigned, streamSignedTr, streamUnsigTr:
		if dec < 0 {
			return nil, apiErrf("MissingContentLength", "x-amz-decoded-content-length is required with a streaming payload")
		}
		cr := &chunkReader{br: bufio.NewReaderSize(r.Body, 128<<10), a: a, signed: a.payloadHash != streamUnsigTr, trailer: a.payloadHash != streamSigned}
		if cr.signed {
			cr.prev = a.seed
		}
		cr.verify = !a.noVerify
		return &bodyResult{r: cr, declared: dec, finish: func() (http.Header, *apiError) {
			if cr.err != nil && !errors.Is(cr.err, io.EOF) {
				return nil, chunkAPIError(cr.err)
			}
			if !cr.done {
				return nil, apiErrf("IncompleteBody", "the chunked stream ended before its last chunk")
			}
			if cr.n != dec {
				return nil, apiErrf("IncompleteBody", fmt.Sprintf("x-amz-decoded-content-length is %d, the stream carried %d bytes", dec, cr.n))
			}
			return cr.trailerHdr, nil
		}}, nil
	case unsignedBody:
		return &bodyResult{r: r.Body, declared: r.ContentLength, finish: func() (http.Header, *apiError) { return nil, nil }}, nil
	default:
		if len(a.payloadHash) != 64 {
			return nil, apiErrf("InvalidArgument", "unsupported x-amz-content-sha256 value")
		}
		h := sha256.New()
		tee := io.TeeReader(r.Body, h)
		return &bodyResult{r: tee, declared: r.ContentLength, finish: func() (http.Header, *apiError) {
			if hex.EncodeToString(h.Sum(nil)) != strings.ToLower(a.payloadHash) {
				return nil, apiErr("XAmzContentSHA256Mismatch")
			}
			return nil, nil
		}}, nil
	}
}

type chunkError struct {
	code string
	msg  string
}

func (e *chunkError) Error() string { return e.code + ": " + e.msg }

func chunkAPIError(err error) *apiError {
	var ce *chunkError
	if errors.As(err, &ce) {
		return apiErrf(ce.code, ce.msg)
	}
	return apiErrf("IncompleteBody", err.Error())
}

// chunkReader decodes the aws-chunked body: "<hex>[;chunk-signature=<sig>]\r\n<data>\r\n" repeated,
// ended by a zero-length chunk and, for the TRAILER variants, trailing headers.
type chunkReader struct {
	br         *bufio.Reader
	a          *authInfo
	signed     bool
	verify     bool
	trailer    bool
	prev       string
	buf        []byte // current chunk's data not yet returned
	n          int64  // payload bytes delivered
	done       bool   // zero chunk (and trailer) fully verified
	err        error
	trailerHdr http.Header
}

func (c *chunkReader) chunkSig(data []byte) string {
	sts := strings.Join([]string{"AWS4-HMAC-SHA256-PAYLOAD", c.a.date.Format("20060102T150405Z"), c.a.scope, c.prev, emptySHA256, sha256Hex(data)}, "\n")
	return hex.EncodeToString(hmacSHA256(signingKey(c.a.secret, c.a.scope[:8], c.a.region), sts))
}

func (c *chunkReader) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.err != nil {
			return 0, c.err
		}
		if c.done {
			return 0, io.EOF
		}
		c.err = c.next()
		if c.err != nil && !errors.Is(c.err, io.EOF) {
			return 0, c.err
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	c.n += int64(n)
	return n, nil
}

func (c *chunkReader) line() (string, error) {
	l, err := c.br.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return "", err
	}
	return strings.TrimRight(l, "\r\n"), nil
}

func (c *chunkReader) next() error {
	hdr, err := c.line()
	if err != nil {
		return err
	}
	sizeStr, sigPart, hasSig := strings.Cut(hdr, ";")
	size, perr := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
	if perr != nil || size < 0 || size > maxChunk {
		return &chunkError{"InvalidRequest", "bad chunk size " + strconv.Quote(sizeStr)}
	}
	var gotSig string
	if c.signed {
		sg, ok := strings.CutPrefix(sigPart, "chunk-signature=")
		if !hasSig || !ok {
			return &chunkError{"SignatureDoesNotMatch", "chunk without a signature"}
		}
		gotSig = sg
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(c.br, data); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if c.signed && c.verify {
		want := c.chunkSig(data)
		if !hmac.Equal([]byte(want), []byte(gotSig)) {
			return &chunkError{"SignatureDoesNotMatch", "a chunk signature does not match"}
		}
		c.prev = want
	}
	if size > 0 {
		var crlf [2]byte
		if _, err := io.ReadFull(c.br, crlf[:]); err != nil || crlf != [2]byte{'\r', '\n'} {
			return &chunkError{"IncompleteBody", "a chunk is not followed by CRLF"}
		}
		c.buf = data
		return nil
	}
	// the zero chunk: optional trailer, then the end
	if c.trailer {
		return c.readTrailer()
	}
	if c.signed {
		// "0;chunk-signature=...\r\n" is followed by an empty line
		if l, err := c.line(); err != nil || l != "" {
			return &chunkError{"IncompleteBody", "the last chunk is not followed by an empty line"}
		}
	} else if l, err := c.line(); err != nil || l != "" {
		return &chunkError{"IncompleteBody", "the last chunk is not followed by an empty line"}
	}
	c.done = true
	return io.EOF
}

func (c *chunkReader) readTrailer() error {
	var signedText bytes.Buffer
	hdr := http.Header{}
	var sig string
	for {
		l, err := c.line()
		if err != nil {
			return err
		}
		if l == "" {
			// the blank separators: one after the key lines (signed variant) and the final one
			if c.signed && sig == "" {
				continue
			}
			break
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			return &chunkError{"InvalidRequest", "bad trailer line"}
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "x-amz-trailer-signature" {
			sig = strings.TrimSpace(v)
			continue
		}
		signedText.WriteString(k + ":" + strings.TrimSpace(v) + "\n")
		hdr.Set(k, strings.TrimSpace(v))
	}
	if c.signed && c.verify {
		sts := strings.Join([]string{"AWS4-HMAC-SHA256-TRAILER", c.a.date.Format("20060102T150405Z"), c.a.scope, c.prev, sha256Hex(signedText.Bytes())}, "\n")
		want := hex.EncodeToString(hmacSHA256(signingKey(c.a.secret, c.a.scope[:8], c.a.region), sts))
		if !hmac.Equal([]byte(want), []byte(sig)) {
			return &chunkError{"SignatureDoesNotMatch", "the trailer signature does not match"}
		}
	}
	c.trailerHdr = hdr
	c.done = true
	return io.EOF
}
