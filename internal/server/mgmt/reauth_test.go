package mgmt_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/server/auth"
)

// A stolen session cookie must not mint a long-lived API key or add a second factor (review F8):
// token creation and TOTP enrolment need a recent sign-in or the password, like passkey changes.
func reauthWeb(t *testing.T, name string) (*web, *atomic.Int64) {
	t.Helper()
	w := newWeb(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	w.auth.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	if _, err := w.auth.CreateUser(context.Background(), name, pw, auth.Admin, "", "", false); err != nil {
		t.Fatal(err)
	}
	w.login(name)
	return w, &clock
}

func TestTokenCreationNeedsReauthentication(t *testing.T) {
	w, clock := reauthWeb(t, "anna")
	// a fresh sign-in counts as proof
	if code, b := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "fresh", "role": "readonly"}, w.h()); code != 201 {
		t.Fatalf("fresh sign-in: %d %s", code, b)
	}
	clock.Add(int64(auth.ReauthWindow + time.Minute)) // the session is still valid (idle limit 30 min) but old
	code, b := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "stolen", "role": "readonly"}, w.h())
	if code != 400 || !strings.Contains(string(b), `"password_required":true`) {
		t.Fatalf("old session without password: %d %s", code, b)
	}
	if code, _ := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "stolen", "role": "readonly", "password": "wrong password here"}, w.h()); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if code, b := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "ok", "role": "readonly", "password": pw}, w.h()); code != 201 {
		t.Fatalf("right password: %d %s", code, b)
	}
	code, b = w.req("GET", "/api/v1/me/tokens", nil, nil)
	if code != 200 || strings.Contains(string(b), `"stolen"`) {
		t.Fatalf("a refused request must not create a token: %d %s", code, b)
	}
}

func TestTokenLifetimeIsCapped(t *testing.T) {
	w, _ := reauthWeb(t, "anna")
	type resp struct {
		Info struct {
			Expires *time.Time `json:"expires_at"`
		}
	}
	create := func(name string, ttl float64) (int, resp) {
		code, b := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": name, "role": "readonly", "ttl_hours": ttl}, w.h())
		var r resp
		_ = json.Unmarshal(b, &r)
		return code, r
	}
	// no ttl, or zero, is the default and never "forever"
	code, r := create("default", 0)
	if code != 201 || r.Info.Expires == nil {
		t.Fatalf("default ttl: %d %+v", code, r)
	}
	if d := time.Until(*r.Info.Expires); d < auth.DefaultTokenTTL-time.Hour || d > auth.DefaultTokenTTL+time.Hour {
		t.Fatalf("default expiry in %v, want about %v", d, auth.DefaultTokenTTL)
	}
	if code, _ := create("negative", -5); code != 400 {
		t.Fatalf("negative ttl: %d", code)
	}
	if code, r = create("max", 365*24); code != 201 || r.Info.Expires == nil {
		t.Fatalf("exactly the maximum: %d", code)
	}
	if code, _ = create("toolong", 365*24+1); code != 400 {
		t.Fatalf("more than the maximum: %d", code)
	}
	if code, _ = create("forever", 1e9); code != 400 {
		t.Fatalf("absurd ttl: %d", code)
	}
}

func TestTOTPEnrolmentNeedsReauthentication(t *testing.T) {
	w, clock := reauthWeb(t, "anna")
	clock.Add(int64(auth.ReauthWindow + time.Minute))
	code, b := w.req("POST", "/api/v1/me/totp", nil, w.h())
	if code != 400 || !strings.Contains(string(b), `"password_required":true`) {
		t.Fatalf("begin without password on an old session: %d %s", code, b)
	}
	if u, _ := w.auth.User(context.Background(), "anna"); u.TOTP {
		t.Fatal("two-factor must not be on")
	}
	code, b = w.req("POST", "/api/v1/me/totp", map[string]any{"password": pw}, w.h())
	if code != 200 {
		t.Fatalf("begin with the password: %d %s", code, b)
	}
	code, b = w.req("POST", "/api/v1/me/totp/confirm", map[string]any{"code": "000000"}, w.h())
	if code != 400 || !strings.Contains(string(b), `"password_required":true`) {
		t.Fatalf("confirm without password on an old session: %d %s", code, b)
	}
	// with the password the request reaches the code check (which refuses a wrong code with 400, no password_required)
	code, b = w.req("POST", "/api/v1/me/totp/confirm", map[string]any{"code": "000000", "password": pw}, w.h())
	if code != 400 || strings.Contains(string(b), "password_required") {
		t.Fatalf("confirm with the password reaches the code check: %d %s", code, b)
	}
}

// A user with no local password (single sign-on only) gets no API tokens and no TOTP: a local key would
// bypass the identity provider and keep the last role it mapped (docs/authentication.md).
func TestSSOOnlyUserGetsNoLocalKeys(t *testing.T) {
	w, _ := reauthWeb(t, "sso")
	if _, err := w.node.Svc.DB.Pool().Exec(context.Background(), `UPDATE users SET password_hash='' WHERE name='sso'`); err != nil {
		t.Fatal(err)
	}
	// the session is fresh, so only the SSO rule can refuse
	if code, b := w.req("POST", "/api/v1/me/tokens", map[string]any{"name": "ci", "role": "readonly"}, w.h()); code != 400 || !strings.Contains(string(b), "single sign-on") {
		t.Fatalf("token for an SSO-only user: %d %s", code, b)
	}
	if code, b := w.req("POST", "/api/v1/me/totp", nil, w.h()); code != 400 || !strings.Contains(string(b), "single sign-on") {
		t.Fatalf("totp for an SSO-only user: %d %s", code, b)
	}
	if l, err := w.auth.Tokens(context.Background(), "sso"); err != nil || len(l) != 0 {
		t.Fatalf("tokens: %v %v", l, err)
	}
}
