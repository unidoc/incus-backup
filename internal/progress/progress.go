// Package progress carries an optional progress and log reporter through a context, so long
// operations (GC, verify, retention, reconcile, sync, repository destroy) can report what they are
// doing without changing their signatures. Without a reporter every call is a no-op, which is the
// case for the CLI's direct path and for tests that do not care.
//
// It is a leaf package (no imports from this module) so the datastore, the service layer and the
// task manager can all use it.
package progress

import (
	"context"
	"fmt"
)

// Level of a log line.
type Level string

// Log levels (the same words as host_job_log and task_log).
const (
	Debug Level = "debug"
	Info  Level = "info"
	Warn  Level = "warn"
	Error Level = "error"
)

// Reporter receives progress and log lines. Implementations must be safe for concurrent use and must
// never block for long (operations call it from hot loops; throttle inside).
type Reporter interface {
	// Progress sets the completion (0-100, -1 = unknown) and a short message.
	Progress(pct int, msg string)
	// Log appends one line to the operation's log.
	Log(level Level, line string)
	// ID identifies the operation (a task id), "" when there is none.
	ID() string
}

type key struct{}

// With returns a context carrying r.
func With(ctx context.Context, r Reporter) context.Context {
	return context.WithValue(ctx, key{}, r)
}

// From returns the reporter of ctx, or nil.
func From(ctx context.Context) Reporter {
	r, _ := ctx.Value(key{}).(Reporter)
	return r
}

// Set reports progress (no-op without a reporter).
func Set(ctx context.Context, pct int, format string, a ...any) {
	if r := From(ctx); r != nil {
		r.Progress(pct, fmt.Sprintf(format, a...))
	}
}

// Logf appends a log line (no-op without a reporter).
func Logf(ctx context.Context, level Level, format string, a ...any) {
	if r := From(ctx); r != nil {
		r.Log(level, fmt.Sprintf(format, a...))
	}
}

// TaskID returns the id of the task ctx belongs to ("" outside a task).
func TaskID(ctx context.Context) string {
	if r := From(ctx); r != nil {
		return r.ID()
	}
	return ""
}

// Pct is done/total as a percentage clamped to [lo, hi] (a phase of an operation maps onto a slice
// of the 0-100 range). total <= 0 gives lo.
func Pct(done, total int64, lo, hi int) int {
	if total <= 0 || hi <= lo {
		return lo
	}
	p := lo + int(float64(hi-lo)*float64(done)/float64(total))
	if p > hi {
		p = hi
	}
	if p < lo {
		p = lo
	}
	return p
}
