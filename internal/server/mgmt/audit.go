package mgmt

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/server/auth"
)

// Security-relevant audit beyond state-changing requests (which build() records one by one):
//
//   - refused requests: a role check (403), a CSRF/origin check, a cross-site login or setup. The
//     first of a kind (actor, action, reason) in a window is written at once; repeats within the
//     window become one summary entry ("repeated": n) when the window ends.
//   - downloads of sealed deliveries (GET /jobs/{id}/delivery): one entry each, with the job id and
//     the bytes sent.
//   - raw repository material (index, manifest, chunks): never one entry per request (a browser
//     reads thousands of chunks), but one summary per actor, repository and snapshot per window
//     with counts and bytes. Chunk URLs carry no snapshot; chunks are attributed to the snapshot
//     whose index or manifest the same actor fetched last from that repository (stated in the
//     entry as "chunks_attributed").
//
// The volume stays bounded: at most one entry per (actor, action, reason) and per (actor,
// repository, snapshot) per window, plus one per delivery download.

// AuditWindow is the summary window of security audit entries (a variable for tests).
var AuditWindow = time.Minute

type aggEntry struct {
	first  time.Time
	entry  auth.AuditEntry
	detail map[string]any
	count  int64 // requests after the first (refusals) or all requests (raw reads)
	counts map[string]int64
	bytes  int64
	raw    bool
}

type auditAgg struct {
	window   time.Duration // AuditWindow when the server started
	mu       sync.Mutex
	now      func() time.Time
	write    func(auth.AuditEntry)
	pending  map[string]*aggEntry
	lastSnap map[string]string // actor|repository -> group/snap last opened
	running  bool
	stop     chan struct{}
}

func newAuditAgg(write func(auth.AuditEntry)) *auditAgg {
	return &auditAgg{window: AuditWindow, now: time.Now, write: write, pending: map[string]*aggEntry{}, lastSnap: map[string]string{}}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// refused records a refused request: immediately the first time, as a count afterwards.
func (a *auditAgg) refused(e auth.AuditEntry, reason string) {
	key := "refused\x00" + e.Actor + "\x00" + e.Action + "\x00" + reason + "\x00" + e.IP
	a.mu.Lock()
	if x, ok := a.pending[key]; ok {
		x.count++
		a.mu.Unlock()
		return
	}
	d := map[string]any{"reason": reason}
	e.Detail, e.OK = mustJSON(d), false
	a.pending[key] = &aggEntry{first: a.now(), entry: e, detail: d}
	a.startLocked()
	a.mu.Unlock()
	a.write(e)
}

// rawRead counts one fetch of repository material (what = index, manifest or chunk).
func (a *auditAgg) rawRead(e auth.AuditEntry, repo, group, snap, what string, bytes int64, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	who := e.Actor + "\x00" + repo
	target, attributed := "", false
	if what == "chunk" {
		if target = a.lastSnap[who]; target != "" {
			attributed = true
		} else {
			target = "chunks"
		}
	} else {
		target = group + "/" + snap
		a.lastSnap[who] = target
	}
	key := "raw\x00" + who + "\x00" + target
	x, found := a.pending[key]
	if !found {
		e.Action = "read repository material"
		e.Target = repo + ":" + target
		x = &aggEntry{first: a.now(), entry: e, raw: true, counts: map[string]int64{}, detail: map[string]any{"repository": repo}}
		if target != "chunks" {
			x.detail["group"], x.detail["snapshot"], _ = strings.Cut(target, "/")
		}
		a.pending[key] = x
		a.startLocked()
	}
	x.count++
	x.counts[what]++
	if !ok {
		x.counts["failed"]++
	}
	if attributed {
		x.counts["chunks_attributed"]++
	}
	x.bytes += bytes
}

// startLocked starts the flusher when it is not running (a.mu held). It ends by itself when
// nothing is pending, so an idle server runs no goroutine.
func (a *auditAgg) startLocked() {
	if a.running {
		return
	}
	a.running = true
	a.stop = make(chan struct{})
	stop := a.stop
	go func() {
		tick := a.window / 4
		if tick < 10*time.Millisecond {
			tick = 10 * time.Millisecond
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			if !a.flush(false) {
				return
			}
		}
	}()
}

// flush writes the entries whose window ended (all of them when all is set) and reports whether
// some remain pending (the flusher keeps running). It stops the flusher when nothing is left.
func (a *auditAgg) flush(all bool) bool {
	a.mu.Lock()
	now := a.now()
	var out []auth.AuditEntry
	for k, x := range a.pending {
		if !all && now.Sub(x.first) < a.window {
			continue
		}
		delete(a.pending, k)
		if x.raw {
			for n, c := range x.counts {
				x.detail[n] = c
			}
			x.detail["requests"], x.detail["bytes"] = x.count, x.bytes
			x.detail["from"], x.detail["to"] = x.first.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339)
			e := x.entry
			e.Detail, e.OK = mustJSON(x.detail), x.counts["failed"] == 0
			out = append(out, e)
		} else if x.count > 0 {
			d := map[string]any{}
			for n, v := range x.detail {
				d[n] = v
			}
			d["repeated"], d["from"], d["to"] = x.count, x.first.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339)
			e := x.entry
			e.Detail = mustJSON(d)
			out = append(out, e)
		}
	}
	more := len(a.pending) > 0
	if !more && a.running {
		a.running = false
		close(a.stop)
	}
	a.mu.Unlock()
	for _, e := range out {
		a.write(e)
	}
	return more
}

