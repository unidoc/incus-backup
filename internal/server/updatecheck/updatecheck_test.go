package updatecheck_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/unidoc/incus-backup/internal/server/updatecheck"
)

func TestOffByDefaultNeverContactsAnything(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer ts.Close()
	c := &updatecheck.Checker{Current: "0.3.0", Settings: func(context.Context) (bool, string) { return false, ts.URL + "/releases/latest" }}
	if _, err := c.Check(context.Background()); err == nil {
		t.Fatal("check while off must be refused")
	}
	if st := c.Status(context.Background()); st.Enabled || st.UpdateAvailable {
		t.Fatalf("%+v", st)
	}
	if hits.Load() != 0 {
		t.Fatal("contacted the feed while off")
	}
}

func TestGitHubRedirectAndJSONFeeds(t *testing.T) {
	var method, ua string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ua = r.Method, r.Header.Get("User-Agent")
		switch r.URL.Path {
		case "/unidoc/incus-backup/releases/latest":
			http.Redirect(w, r, "/unidoc/incus-backup/releases/tag/v0.4.1", http.StatusFound)
		case "/unidoc/incus-backup/releases/tag/v0.4.1":
			t.Error("the redirect must not be followed")
		case "/version.json":
			_, _ = w.Write([]byte(`{"version": "0.3.0", "url": "https://example.com/r"}`))
		case "/garbage":
			_, _ = w.Write([]byte(`<html>`))
		}
	}))
	defer ts.Close()
	url := ts.URL + "/unidoc/incus-backup/releases/latest"
	c := &updatecheck.Checker{Current: "0.3.0", Settings: func(context.Context) (bool, string) { return true, url }}
	st, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if method != "HEAD" || ua == "" || st.Latest != "0.4.1" || !st.UpdateAvailable || st.CheckedAt == nil || st.Current != "0.3.0" {
		t.Fatalf("%s %+v", method, st)
	}
	url = ts.URL + "/version.json"
	st, err = c.Check(context.Background())
	if err != nil || st.UpdateAvailable || st.Latest != "0.3.0" || st.ReleaseURL != "https://example.com/r" || method != "GET" {
		t.Fatalf("%v %+v", err, st)
	}
	url = ts.URL + "/garbage"
	if st, err = c.Check(context.Background()); err == nil || st.Error == "" || st.Latest != "0.3.0" {
		t.Fatalf("garbage: %v %+v", err, st)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		l, c string
		want bool
	}{{"0.4.0", "0.3.9", true}, {"0.3.0", "0.3.0", false}, {"1.0", "0.9.9", true}, {"0.3.0", "0.3.0-rc1", true}, {"0.3.0-rc1", "0.3.0", false},
		{"0.10.0", "0.9.0", true}, {"x", "0.1", false}, {"0.2", "dev", false}} {
		if got := updatecheck.Newer(c.l, c.c); got != c.want {
			t.Errorf("Newer(%s,%s)=%v", c.l, c.c, got)
		}
	}
}
