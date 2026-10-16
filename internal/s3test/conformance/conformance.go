// Package conformance is the S3 conformance suite of incus-backup: every S3 behaviour the
// repository code relies on, checked through a real S3 client (minio-go) and raw signed HTTP.
//
// The same suite runs (a) in every normal `go test` against the in-process fake (internal/s3test/s3fake),
// which is how the fake is kept honest, and (b) against any real S3-compatible service when
// INCUS_BACKUP_TEST_S3_ENDPOINT / _ACCESS_KEY / _SECRET_KEY are set (CI job s3-conformance).
//
// Checks are plain functions (Checks()), so the fake's mutation tests can run one against a
// deliberately broken server and require it to FAIL. Optional features a service lacks are declared
// (Target.Unsupported / INCUS_BACKUP_TEST_S3_UNSUPPORTED); a declared feature is not skipped: the
// suite then requires the service to REFUSE it with an S3 error. Nothing is silently skipped.
package conformance

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/minio/minio-go/v7/pkg/signer"
)

// Feature names (Target.Unsupported).
const (
	FeatureVersioning  = "versioning"
	FeatureLifecycle   = "lifecycle"
	FeatureObjectLock  = "object-lock"
	FeatureMultiDelete = "multi-delete"
	FeatureCopy        = "copy"
)

// Features lists every optional feature a service may declare unsupported.
var Features = []string{FeatureVersioning, FeatureLifecycle, FeatureObjectLock, FeatureMultiDelete, FeatureCopy}

// Target is the service under test.
type Target struct {
	Endpoint  string // host:port (plain HTTP) or https://host:port
	AccessKey string
	SecretKey string
	// Unsupported declares optional features the service lacks (see Features).
	Unsupported []string
}

func (t Target) secure() bool { return strings.HasPrefix(t.Endpoint, "https://") }
func (t Target) host() string {
	return strings.TrimPrefix(strings.TrimPrefix(t.Endpoint, "https://"), "http://")
}
func (t Target) unsupported(f string) bool {
	for _, u := range t.Unsupported {
		if u == f {
			return true
		}
	}
	return false
}

// Check is one named conformance check. Feature is "" or one of Features.
type Check struct {
	Name    string
	Feature string
	Fn      func(c *C)
}

// C is the check's handle: Errorf records, Fatalf records and stops the check.
type C struct {
	Target Target
	Ctx    context.Context
	Client *minio.Client
	Core   minio.Core

	mu      sync.Mutex
	fails   []string
	buckets []string
	rnd     *rand.Rand
}

type stop struct{}

func (c *C) Errorf(format string, a ...any) {
	c.mu.Lock()
	c.fails = append(c.fails, fmt.Sprintf(format, a...))
	c.mu.Unlock()
}

func (c *C) Fatalf(format string, a ...any) {
	c.Errorf(format, a...)
	panic(stop{})
}

func (c *C) must(err error, what string) {
	if err != nil {
		c.Fatalf("%s: %v", what, err)
	}
}

// code is the S3 error code of err ("" if it is no S3 error response).
func code(err error) string { return minio.ToErrorResponse(err).Code }

func status(err error) int { return minio.ToErrorResponse(err).StatusCode }

// wantErr requires an S3 error with one of the codes (or, when none are given, any S3 error).
func (c *C) wantErr(err error, what string, codes ...string) {
	c.Helper()
	if err == nil {
		c.Fatalf("%s: succeeded, an error was required (%v)", what, codes)
	}
	er := minio.ToErrorResponse(err)
	if er.StatusCode == 0 {
		c.Fatalf("%s: %v is not an S3 error response", what, err)
	}
	if len(codes) == 0 {
		return
	}
	for _, cd := range codes {
		if er.Code == cd {
			return
		}
	}
	c.Fatalf("%s: code %q (HTTP %d), want one of %v", what, er.Code, er.StatusCode, codes)
}

func (c *C) Helper() {}

func newClient(t Target, access, secret string) (*minio.Client, error) {
	return minio.New(t.host(), &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: t.secure()})
}

