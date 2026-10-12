package auth_test

// Regression test (review F7): SafeNext must refuse tab and other control characters: browsers delete
// them from a Location header, so "/\t/evil.example/" would become "//evil.example/". The browser step
// is modelled (strip tab/LF/CR, then resolve), not run in a browser.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/server/auth"
)

func TestSafeNextRedirectStaysOnHost(t *testing.T) {
	base, _ := url.Parse("https://backup.example/api/v1/auth/oidc/corp/callback")
	for _, next := range []string{"/\t/evil.example/x", "/\n/evil.example/x", "/%09/evil.example/x", "/jobs?x=1", "/"} {
		safe := auth.SafeNext(next)
		rec := httptest.NewRecorder()
		http.Redirect(rec, httptest.NewRequest("GET", base.String(), nil), safe, http.StatusFound)
		loc := rec.Header().Get("Location")
		browser := strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(loc) // WHATWG: remove ASCII tab or newline
		u, err := base.Parse(browser)
		host := "?"
		if err == nil {
			host = u.Host
		}
		got := fmt.Sprintf("next=%q | Location=%q | browser lands on host %s", next, loc, host)
		t.Log(got)
		if host != "backup.example" {
			t.Errorf("open redirect: %s", got)
		}
	}
}