// FlushAudit writes every pending security summary now (shutdown, tests).
func (s *Server) FlushAudit() {
	s.init()
	s.audit.flush(true)
}

func (s *Server) writeAudit(e auth.AuditEntry) {
	if s.Auth == nil {
		return
	}
	if err := s.Auth.Audit(context.Background(), e); err != nil && s.Log != nil {
		s.Log.Warn("could not write an audit entry", "action", e.Action, "err", err)
	}
}

// auditRefused records a refused request of r (actor may be "" for an unauthenticated caller).
func (s *Server) auditRefused(r *http.Request, p auth.Principal, pattern, reason string) {
	actor, kind := p.Name(), p.Kind
	if actor == "" {
		actor, kind = "anonymous", "anonymous"
	}
	s.audit.refused(auth.AuditEntry{Actor: actor, Kind: kind, Action: auditAction(pattern), Target: truncTarget(r.URL.Path), IP: clientIP(r)}, reason)
}

func truncTarget(p string) string {
	if len(p) > 512 {
		return p[:512]
	}
	return p
}

// auditRead records what the security audit wants from a GET that completed (sw holds the
// status and the bytes written).
func (s *Server) auditRead(r *http.Request, p auth.Principal, pattern string, sw *statusWriter) {
	ok := sw.code < 400
	switch pattern {
	case "GET /api/v1/jobs/{id}/delivery":
		s.writeAudit(auth.AuditEntry{Actor: p.Name(), Kind: p.Kind, Action: "download delivery", Target: truncTarget(r.URL.Path), IP: clientIP(r), OK: ok,
			Detail: mustJSON(map[string]any{"job": r.PathValue("id"), "bytes": sw.n, "status": sw.status()})})
	case "GET /api/v1/snapshots/{group}/{snap}/index", "GET /api/v1/snapshots/{group}/{snap}/manifest", "GET /api/v1/chunks/{id}":
		what := pattern[strings.LastIndexByte(pattern, '/')+1:]
		if strings.HasPrefix(pattern, "GET /api/v1/chunks/") {
			what = "chunk"
		}
		repo := r.URL.Query().Get("repository")
		if repo == "" {
			repo = "(default)"
		}
		s.audit.rawRead(auth.AuditEntry{Actor: p.Name(), Kind: p.Kind, IP: clientIP(r)}, repo, r.PathValue("group"), r.PathValue("snap"), what, sw.n, ok)
	}
}
