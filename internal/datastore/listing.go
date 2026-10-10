package datastore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7"
)

// Sharded listing. chunks/ holds every chunk of the repository below 256 first-level prefixes
// (chunks/xx/yy/<id>). One recursive listing of an object store is a strictly sequential chain of
// pages (1000 keys per request); listing the first-level prefixes in parallel turns it into
// parallel chains and lets the service list them on all its cores. The keys are delivered in the
// same order a single listing delivers them (by key), one prefix at a time.

// listedObject is one object of a shallow listing.
type listedObject struct {
	key  string
	info Info
}

// shallowLister is implemented by backends that can list one level below a prefix: the objects
// directly in it and the names of the "directories" below it.
type shallowLister interface {
	listShallow(prefix string) (objs []listedObject, dirs []string, err error)
}

func (b *S3Backend) listShallow(prefix string) ([]listedObject, []string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	full := b.prefix + prefix
	var objs []listedObject
	var dirs []string
	for o := range b.cl().ListObjects(ctx, b.bucket, minio.ListObjectsOptions{Prefix: full, Recursive: false}) {
		if o.Err != nil {
			return nil, nil, o.Err
		}
		name := strings.TrimPrefix(o.Key, full)
		if strings.HasSuffix(name, "/") {
			dirs = append(dirs, strings.TrimSuffix(name, "/"))
		} else {
			objs = append(objs, listedObject{key: prefix + name, info: Info{Size: o.Size, ModTime: o.LastModified}})
		}
	}
	return objs, dirs, nil
}

func (b *FSBackend) listShallow(prefix string) ([]listedObject, []string, error) {
	dir := strings.TrimSuffix(prefix, "/")
	if err := validKey(dir); err != nil {
		return nil, nil, err
	}
	es, err := os.ReadDir(filepath.Join(b.root, filepath.FromSlash(dir)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var objs []listedObject
	var dirs []string
	for _, e := range es {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		objs = append(objs, listedObject{key: prefix + e.Name(), info: Info{Size: info.Size(), ModTime: info.ModTime()}})
	}
	return objs, dirs, nil
}

func (m *MemBackend) listShallow(prefix string) ([]listedObject, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var objs []listedObject
	seen := map[string]bool{}
	var dirs []string
	for k, o := range m.objs {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if i := strings.Index(rest, "/"); i >= 0 {
			if !seen[rest[:i]] {
				seen[rest[:i]] = true
				dirs = append(dirs, rest[:i])
			}
			continue
		}
		objs = append(objs, listedObject{key: k, info: Info{Size: int64(len(o.data)), ModTime: o.mod}})
	}
	return objs, dirs, nil
}

// ListBufferMax bounds the listed keys a sharded listing holds before they are delivered (about
// 130 bytes each): shards are listed ahead in parallel only while fewer keys than this wait.
var ListBufferMax = 1 << 19

// listChunkObjects lists every object below chunks/, sharded by first-level prefix when the
// backend supports it, in key order.
func (s *Store) listChunkObjects(fn func(key string, info Info) error) error {
	sl := shallowOf(s.b)
	if sl == nil {
		return s.b.List("chunks/", fn)
	}
	return s.listSharded(sl, "chunks/", fn)
}

// shallowOf returns the shallow lister of b, looking through a maintenance-mode wrapper (whose
// mode is checked on every shallow listing, exactly as on its List), or nil.
func shallowOf(b Backend) shallowLister {
	if mb, ok := b.(*ModeBackend); ok {
		inner := shallowOf(mb.Backend)
		if inner == nil {
			return nil
		}
		return modeShallow{mb, inner}
	}
	if sl, ok := b.(shallowLister); ok {
		return sl
	}
	return nil
}

type modeShallow struct {
	mb    *ModeBackend
	inner shallowLister
}

func (m modeShallow) listShallow(prefix string) ([]listedObject, []string, error) {
	if err := m.mb.read(); err != nil {
		return nil, nil, err
	}
	return m.inner.listShallow(prefix)
}

func (s *Store) listSharded(sl shallowLister, prefix string, fn func(key string, info Info) error) error {
	objs, dirs, err := sl.listShallow(prefix)
	if err != nil {
		return err
	}
	// the order of one recursive listing: by name, an object before a directory of the same name
	type item struct {
		name  string
		obj   *listedObject
		shard int
	}
	items := make([]item, 0, len(objs)+len(dirs))
	for i := range objs {
		items = append(items, item{name: objs[i].key[len(prefix):], obj: &objs[i], shard: -1})
	}
	for i, d := range dirs {
		items = append(items, item{name: d + "/", shard: i})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	type shard struct {
		objs []listedObject
		err  error
	}
	results := make([]chan shard, len(dirs))
	for i := range results {
		results[i] = make(chan shard, 1)
	}
	// at most par shards are listed or waiting to be delivered at once, and no new shard starts
	// while ListBufferMax listed keys wait for delivery (bounded memory: on a large repository a
	// window of par shards would otherwise hold a sizeable fraction of the whole listing)
	window := make(chan struct{}, s.parallelism())
	stop := make(chan struct{})
	var (
		bufMu    sync.Mutex
		bufCond  = sync.NewCond(&bufMu)
		buffered int
		inFlight int
		stopped  bool
	)
	defer func() {
		close(stop)
		bufMu.Lock()
		stopped = true
		bufMu.Unlock()
		bufCond.Broadcast()
	}()
	go func() {
		for _, it := range items {
			if it.shard < 0 {
				continue
			}
			select {
			case window <- struct{}{}:
			case <-stop:
				return
			}
			bufMu.Lock()
			for buffered >= ListBufferMax && inFlight > 0 && !stopped {
				bufCond.Wait()
			}
			if stopped {
				bufMu.Unlock()
				return
			}
			inFlight++
			bufMu.Unlock()
			go func(i int, p string) {
				var sh shard
				sh.err = s.b.List(p, func(k string, info Info) error {
					sh.objs = append(sh.objs, listedObject{k, info})
					return nil
				})
				bufMu.Lock()
				buffered += len(sh.objs)
				bufMu.Unlock()
				results[i] <- sh
			}(it.shard, prefix+dirs[it.shard]+"/")
		}
	}()
	for _, it := range items {
		if it.obj != nil {
			if err := fn(it.obj.key, it.obj.info); err != nil {
				return err
			}
			continue
		}
		sh := <-results[it.shard]
		<-window
		if sh.err != nil {
			return sh.err
		}
		for j, o := range sh.objs {
			sh.objs[j] = listedObject{}
			if err := fn(o.key, o.info); err != nil {
				return err
			}
		}
		bufMu.Lock()
		buffered -= len(sh.objs)
		inFlight--
		bufMu.Unlock()
		bufCond.Broadcast()
	}
	return nil
}
