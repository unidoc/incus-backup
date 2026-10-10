// Package logring keeps the server's recent log in memory so an administrator can read it from the web
// UI (GET /system/logs) and attach it to a support report without shell access. A slog handler tees
// every record into a bounded ring (oldest lines drop out) and on to the real handler.
//
// Attributes whose key looks secret (token, password, secret, key, credential, authorization) are
// redacted when captured: the ring, the log viewer and the support report never hold them. The real
// output (stderr, the service log file) is left as it was.
package logring

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Line is one captured record.
type Line struct {
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"time"`
	Level   string    `json:"level"` // DEBUG, INFO, WARN, ERROR
	Message string    `json:"message"`
	Attrs   string    `json:"attrs"` // key=value pairs, secrets redacted
}

// Ring is a bounded, concurrency-safe buffer of log lines.
type Ring struct {
	mu   sync.Mutex
	buf  []Line
	next int
	full bool
	seq  int64
}

// New returns a ring holding the last n lines (n <= 0: 5000).
func New(n int) *Ring {
	if n <= 0 {
		n = 5000
	}
	return &Ring{buf: make([]Line, n)}
}

// Add appends a line (assigning its seq).
func (r *Ring) Add(l Line) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	l.Seq = r.seq
	r.buf[r.next] = l
	r.next++
	if r.next == len(r.buf) {
		r.next, r.full = 0, true
	}
}

// Query selects lines.
type Query struct {
	MinLevel slog.Level // lines below are left out
	Contains string     // case-insensitive substring of message or attributes
	After    int64      // only lines with seq > After
	Limit    int        // newest Limit lines of the selection (default 500, at most the ring size)
}

// Result is a query answer.
type Result struct {
	Lines []Line `json:"lines"`
	// LastSeq is the newest seq in the ring (pass it as After to follow).
	LastSeq int64 `json:"last_seq"`
	// Capacity and Dropped tell how much history the ring keeps.
	Capacity int   `json:"capacity"`
	Dropped  int64 `json:"dropped"`
}

// Get returns the matching lines, oldest first.
func (r *Ring) Get(q Query) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	if q.Limit <= 0 {
		q.Limit = 500
	}
	n := r.next
	start := 0
	if r.full {
		n = len(r.buf)
		start = r.next
	}
	needle := strings.ToLower(q.Contains)
	var out []Line
	for i := 0; i < n; i++ {
		l := r.buf[(start+i)%len(r.buf)]
		if l.Seq <= q.After || parseLevel(l.Level) < q.MinLevel {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(l.Message), needle) && !strings.Contains(strings.ToLower(l.Attrs), needle) {
			continue
		}
		out = append(out, l)
	}
	if len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	if out == nil {
		out = []Line{}
	}
	dropped := int64(0)
	if r.full {
		dropped = r.seq - int64(len(r.buf))
	}
	return Result{Lines: out, LastSeq: r.seq, Capacity: len(r.buf), Dropped: dropped}
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// ParseLevel reads "debug", "info", "warn", "error" (any case; "" = debug: everything).
func ParseLevel(s string) (slog.Level, error) {
	if s == "" {
		return slog.LevelDebug, nil
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("level must be debug, info, warn or error")
	}
	return l, nil
}

// secretKey reports whether an attribute key names something secret.
func secretKey(k string) bool {
	k = strings.ToLower(k)
	for _, w := range []string{"token", "password", "passwd", "secret", "credential", "authorization", "cookie", "private", "key"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// Redacted is what replaces a secret attribute value.
const Redacted = "[redacted]"

// Handler tees records into a ring and passes them on to Next (which may be nil).
type Handler struct {
	Ring  *Ring
	Next  slog.Handler
	Level slog.Leveler // minimum level captured into the ring (default: info)
	attrs []slog.Attr
	group string
}

// NewHandler returns a tee handler.
func NewHandler(r *Ring, next slog.Handler) *Handler { return &Handler{Ring: r, Next: next} }

// Enabled is true when either the ring or the next handler wants the level.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	min := slog.LevelInfo
	if h.Level != nil {
		min = h.Level.Level()
	}
	return l >= min || (h.Next != nil && h.Next.Enabled(ctx, l))
}

// Handle captures the record and forwards it.
func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	min := slog.LevelInfo
	if h.Level != nil {
		min = h.Level.Level()
	}
	if rec.Level >= min {
		var b strings.Builder
		var write func(a slog.Attr, prefix string)
		write = func(a slog.Attr, prefix string) {
			a.Value = a.Value.Resolve()
			key := a.Key
			if prefix != "" {
				key = prefix + "." + key
			}
			if a.Value.Kind() == slog.KindGroup {
				for _, g := range a.Value.Group() {
					write(g, key)
				}
				return
			}
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			v := a.Value.String()
			if secretKey(a.Key) {
				v = Redacted
			}
			if strings.ContainsAny(v, " \"=\n") {
				v = fmt.Sprintf("%q", v)
			}
			b.WriteString(key + "=" + v)
		}
		for _, a := range h.attrs { // qualified when they were added
			write(a, "")
		}
		rec.Attrs(func(a slog.Attr) bool { write(a, h.group); return true })
		h.Ring.Add(Line{Time: rec.Time.UTC(), Level: rec.Level.String(), Message: rec.Message, Attrs: b.String()})
	}
	if h.Next != nil && h.Next.Enabled(ctx, rec.Level) {
		return h.Next.Handle(ctx, rec)
	}
	return nil
}

// WithAttrs returns a handler with extra attributes.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append([]slog.Attr{}, h.attrs...)
	for _, a := range as {
		if h.group != "" {
			a.Key = h.group + "." + a.Key
		}
		c.attrs = append(c.attrs, a)
	}
	if h.Next != nil {
		c.Next = h.Next.WithAttrs(as)
	}
	return &c
}

// WithGroup returns a handler that qualifies later attributes.
func (h *Handler) WithGroup(name string) slog.Handler {
	c := *h
	if c.group != "" {
		c.group += "." + name
	} else {
		c.group = name
	}
	if h.Next != nil {
		c.Next = h.Next.WithGroup(name)
	}
	return &c
}
