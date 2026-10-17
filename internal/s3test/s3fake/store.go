package s3fake

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// version is one stored version of an object (or a delete marker). In memory mode the body is
// data; in disk mode it is the tail of file starting at off.
type version struct {
	ID           string            `json:"id"`
	Key          string            `json:"key"`
	DeleteMarker bool              `json:"delete_marker,omitempty"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"` // hex MD5, unquoted
	Mod          time.Time         `json:"mod"`
	Hdr          map[string]string `json:"hdr,omitempty"` // lower-case header name -> value (content-type, x-amz-meta-*, ...)

	data []byte
	file string
	off  int64
}

type object struct {
	key      string
	versions []*version // newest first
}

func (o *object) latest() *version {
	if len(o.versions) == 0 {
		return nil
	}
	return o.versions[0]
}

// bucketConf is what is persisted of a bucket besides its objects.
type bucketConf struct {
	Created    time.Time `json:"created"`
	Versioning string    `json:"versioning"` // "", "Enabled", "Suspended"
	Lock       bool      `json:"lock"`
	LockXML    []byte    `json:"lock_xml,omitempty"`
	Lifecycle  []byte    `json:"lifecycle_xml,omitempty"`
}

type bucket struct {
	name string
	mu   sync.RWMutex
	conf bucketConf
	objs map[string]*object
	keys []string // sorted keys of objs
	dir  string   // "" in memory mode
}

type store struct {
	mu      sync.RWMutex
	buckets map[string]*bucket
	dir     string
}

func newStore(dir string) (*store, error) {
	st := &store{buckets: map[string]*bucket{}, dir: dir}
	if dir == "" {
		return st, nil
	}
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil {
		return nil, err
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if !e.IsDir() || e.Name() == "tmp" {
			continue
		}
		b, err := loadBucket(filepath.Join(dir, e.Name()), e.Name())
		if err != nil {
			return nil, fmt.Errorf("s3fake: loading bucket %s: %w", e.Name(), err)
		}
		st.buckets[e.Name()] = b
	}
	return st, nil
}

func (st *store) bucket(name string) *bucket {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.buckets[name]
}

func (st *store) names() []string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]string, 0, len(st.buckets))
	for n := range st.buckets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// create makes a bucket; false if it exists.
func (st *store) create(name string, lock bool) (*bucket, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if b, ok := st.buckets[name]; ok {
		return b, false, nil
	}
	b := &bucket{name: name, objs: map[string]*object{}, conf: bucketConf{Created: time.Now().UTC().Truncate(time.Millisecond), Lock: lock}}
	if lock {
		b.conf.Versioning = "Enabled"
	}
	if st.dir != "" {
		b.dir = filepath.Join(st.dir, name)
		if err := os.MkdirAll(filepath.Join(b.dir, "objects"), 0o755); err != nil {
			return nil, false, err
		}
		if err := b.saveConf(); err != nil {
			return nil, false, err
		}
	}
	st.buckets[name] = b
	return b, true, nil
}

func (st *store) remove(name string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	b := st.buckets[name]
	if b == nil {
		return nil
	}
	delete(st.buckets, name)
	if b.dir != "" {
		return os.RemoveAll(b.dir)
	}
	return nil
}

