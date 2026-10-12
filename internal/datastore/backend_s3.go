package datastore

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config is everything needed to reach an S3-compatible bucket. The object model
// stored there is identical to the filesystem one: nothing in it is S3-specific.
type S3Config struct {
	Endpoint  string `yaml:"endpoint" json:"endpoint"` // host[:port], or a URL with scheme
	Region    string `yaml:"region" json:"region"`
	Bucket    string `yaml:"bucket" json:"bucket"`
	Prefix    string `yaml:"prefix" json:"prefix"`
	AccessKey string `yaml:"access_key" json:"access_key"`
	SecretKey string `yaml:"secret_key" json:"secret_key"`
	// UseTLS defaults to true; false only for plain-HTTP test or LAN endpoints.
	NoTLS bool `yaml:"no_tls" json:"no_tls"`
	// CAFile trusts a private CA for the endpoint; InsecureSkipVerify is a last resort.
	CAFile             string `yaml:"ca_file" json:"ca_file"`
	CAPEM              string `yaml:"ca_pem" json:"ca_pem"` // the same, inline
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify" json:"insecure_skip_verify"`
	// PathStyle forces path-style addressing (most non-AWS services); default auto.
	PathStyle bool `yaml:"path_style" json:"path_style"`
}

// Validate reports a configuration problem without touching the network.
func (c S3Config) Validate() error {
	switch {
	case c.Endpoint == "":
		return errors.New("s3: endpoint is required")
	case c.Bucket == "":
		return errors.New("s3: bucket is required")
	case c.AccessKey == "" || c.SecretKey == "":
		return errors.New("s3: access_key and secret_key are required")
	}
	return nil
}

// s3TransportHook lets tests adjust the HTTP transport (e.g. disable keep-alives so every
// request is a fresh connection a fault-injecting proxy can break).
var s3TransportHook func(*http.Transport)

// S3Backend implements Backend on the S3 API directly (no FUSE, no filesystem emulation).
type S3Backend struct {
	mu     sync.RWMutex
	c      *minio.Client
	bucket string
	prefix string // "" or ends in "/"
	cfg    S3Config
}

func (b *S3Backend) cl() *minio.Client {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.c
}

// Rebind swaps the connection settings (a rotated credential) in place; bucket and prefix must not change.
func (b *S3Backend) Rebind(cfg S3Config) error {
	nb, err := NewS3Backend(cfg)
	if err != nil {
		return err
	}
	if nb.bucket != b.bucket || nb.prefix != b.prefix {
		return errors.New("s3: a rebind cannot change the bucket or the prefix")
	}
	b.mu.Lock()
	b.c, b.cfg = nb.c, cfg
	b.mu.Unlock()
	return nil
}

// NewS3Backend connects (lazily: no request is made) and validates the config.
func NewS3Backend(cfg S3Config) (*S3Backend, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ep := cfg.Endpoint
	secure := !cfg.NoTLS
	if strings.HasPrefix(ep, "https://") {
		ep, secure = strings.TrimPrefix(ep, "https://"), true
	} else if strings.HasPrefix(ep, "http://") {
		ep, secure = strings.TrimPrefix(ep, "http://"), false
	}
	ep = strings.TrimRight(ep, "/")
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure,
		Region: cfg.Region,
	}
	if cfg.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	// One transport for both schemes, sized for the request parallelism of the store: with fewer
	// idle connections than requests in flight, every request beyond the idle pool opens (and
	// on TLS, handshakes) a new connection, which costs one or two extra round trips each.
	tr, err := minio.DefaultTransport(secure)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	tr.MaxIdleConnsPerHost = 2 * DefaultRequestParallelism
	tr.MaxIdleConns = max(tr.MaxIdleConns, 4*DefaultRequestParallelism)
	opts.Transport = tr
	if secure {
		tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // explicit opt-in
		if cfg.CAFile != "" || cfg.CAPEM != "" {
			pem := []byte(cfg.CAPEM)
			if cfg.CAFile != "" {
				var err error
				if pem, err = os.ReadFile(cfg.CAFile); err != nil {
					return nil, fmt.Errorf("s3: ca_file: %w", err)
				}
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("s3: ca_file contains no certificate")
			}
			tc.RootCAs = pool
		}
		tr.TLSClientConfig = tc
	}
	if s3TransportHook != nil {
		s3TransportHook(tr)
	}
	c, err := minio.New(ep, opts)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	p := strings.Trim(cfg.Prefix, "/")
	if p != "" {
		p += "/"
	}
	return &S3Backend{c: c, bucket: cfg.Bucket, prefix: p, cfg: cfg}, nil
}

func (b *S3Backend) key(k string) (string, error) {
	if err := validKey(k); err != nil {
		return "", err
	}
	return b.prefix + k, nil
}

func s3Code(err error) string { return minio.ToErrorResponse(err).Code }

// isNotFound is true only for a missing OBJECT. A missing bucket is a hard error: treating it as
// "no objects" would make every chunk look missing and every repository look unowned.
func isNotFound(err error) bool {
	er := minio.ToErrorResponse(err)
	if er.Code == "NoSuchBucket" {
		return false
	}
	return er.Code == "NoSuchKey" || er.Code == "NotFound" || er.StatusCode == http.StatusNotFound
}

