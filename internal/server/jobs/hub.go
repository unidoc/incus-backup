package jobs

import (
	"context"
	"sync"
	"time"
)

// Event is one change of a job, for live views (SSE). It carries no params or result: a viewer that
// wants them calls GetJob.
type Event struct {
	JobID           string    `json:"job_id"`
	Repository      string    `json:"repository"`
	Host            string    `json:"host"`
	Type            string    `json:"type"`
	State           string    `json:"state"`
	Progress        int       `json:"progress"`
	Message         string    `json:"message"`
	Attempt         int       `json:"attempt"`
	CancelRequested bool      `json:"cancel_requested"`
	At              time.Time `json:"at"`
	// Lagged: events were dropped for this subscriber before this one (its buffer was full); it
	// should re-list instead of trusting its incremental view.
	Lagged bool `json:"lagged,omitempty"`
}

// SubscriberBuffer is the per-subscriber queue length; MaxSubscribers bounds the hub.
const (
	SubscriberBuffer = 64
	MaxSubscribers   = 256
)

// Hub fans job changes out to subscribers without ever blocking a publisher: a slow subscriber
// loses events and gets the next one flagged Lagged.
type Hub struct {
	mu   sync.Mutex
	subs map[*sub]struct{}
}

type sub struct {
	ch     chan Event
	lagged bool
}

func newHub() *Hub { return &Hub{subs: map[*sub]struct{}{}} }

// Subscribe returns a channel of job changes and a function that ends the subscription (and closes
// the channel). It fails (nil channel) when MaxSubscribers are already connected.
func (s *Service) Subscribe() (<-chan Event, func()) {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= MaxSubscribers {
		return nil, func() {}
	}
	x := &sub{ch: make(chan Event, SubscriberBuffer)}
	h.subs[x] = struct{}{}
	var once sync.Once
	return x.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, x)
			close(x.ch)
			h.mu.Unlock()
		})
	}
}

func (h *Hub) publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for x := range h.subs {
		ev := e
		ev.Lagged = x.lagged
		select {
		case x.ch <- ev:
			x.lagged = false
		default:
			x.lagged = true
		}
	}
}

func (s *Service) publish(j *Job) {
	s.hub.publish(Event{JobID: j.ID, Repository: j.Repository, Host: j.Host, Type: j.Type, State: j.State, Progress: j.Progress,
		Message: j.Message, Attempt: j.Attempt, CancelRequested: j.CancelRequested, At: s.now()})
}

func (s *Service) publishID(ctx context.Context, id string) {
	s.hub.mu.Lock()
	n := len(s.hub.subs)
	s.hub.mu.Unlock()
	if n == 0 {
		return
	}
	j, err := scanJob(s.Svc.DB.Pool().QueryRow(ctx, `SELECT `+jobCols+jobFrom+` WHERE j.id=$1`, id))
	if err == nil {
		s.publish(j)
	}
}