func (b *bucket) saveConf() error {
	if b.dir == "" {
		return nil
	}
	j, _ := json.Marshal(b.conf)
	return writeFileAtomic(filepath.Join(b.dir, "bucket.json"), j)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- disk format: [8 bytes header length][JSON version header][body] ----

func objFileName(key, id string) string {
	return hex.EncodeToString([]byte(key)) + "@" + id + ".obj"
}

func loadBucket(dir, name string) (*bucket, error) {
	b := &bucket{name: name, objs: map[string]*object{}, dir: dir}
	j, err := os.ReadFile(filepath.Join(dir, "bucket.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(j, &b.conf); err != nil {
		return nil, err
	}
	es, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".obj") {
			continue
		}
		p := filepath.Join(dir, "objects", e.Name())
		v, err := readHeader(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		o := b.objs[v.Key]
		if o == nil {
			o = &object{key: v.Key}
			b.objs[v.Key] = o
		}
		o.versions = append(o.versions, v)
	}
	for k, o := range b.objs {
		sort.SliceStable(o.versions, func(i, j int) bool { return o.versions[i].Mod.After(o.versions[j].Mod) })
		b.keys = append(b.keys, k)
	}
	sort.Strings(b.keys)
	return b, nil
}

func readHeader(path string) (*version, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var l [8]byte
	if _, err := io.ReadFull(f, l[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint64(l[:])
	if n > 1<<20 {
		return nil, errors.New("corrupt header length")
	}
	hb := make([]byte, n)
	if _, err := io.ReadFull(f, hb); err != nil {
		return nil, err
	}
	v := &version{}
	if err := json.Unmarshal(hb, v); err != nil {
		return nil, err
	}
	v.file, v.off = path, 8+int64(n)
	return v, nil
}

// ---- body sink ----

// sink collects an incoming body (memory or a temp file) while hashing it; nothing is visible to
// readers until commit, so a torn upload leaves no trace.
type sink struct {
	md5 hash.Hash
	buf bytes.Buffer
	f   *os.File
	n   int64
}

func (s *Server) newSink() (*sink, error) {
	k := &sink{md5: md5.New()}
	if s.st.dir != "" {
		f, err := os.CreateTemp(filepath.Join(s.st.dir, "tmp"), "put-*")
		if err != nil {
			return nil, err
		}
		k.f = f
	}
	return k, nil
}

func (k *sink) Write(p []byte) (int, error) {
	k.md5.Write(p)
	k.n += int64(len(p))
	if k.f != nil {
		return k.f.Write(p)
	}
	return k.buf.Write(p)
}

func (k *sink) etag() string { return hex.EncodeToString(k.md5.Sum(nil)) }

func (k *sink) discard() {
	if k.f != nil {
		name := k.f.Name()
		k.f.Close()
		os.Remove(name)
	}
}

func newVersionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---- operations; the caller holds b.mu (write) ----

func (b *bucket) insertKey(key string) {
	i := sort.SearchStrings(b.keys, key)
	b.keys = append(b.keys, "")
	copy(b.keys[i+1:], b.keys[i:])
	b.keys[i] = key
}

func (b *bucket) dropKey(key string) {
	i := sort.SearchStrings(b.keys, key)
	if i < len(b.keys) && b.keys[i] == key {
		b.keys = append(b.keys[:i], b.keys[i+1:]...)
	}
}

// current returns the live (non-delete-marker) latest version, or nil.
func (b *bucket) current(key string) *version {
	o := b.objs[key]
	if o == nil {
		return nil
	}
	if v := o.latest(); v != nil && !v.DeleteMarker {
		return v
	}
	return nil
}

// persist writes the version's file (disk mode) from the sink and sets data/file/off.
func (b *bucket) persist(v *version, k *sink) error {
	if b.dir == "" {
		if k != nil {
			v.data = append([]byte(nil), k.buf.Bytes()...)
		}
		return nil
	}
	hb, _ := json.Marshal(v)
	path := filepath.Join(b.dir, "objects", objFileName(v.Key, v.ID))
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(hb)))
	if k == nil || k.f == nil { // delete marker
		if err := writeFileAtomic(path, append(l[:], hb...)); err != nil {
			return err
		}
		v.file, v.off = path, 8+int64(len(hb))
		return nil
	}
	// header first, then the body already in the temp file: rewrite as header + body
	tmp := k.f.Name() + ".full"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := out.Write(append(l[:], hb...)); err != nil {
		out.Close()
		return err
	}
	if _, err := k.f.Seek(0, io.SeekStart); err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(out, k.f); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	v.file, v.off = path, 8+int64(len(hb))
	return nil
}

func (b *bucket) unlink(v *version) {
	if v.file != "" {
		_ = os.Remove(v.file)
	}
}

// put stores the new version according to the bucket's versioning state.
func (b *bucket) put(v *version, k *sink) error {
	switch b.conf.Versioning {
	case "Enabled":
		v.ID = newVersionID()
	default:
		v.ID = "null"
	}
	if err := b.persist(v, k); err != nil {
		return err
	}
	o := b.objs[v.Key]
	if o == nil {
		o = &object{key: v.Key}
		b.objs[v.Key] = o
		b.insertKey(v.Key)
	}
	if b.conf.Versioning != "Enabled" { // the "null" version is replaced
		keep := o.versions[:0]
		for _, old := range o.versions {
			if old.ID == "null" {
				if old.file != v.file {
					b.unlink(old)
				}
				continue
			}
			keep = append(keep, old)
		}
		o.versions = keep
	}
	o.versions = append([]*version{v}, o.versions...)
	return nil
}

// remove implements DELETE. With a version id that version goes for good; without one an
// unversioned bucket drops the object and a versioned one adds a delete marker.
// It returns the id of the affected version and whether it is (was) a delete marker.
func (b *bucket) remove(key, versionID string) (id string, marker bool, err error) {
	o := b.objs[key]
	if versionID != "" {
		if o == nil {
			return versionID, false, nil
		}
		for i, v := range o.versions {
			if v.ID == versionID {
				o.versions = append(o.versions[:i:i], o.versions[i+1:]...)
				b.unlink(v)
				if len(o.versions) == 0 {
					delete(b.objs, key)
					b.dropKey(key)
				}
				return versionID, v.DeleteMarker, nil
			}
		}
		return versionID, false, nil
	}
	if b.conf.Versioning == "" {
		if o != nil {
			for _, v := range o.versions {
				b.unlink(v)
			}
			delete(b.objs, key)
			b.dropKey(key)
		}
		return "", false, nil
	}
	m := &version{Key: key, DeleteMarker: true, Mod: time.Now().UTC().Truncate(time.Millisecond)}
	if b.conf.Versioning == "Enabled" {
		m.ID = newVersionID()
	} else {
		m.ID = "null"
	}
	if err := b.persist(m, nil); err != nil {
		return "", false, err
	}
	if o == nil {
		o = &object{key: key}
		b.objs[key] = o
		b.insertKey(key)
	}
	if m.ID == "null" {
		keep := o.versions[:0]
		for _, old := range o.versions {
			if old.ID == "null" {
				if old.file != m.file {
					b.unlink(old)
				}
				continue
			}
			keep = append(keep, old)
		}
		o.versions = keep
	}
	o.versions = append([]*version{m}, o.versions...)
	return m.ID, true, nil
}

// reader opens the body of v.
func (v *version) reader() (io.ReadSeekCloser, error) {
	if v.file == "" {
		return nopSeekCloser{bytes.NewReader(v.data)}, nil
	}
	f, err := os.Open(v.file)
	if err != nil {
		return nil, err
	}
	return &sectionFile{SectionReader: io.NewSectionReader(f, v.off, v.Size), f: f}, nil
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

type sectionFile struct {
	*io.SectionReader
	f *os.File
}

func (s *sectionFile) Close() error { return s.f.Close() }