// Bucket creates a fresh bucket (removed after the check).
func (c *C) Bucket() string {
	name := fmt.Sprintf("ibconf-%d-%d", time.Now().UnixNano()%1_000_000_000, atomic.AddInt64(&bucketSeq, 1))
	c.must(c.Client.MakeBucket(c.Ctx, name, minio.MakeBucketOptions{}), "make bucket")
	c.mu.Lock()
	c.buckets = append(c.buckets, name)
	c.mu.Unlock()
	return name
}

var bucketSeq int64

func (c *C) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, b := range c.buckets {
		objs := make(chan minio.ObjectInfo)
		go func() {
			defer close(objs)
			for o := range c.Client.ListObjects(ctx, b, minio.ListObjectsOptions{Recursive: true, WithVersions: !c.Target.unsupported(FeatureVersioning)}) {
				if o.Err == nil {
					objs <- o
				}
			}
		}()
		for range c.Client.RemoveObjects(ctx, b, objs, minio.RemoveObjectsOptions{}) {
		}
		_ = c.Client.RemoveBucket(ctx, b)
	}
}

// data returns n deterministic pseudo-random bytes.
func (c *C) data(n int) []byte {
	b := make([]byte, n)
	c.rnd.Read(b)
	return b
}

func (c *C) put(bucket, key string, data []byte, opts ...func(*minio.PutObjectOptions)) minio.UploadInfo {
	o := minio.PutObjectOptions{DisableMultipart: true}
	for _, f := range opts {
		f(&o)
	}
	info, err := c.Client.PutObject(c.Ctx, bucket, key, bytes.NewReader(data), int64(len(data)), o)
	c.must(err, "put "+key)
	return info
}

func (c *C) get(bucket, key string, o minio.GetObjectOptions) ([]byte, error) {
	obj, err := c.Client.GetObject(c.Ctx, bucket, key, o)
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}

func (c *C) mustGet(bucket, key string) []byte {
	d, err := c.get(bucket, key, minio.GetObjectOptions{})
	c.must(err, "get "+key)
	return d
}

func (c *C) list(bucket string, o minio.ListObjectsOptions) (keys []string, prefixes []string) {
	for obj := range c.Client.ListObjects(c.Ctx, bucket, o) {
		if obj.Err != nil {
			c.Fatalf("list %s %+v: %v", bucket, o, obj.Err)
		}
		if strings.HasSuffix(obj.Key, "/") && !o.Recursive {
			prefixes = append(prefixes, obj.Key)
		} else {
			keys = append(keys, obj.Key)
		}
	}
	return
}

func (c *C) equalStrings(what string, got, want []string) {
	if len(got) != len(want) {
		c.Fatalf("%s: %d entries %v, want %d %v", what, len(got), head(got), len(want), head(want))
	}
	for i := range got {
		if got[i] != want[i] {
			c.Fatalf("%s: entry %d is %q, want %q", what, i, got[i], want[i])
		}
	}
}

func head(s []string) []string {
	if len(s) > 12 {
		return append(append([]string(nil), s[:12]...), "...")
	}
	return s
}

