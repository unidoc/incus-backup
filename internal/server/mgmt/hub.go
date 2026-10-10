package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/server/auth"
)

// Event is one live update pushed to browsers.
type Event struct {
	Type string          `json:"type"` // e.g. "job", "alert", "run", "host"
	Data json.RawMessage `json:"data"`
}

// Hub fans events out to the open event streams. Publishing never blocks: a subscriber that cannot keep
// up loses events (the UI refetches on reconnect), it never slows the server.
type Hub struct {
	mu sync.Mutex
	// subs maps each subscriber to its repository filter (nil = sees everything).
	subs map[chan Event]func(repo string) bool
	// Resolve names the repository of an event ("" = global); needed only for filtered subscribers.
	Resolve func(typ string, data json.RawMessage) string
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan Event]func(string) bool{}} }

// Subscribe returns a channel and a cancel function.
func (h *Hub) Subscribe() (<-chan Event, func()) { return h.SubscribeFiltered(nil) }

// SubscribeFiltered is Subscribe for a subscriber that may only see some repositories: allow gets the
// event's repository ("" for global events) and decides.
func (h *Hub) SubscribeFiltered(allow func(repo string) bool) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = allow
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Publish sends an event to every subscriber.
func (h *Hub) Publish(typ string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	e := Event{Type: typ, Data: b}
	h.mu.Lock()
	filtered := false
	for _, allow := range h.subs {
		filtered = filtered || allow != nil
	}
	h.mu.Unlock()
	repo, resolved := "", false
	if filtered {
		if h.Resolve != nil {
			repo, resolved = h.Resolve(typ, b), true
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, allow := range h.subs {
		if allow != nil && (!resolved || !allow(repo)) {
			continue // fail closed: a filtered subscriber never gets an event it cannot place
		}
		select {
		case ch <- e:
		default:
		}
	}
}

// EventsRecheck is how often an open event stream re-validates its session or token (tests lower it): a
// stream must end when the session is logged out, revoked or expires, or the user is disabled.
var EventsRecheck = 20 * time.Second

// serveEvents is the Server-Sent Events handler.
func (h *Hub) serveEvents(w http.ResponseWriter, r *http.Request) {
	recheck, _ := r.Context().Value(recheckKey{}).(func(context.Context) bool)
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	var allow func(string) bool
	if p := PrincipalFrom(r.Context()); p.Scoped() {
		allow = func(repo string) bool { return visible(p, repo, auth.PermRead) }
	}
	ch, cancel := h.SubscribeFiltered(allow)
	defer cancel()
	tick := time.NewTicker(EventsRecheck)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b)
			fl.Flush()
		case <-tick.C:
			if recheck != nil && !recheck(r.Context()) {
				fmt.Fprint(w, "event: signed_out\ndata: {}\n\n")
				fl.Flush()
				return
			}
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}
