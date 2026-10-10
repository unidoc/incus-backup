package notify

import (
	"sync"
	"sync/atomic"
	"time"
)

// Change is one alert, silence or delivery change, published for the UI's live feed.
type Change struct {
	Kind     string    `json:"kind"` // alert, silence, delivery
	At       time.Time `json:"at"`
	AlertID  int64     `json:"alert_id,omitempty"`
	Key      string    `json:"key,omitempty"`
	Type     string    `json:"type,omitempty"`
	Severity string    `json:"severity,omitempty"`
	State    string    `json:"state,omitempty"`
	Title    string    `json:"title,omitempty"`
	// Transition: fired, resolved, escalated, updated, acknowledged, unacknowledged, silenced,
	// unsilenced, flapping, stable, expired (alerts); created, ended (silences); sent, failed, dead (deliveries).
	Transition string `json:"transition"`
	ID         int64  `json:"id,omitempty"` // silence or delivery id
}

// subscriberBuffer bounds what a slow subscriber may lag behind; beyond it changes are dropped for
// that subscriber only (publishing never blocks the engine).
const subscriberBuffer = 256

type hub struct {
	mu      sync.Mutex
	subs    map[chan Change]struct{}
	dropped atomic.Int64
}

// Subscribe returns a channel of changes made by this process. Call Unsubscribe when done.
// The feed is in-process: changes made by another process (a CLI command) appear only through
// the database (ListAlerts).
func (e *Engine) Subscribe() <-chan Change {
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	if e.hub.subs == nil {
		e.hub.subs = map[chan Change]struct{}{}
	}
	c := make(chan Change, subscriberBuffer)
	e.hub.subs[c] = struct{}{}
	return c
}

// Unsubscribe stops and closes a subscription.
func (e *Engine) Unsubscribe(c <-chan Change) {
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	for s := range e.hub.subs {
		if (<-chan Change)(s) == c {
			delete(e.hub.subs, s)
			close(s)
		}
	}
}

// DroppedChanges counts changes dropped because a subscriber's buffer was full.
func (e *Engine) DroppedChanges() int64 { return e.hub.dropped.Load() }

func (e *Engine) publish(cs ...Change) {
	if len(cs) == 0 {
		return
	}
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	for s := range e.hub.subs {
		for _, c := range cs {
			select {
			case s <- c:
			default:
				e.hub.dropped.Add(1)
			}
		}
	}
}
