package engine_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

// inflightRT counts concurrent requests per kind and slows them down so that overlap is visible.
type inflightRT struct {
	next                     http.RoundTripper
	missing, puts            atomic.Int64
	maxMissing, maxPuts      atomic.Int64
	missingCalls, putCalls   atomic.Int64
	delay                    time.Duration
	firstPutBeforeLastAnswer atomic.Bool
	answers                  atomic.Int64
	batches                  int64
}

func bump(cur, max *atomic.Int64) func() {
	n := cur.Add(1)
	for {
		m := max.Load()
		if n <= m || max.CompareAndSwap(m, n) {
			break
		}
	}
	return func() { cur.Add(-1) }
}

func (r *inflightRT) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(req.URL.Path, "/missing"):
		call := r.missingCalls.Add(1)
		defer bump(&r.missing, &r.maxMissing)()
		time.Sleep(r.delay)
		if call == r.batches {
			time.Sleep(40 * r.delay) // the last answer is slow: uploads of the others must not wait for it
		}
		resp, err := r.next.RoundTrip(req)
		r.answers.Add(1)
		return resp, err
	case req.Method == "PUT" && strings.Contains(req.URL.Path, "/chunks/"):
		r.putCalls.Add(1)
		if r.answers.Load() < r.batches {
			r.firstPutBeforeLastAnswer.Store(true)
		}
		defer bump(&r.puts, &r.maxPuts)()
		time.Sleep(time.Millisecond)
	}
	return r.next.RoundTrip(req)
}

// An upload of more chunks than one "missing" request carries: the questions are asked several at
// once (bounded), uploads start before the last answer is in (pipelining), never more uploads are in
// flight than UploadWorkers, every missing chunk arrives and the snapshot commits; a second upload of
// the same chunks sends nothing.
func TestUploadPipelinesMissingQuestionsWithUploads(t *testing.T) {
	n := testsupport.NewNode(t)
	keys := masterKeys(5)
	cr := n.Enrol("benfica", api.ScopeBackup)
	cl := n.Client(cr, keys.DomainID)
	const chunks = 4500 // three "missing" batches
	rt := &inflightRT{next: cl.HTTP.Transport, delay: 20 * time.Millisecond, batches: 3}
	cl.HTTP.Transport = rt
	sp, err := engine.NewSpool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sp.Close)
	idx := &format.Index{Kind: format.IndexDynamic}
	for i := 0; i < chunks; i++ {
		data := []byte(fmt.Sprintf("pipelined chunk %d", i))
		id, blob, err := keys.EncodeChunk(data, format.EncodeOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := sp.Put(keys.DomainID, id, blob); err != nil {
			t.Fatal(err)
		}
		idx.Size += uint64(len(data))
		idx.Entries = append(idx.Entries, format.Entry{End: idx.Size, ID: id})
	}
	e := &engine.Engine{Host: "benfica", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now, UploadWorkers: 6}
	tg := &engine.Target{Name: "t", Client: cl, Keys: keys}
	upload := func(at time.Time) int64 {
		snap, _ := format.NewSnapshotID(at, nil)
		p := &engine.Prepared{Obj: &engine.Object{Project: "default", Name: "volume/pipelined", Kind: engine.KindFilesystem, Policy: "p"},
			SnapID: snap, Created: at, Size: idx.Size, ByDomain: map[string]*engine.DomainSnapshot{keys.DomainID: {Index: idx, Keys: keys}}}
		sent, err := e.Upload(context.Background(), tg, p, sp)
		if err != nil {
			t.Fatal(err)
		}
		return sent
	}
	if sent := upload(n.Now.Add(-time.Hour)); sent == 0 {
		t.Fatal("nothing was uploaded")
	}
	if c := rt.putCalls.Load(); c != chunks {
		t.Fatalf("%d chunk uploads for %d missing chunks", c, chunks)
	}
	if m := rt.maxMissing.Load(); m != 2 {
		t.Fatalf("%d missing requests in flight at once (want 2)", m)
	}
	if !rt.firstPutBeforeLastAnswer.Load() {
		t.Fatal("no chunk was uploaded before the last answer arrived: the questions are not pipelined")
	}
	if p := rt.maxPuts.Load(); p > 6 || p < 2 {
		t.Fatalf("%d uploads in flight (UploadWorkers=6)", p)
	}
	rt.putCalls.Store(0)
	if sent := upload(n.Now.Add(-time.Minute)); sent != 0 || rt.putCalls.Load() != 0 {
		t.Fatalf("second upload of the same chunks sent %d bytes in %d requests", sent, rt.putCalls.Load())
	}
}