// parallel runs fn(i) for 0 <= i < n on `workers` goroutines and returns the first error.
func parallel(n, workers int, fn func(i int) error) error {
	var wg sync.WaitGroup
	var first atomic.Value
	idx := int64(-1)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&idx, 1))
				if i >= n {
					return
				}
				if err := fn(i); err != nil {
					first.CompareAndSwap(nil, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if e := first.Load(); e != nil {
		return e.(error)
	}
	return nil
}

// Run executes one check against the target and returns its failures (nil = passed). A check of a
// feature the target declares unsupported runs its refusal variant instead.
func Run(tgt Target, ch Check) (fails []string) {
	cl, err := newClient(tgt, tgt.AccessKey, tgt.SecretKey)
	if err != nil {
		return []string{err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := &C{Target: tgt, Ctx: ctx, Client: cl, Core: minio.Core{Client: cl}, rnd: rand.New(rand.NewSource(42))}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(stop); !ok {
				c.Errorf("panic: %v", r)
			}
		}
		c.cleanup()
		fails = c.fails
	}()
	if ch.Feature != "" && tgt.unsupported(ch.Feature) {
		refused(c, ch.Feature)
		return
	}
	ch.Fn(c)
	return
}

// RunConformance runs the whole suite as subtests of t.
func RunConformance(t *testing.T, tgt Target) {
	t.Helper()
	for _, ch := range Checks() {
		t.Run(ch.Name, func(t *testing.T) {
			for _, f := range Run(tgt, ch) {
				t.Error(f)
			}
		})
	}
}

// refused is the check of a feature the target declares unsupported: the service must say so with
// an S3 error rather than hang or pretend.
func refused(c *C, feature string) {
	b := c.Bucket()
	var err error
	switch feature {
	case FeatureVersioning:
		err = c.Client.EnableVersioning(c.Ctx, b)
	case FeatureLifecycle:
		lc := lifecycle.NewConfiguration()
		lc.Rules = []lifecycle.Rule{{ID: "r", Status: "Enabled", Expiration: lifecycle.Expiration{Days: 1}}}
		err = c.Client.SetBucketLifecycle(c.Ctx, b, lc)
	case FeatureObjectLock:
		err = c.Client.MakeBucket(c.Ctx, b+"-l", minio.MakeBucketOptions{ObjectLocking: true})
	case FeatureMultiDelete:
		c.put(b, "k", []byte("x"))
		objs := make(chan minio.ObjectInfo, 1)
		objs <- minio.ObjectInfo{Key: "k"}
		close(objs)
		for r := range c.Client.RemoveObjects(c.Ctx, b, objs, minio.RemoveObjectsOptions{}) {
			err = r.Err
		}
	case FeatureCopy:
		c.put(b, "k", []byte("x"))
		_, err = c.Client.CopyObject(c.Ctx, minio.CopyDestOptions{Bucket: b, Object: "k2"}, minio.CopySrcOptions{Bucket: b, Object: "k"})
	}
	// a feature declared unsupported must fail CLEANLY (an S3 error response), not silently half-work
	if err == nil {
		c.Errorf("%s is declared unsupported but the service accepted it: drop it from the unsupported list", feature)
		return
	}
	if minio.ToErrorResponse(err).StatusCode == 0 {
		c.Errorf("%s is unsupported but the failure is not an S3 error response: %v", feature, err)
	}
}

func md5hex(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) }

var errTear = errors.New("conformance: simulated client failure")

type tearReader struct {
	r    io.Reader
	left int
}

func (t *tearReader) Read(p []byte) (int, error) {
	if t.left <= 0 {
		return 0, errTear
	}
	if len(p) > t.left {
		p = p[:t.left]
	}
	n, err := t.r.Read(p)
	t.left -= n
	return n, err
}

// rawSigned builds a SigV4-signed request (header form) the way a client library does.
func (c *C) rawSigned(method, path, rawQuery string) *http.Request {
	scheme := "http"
	if c.Target.secure() {
		scheme = "https"
	}
	u := scheme + "://" + c.Target.host() + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(c.Ctx, method, u, nil)
	c.must(err, "new request")
	req.Header.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	loc, err := c.Client.GetBucketLocation(c.Ctx, strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0])
	if err != nil || loc == "" {
		loc = "us-east-1"
	}
	return signer.SignV4(*req, c.Target.AccessKey, c.Target.SecretKey, "", loc)
}

func (c *C) do(req *http.Request) (int, []byte) {
	resp, err := http.DefaultClient.Do(req)
	c.must(err, "raw request")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func sortedCopy(s []string) []string {
	o := append([]string(nil), s...)
	sort.Strings(o)
	return o
}

// Lookup returns the check with that name.
func Lookup(name string) (Check, bool) {
	for _, ch := range Checks() {
		if ch.Name == name {
			return ch, true
		}
	}
	return Check{}, false
}