// Put implements Backend. A whole object goes up in one PUT; with ifAbsent the request
// carries If-None-Match: * so the service itself arbitrates concurrent creators.
func (b *S3Backend) Put(key string, data []byte, ifAbsent bool) error {
	k, err := b.key(key)
	if err != nil {
		return err
	}
	opts := minio.PutObjectOptions{ContentType: "application/octet-stream", DisableMultipart: true}
	if ifAbsent {
		opts.SetMatchETagExcept("*")
	}
	_, err = b.cl().PutObject(context.Background(), b.bucket, k, bytes.NewReader(data), int64(len(data)), opts)
	if err != nil {
		if ifAbsent && s3Code(err) == "PreconditionFailed" {
			return ErrExists
		}
		if ifAbsent && minio.ToErrorResponse(err).StatusCode == http.StatusPreconditionFailed {
			return ErrExists
		}
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

// Get implements Backend.
func (b *S3Backend) Get(key string) ([]byte, error) {
	k, err := b.key(key)
	if err != nil {
		return nil, err
	}
	o, err := b.cl().GetObject(context.Background(), b.bucket, k, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	defer o.Close()
	data, err := io.ReadAll(o)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	return data, nil
}

// Stat implements Backend.
func (b *S3Backend) Stat(key string) (Info, error) {
	k, err := b.key(key)
	if err != nil {
		return Info{}, err
	}
	st, err := b.cl().StatObject(context.Background(), b.bucket, k, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return Info{}, ErrNotFound
		}
		return Info{}, fmt.Errorf("s3 stat %s: %w", key, err)
	}
	return Info{Size: st.Size, ModTime: st.LastModified}, nil
}

// List implements Backend.
func (b *S3Backend) List(prefix string, fn func(string, Info) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for o := range b.cl().ListObjects(ctx, b.bucket, minio.ListObjectsOptions{Prefix: b.prefix + prefix, Recursive: true}) {
		if o.Err != nil {
			return fmt.Errorf("s3 list %s: %w", prefix, o.Err)
		}
		if err := fn(strings.TrimPrefix(o.Key, b.prefix), Info{Size: o.Size, ModTime: o.LastModified}); err != nil {
			return err
		}
	}
	return nil
}

// ListPrefixes implements Backend.
func (b *S3Backend) ListPrefixes(prefix string) ([]string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out []string
	full := b.prefix + prefix
	for o := range b.cl().ListObjects(ctx, b.bucket, minio.ListObjectsOptions{Prefix: full, Recursive: false}) {
		if o.Err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", prefix, o.Err)
		}
		if strings.HasSuffix(o.Key, "/") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(o.Key, full), "/"))
		}
	}
	return out, nil
}

// Delete implements Backend (idempotent: S3 delete of a missing key succeeds).
func (b *S3Backend) Delete(key string) error {
	k, err := b.key(key)
	if err != nil {
		return err
	}
	if err := b.cl().RemoveObject(context.Background(), b.bucket, k, minio.RemoveObjectOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	return nil
}

// Describe implements Backend.
func (b *S3Backend) Describe() string {
	return fmt.Sprintf("s3 %s/%s%s", b.cfg.Endpoint, b.bucket, strings.TrimSuffix("/"+b.prefix, "/"))
}

// Probe is the doctor check for an S3 target: every operation the repository needs,
// run against a scratch key under `_probe/`, each reported separately so a missing
// permission is named rather than guessed at.
type ProbeResult struct {
	Op  string `json:"op"`
	OK  bool   `json:"ok"`
	Err string `json:"err"`
}

func (b *S3Backend) Probe() []ProbeResult {
	var rs []ProbeResult
	add := func(op string, err error) bool {
		r := ProbeResult{Op: op, OK: err == nil}
		if err != nil {
			r.Err = err.Error()
		}
		rs = append(rs, r)
		return err == nil
	}
	ctx := context.Background()
	ok, err := b.cl().BucketExists(ctx, b.bucket)
	if err == nil && !ok {
		err = fmt.Errorf("bucket %q does not exist", b.bucket)
	}
	if !add("connect + bucket exists", err) {
		return rs
	}
	key := fmt.Sprintf("_probe/%d-%d", time.Now().UnixNano(), os.Getpid())
	defer b.Delete(key)
	data := []byte("incus-backup probe")
	if !add("put", b.Put(key, data, false)) {
		return rs
	}
	add("put if-absent (conditional write)", func() error {
		err := b.Put(key, data, true)
		if errors.Is(err, ErrExists) {
			return nil
		}
		if err == nil {
			return errors.New("service overwrote an existing object despite If-None-Match: * — commit markers are NOT safe on this service")
		}
		return err
	}())
	add("get", func() error {
		got, err := b.Get(key)
		if err == nil && !bytes.Equal(got, data) {
			err = errors.New("read-back differs")
		}
		return err
	}())
	add("stat", func() error { _, err := b.Stat(key); return err }())
	add("list", func() error {
		n := 0
		err := b.List("_probe/", func(string, Info) error { n++; return nil })
		if err == nil && n == 0 {
			err = errors.New("listing does not show the object just written")
		}
		return err
	}())
	add("delete", b.Delete(key))
	return rs
}
