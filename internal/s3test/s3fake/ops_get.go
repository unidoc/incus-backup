package s3fake

import (
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// findVersion resolves the version a read addresses: the explicit version id, else the latest.
// A latest delete marker reads as NoSuchKey (with the marker headers), as on S3.
func findVersion(b *bucket, key, versionID string, w http.ResponseWriter) (*version, *apiError) {
	o := b.objs[key]
	if versionID != "" {
		if o != nil {
			for _, v := range o.versions {
				if v.ID == versionID {
					if v.DeleteMarker {
						w.Header().Set("x-amz-delete-marker", "true")
						w.Header().Set("x-amz-version-id", v.ID)
						return nil, apiErr("MethodNotAllowed")
					}
					return v, nil
				}
			}
		}
		return nil, apiErr("NoSuchVersion")
	}
	if o == nil || o.latest() == nil {
		return nil, apiErr("NoSuchKey")
	}
	if v := o.latest(); v.DeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
		w.Header().Set("x-amz-version-id", v.ID)
		return nil, apiErr("NoSuchKey")
	}
	return o.latest(), nil
}

// parseRange handles one "bytes=a-b" range; ok=false means "no (usable) range: send everything".
func parseRange(h string, size int64) (start, length int64, ok bool, err *apiError) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false, nil
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false, nil
	}
	switch {
	case a == "": // suffix
		n, e := strconv.ParseInt(b, 10, 64)
		if e != nil || n < 0 {
			return 0, 0, false, nil
		}
		if n == 0 || size == 0 {
			return 0, 0, false, apiErr("InvalidRange")
		}
		n = min(n, size)
		return size - n, n, true, nil
	default:
		first, e := strconv.ParseInt(a, 10, 64)
		if e != nil || first < 0 {
			return 0, 0, false, nil
		}
		last := size - 1
		if b != "" {
			l, e := strconv.ParseInt(b, 10, 64)
			if e != nil || l < first {
				return 0, 0, false, nil
			}
			last = min(l, size-1)
		}
		if first >= size {
			return 0, 0, false, apiErr("InvalidRange")
		}
		return first, last - first + 1, true, nil
	}
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, rc *reqCtx) *apiError {
	b, aerr := s.lookup(rc)
	if aerr != nil {
		return aerr
	}
	b.mu.RLock()
	locked := true
	unlock := func() {
		if locked {
			locked = false
			b.mu.RUnlock()
		}
	}
	defer unlock()
	v, aerr := findVersion(b, rc.key, r.URL.Query().Get("versionId"), w)
	if aerr != nil {
		if aerr.Code == "NoSuchKey" && s.quirk(QuirkMissingKeyIs200) {
			w.Header().Set("ETag", quoteETag(md5hexEmpty))
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(200)
			return nil
		}
		return aerr
	}
	h := w.Header()
	if im := r.Header.Get("If-Match"); im != "" && !etagMatches(im, v.ETag) {
		return apiErr("PreconditionFailed")
	}
	h.Set("ETag", quoteETag(v.ETag))
	h.Set("Last-Modified", v.Mod.UTC().Format(http.TimeFormat))
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, v.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	for k, val := range v.Hdr {
		switch {
		case k == "content-type":
			h.Set("Content-Type", val)
		case strings.HasPrefix(k, "x-amz-meta-"):
			h.Set(k, val)
		default:
			h.Set(k, val)
		}
	}
	if b.conf.Versioning != "" {
		h.Set("x-amz-version-id", v.ID)
	}
	start, length, ranged := int64(0), v.Size, false
	if rg := r.Header.Get("Range"); rg != "" && !s.quirk(QuirkIgnoreRange) {
		st, ln, ok, err := parseRange(rg, v.Size)
		if err != nil {
			h.Del("ETag")
			h.Del("Last-Modified")
			h.Set("Content-Range", "bytes */"+strconv.FormatInt(v.Size, 10))
			return err
		}
		if ok {
			start, length, ranged = st, ln, true
		}
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	if ranged {
		h.Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+length-1, 10)+"/"+strconv.FormatInt(v.Size, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead || length == 0 {
		return nil
	}
	rd, err := v.reader()
	unlock() // versions are immutable: stream without holding the bucket lock
	if err != nil {
		return nil // headers are out; the client sees a short body
	}
	defer rd.Close()
	if _, err := rd.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	_, _ = io.CopyN(w, rd, length)
	return nil
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, rc *reqCtx) *apiError {
	b, aerr := s.lookup(rc)
	if aerr != nil {
		return aerr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.quirk(QuirkDeleteMissing404) && b.objs[rc.key] == nil {
		return apiErr("NoSuchKey")
	}
	if s.quirk(QuirkNoVersioningMarker) && b.conf.Versioning != "" && r.URL.Query().Get("versionId") == "" {
		// mutation: a versioned delete destroys every version instead of adding a marker
		if o := b.objs[rc.key]; o != nil {
			for _, v := range o.versions {
				b.unlink(v)
			}
			delete(b.objs, rc.key)
			b.dropKey(rc.key)
		}
		w.WriteHeader(204)
		return nil
	}
	id, marker, err := b.remove(rc.key, r.URL.Query().Get("versionId"))
	if err != nil {
		return apiErrf("InternalError", err.Error())
	}
	if id != "" {
		w.Header().Set("x-amz-version-id", id)
	}
	if marker {
		w.Header().Set("x-amz-delete-marker", "true")
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, rc *reqCtx, a *authInfo) *apiError {
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-copy-source-") {
			return apiErrf("NotImplemented", k+" is not implemented by the test server")
		}
	}
	dst, aerr := s.lookup(rc)
	if aerr != nil {
		return aerr
	}
	src := r.Header.Get("X-Amz-Copy-Source")
	if u, err := url.PathUnescape(src); err == nil {
		src = u
	}
	srcPath, srcQuery, _ := strings.Cut(src, "?")
	sb, sk := splitPath(srcPath)
	if sb == "" || sk == "" {
		return apiErrf("InvalidArgument", "Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	srcVer := ""
	if q, err := url.ParseQuery(srcQuery); err == nil {
		srcVer = q.Get("versionId")
	}
	sbk := s.st.bucket(sb)
	if sbk == nil {
		return apiErr("NoSuchBucket")
	}
	if _, aerr := s.readSmallDiscard(r, a); aerr != nil {
		return aerr
	}
	sbk.mu.RLock()
	sv, aerr := findVersion(sbk, sk, srcVer, w)
	if aerr != nil {
		sbk.mu.RUnlock()
		return aerr
	}
	k, err := s.newSink()
	if err != nil {
		sbk.mu.RUnlock()
		return apiErrf("InternalError", err.Error())
	}
	defer k.discard()
	rd, err := sv.reader()
	if err == nil {
		_, err = io.Copy(k, rd)
		rd.Close()
	}
	hdr := map[string]string{}
	for hk, hv := range sv.Hdr {
		hdr[hk] = hv
	}
	sbk.mu.RUnlock()
	if err != nil {
		return apiErrf("InternalError", err.Error())
	}
	if strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE") {
		hdr = storedHeaders(r.Header, nil)
	}
	v := &version{Key: rc.key, Size: k.n, ETag: k.etag(), Mod: time.Now().UTC().Truncate(time.Millisecond), Hdr: hdr}
	dst.mu.Lock()
	defer dst.mu.Unlock()
	if err := dst.put(v, k); err != nil {
		return apiErrf("InternalError", err.Error())
	}
	if dst.conf.Versioning == "Enabled" {
		w.Header().Set("x-amz-version-id", v.ID)
	}
	writeXML(w, 200, struct {
		XMLName      struct{} `xml:"CopyObjectResult"`
		ETag         string   `xml:"ETag"`
		LastModified string   `xml:"LastModified"`
	}{ETag: quoteETag(v.ETag), LastModified: isoTime(v.Mod)})
	return nil
}

// readSmallDiscard consumes (and verifies) the empty body of a copy request.
func (s *Server) readSmallDiscard(r *http.Request, a *authInfo) ([]byte, *apiError) {
	return readSmall(r, a, s)
}

var _ = sort.Strings

const md5hexEmpty = "d41d8cd98f00b204e9800998ecf8427e"
