package mgmt_test

import (
	"bufio"
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
)

// Login and setup carry no CSRF token, so a cross-site page must not be able to post them (login CSRF: an
// HTML form with enctype=text/plain can produce a valid JSON body).
func TestLoginRefusesCrossSiteRequests(t *testing.T) {
	w := newWeb(t)
	if _, err := w.auth.CreateUser(context.Background(), "eve", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	body := map[string]string{"name": "eve", "password": pw, "code": "="}
	if code, b := w.req("POST", "/api/v1/session", body, map[string]string{"Origin": "https://evil.example", "Content-Type": "application/json"}); code != http.StatusForbidden {
		t.Fatalf("foreign Origin: %d %s", code, b)
	}
	if code, b := w.req("POST", "/api/v1/session", body, map[string]string{"Content-Type": "text/plain"}); code != http.StatusForbidden {
		t.Fatalf("text/plain body: %d %s", code, b)
	}
	if code, b := w.req("POST", "/api/v1/setup", map[string]string{"token": "x", "name": "a", "password": "="}, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); code != http.StatusForbidden {
		t.Fatalf("form body on setup: %d %s", code, b)
	}
	host := strings.TrimPrefix(w.ts.URL, "http://")
	if code, b := w.req("POST", "/api/v1/session", body, map[string]string{"Origin": "http://" + host, "Content-Type": "application/json; charset=utf-8"}); code != 200 {
		t.Fatalf("same-origin JSON login: %d %s", code, b)
	}
}

// An open event stream must end when its session ends (logout, revocation, disabled user), not keep
// delivering events for as long as the connection lives.
func TestEventStreamEndsAfterLogout(t *testing.T) {
	old := mgmt.EventsRecheck
	mgmt.EventsRecheck = 100 * time.Millisecond
	t.Cleanup(func() { mgmt.EventsRecheck = old })
	w := newWeb(t)
	if _, err := w.auth.CreateUser(context.Background(), "eve", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	w.login("eve")
	req, _ := http.NewRequest("GET", w.ts.URL+"/api/v1/events", nil)
	resp, err := w.httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("events: %d", resp.StatusCode)
	}
	ended := make(chan string, 1)
	go func() {
		var all strings.Builder
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			all.WriteString(sc.Text() + "\n")
		}
		ended <- all.String()
	}()
	if code, b := w.req("DELETE", "/api/v1/session", nil, w.h()); code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", code, b)
	}
	select {
	case out := <-ended:
		if !strings.Contains(out, "event: signed_out") {
			t.Fatalf("stream ended without signed_out: %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event stream kept running after logout")
	}
}

// splitComma parses a query parameter of any signed-in user: it must be linear (the old version was
// quadratic, 34 s of CPU for 400 kB).
func TestSplitCommaIsLinear(t *testing.T) {
	if got := mgmt.SplitCommaForTest(",a,,b,"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("split: %q", got)
	}
	if got := mgmt.SplitCommaForTest(""); got != nil {
		t.Fatalf("empty: %q", got)
	}
	start := time.Now()
	mgmt.SplitCommaForTest(strings.Repeat("a", 4<<20))
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("4 MiB without a comma took %v", d)
	}
}
