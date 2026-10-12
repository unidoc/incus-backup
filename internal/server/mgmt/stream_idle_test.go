package mgmt_test

// Regression test (review F6): an open /api/v1/events stream re-validates its session every
// EventsRecheck through Store.Recheck, which must not move last_seen: a console left alone reaches the
// 30-minute idle timeout.

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
)

func TestEventStreamRecheckDoesNotRenewTheSession(t *testing.T) {
	old := mgmt.EventsRecheck
	mgmt.EventsRecheck = 20 * time.Millisecond
	t.Cleanup(func() { mgmt.EventsRecheck = old })
	w := newWeb(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	w.auth.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	if _, err := w.auth.CreateUser(context.Background(), "eve", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	w.login("eve")
	req, _ := http.NewRequest("GET", w.ts.URL+"/api/v1/events", nil)
	resp, err := w.httpc.Do(req) // the console's EventSource, open in a background tab
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 40 minutes without a single user request: only the stream's own rechecks run
	for i := 0; i < 20; i++ {
		clock.Add(int64(2 * time.Minute))
		time.Sleep(60 * time.Millisecond) // a few rechecks per step
	}
	code, _ := w.req("GET", "/api/v1/session", nil, nil)
	got := fmt.Sprintf("after 40 idle minutes GET /session = %d", code)
	if want := "after 40 idle minutes GET /session = 401"; got != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}

// Control: the same 40 idle minutes WITHOUT an open stream end the session on head too, so the
// failure above comes from the stream's rechecks, not from the clock or the harness.
func TestIdleSessionEndsWithoutStreamControl(t *testing.T) {
	w := newWeb(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	w.auth.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	if _, err := w.auth.CreateUser(context.Background(), "eve", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	w.login("eve")
	clock.Add(int64(40 * time.Minute))
	code, _ := w.req("GET", "/api/v1/session", nil, nil)
	if got, want := fmt.Sprintf("after 40 idle minutes GET /session = %d", code), "after 40 idle minutes GET /session = 401"; got != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}
