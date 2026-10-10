package auth_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/db"
	"github.com/unidoc/incus-backup/internal/testdb"
)

func TestMain(m *testing.M) {
	auth.Argon2Memory, auth.Argon2Time = 1024, 1 // cheap in tests; production keeps the real cost
	testdb.Main(m)
}

type sealer struct{ key []byte }

func (s sealer) Seal(p []byte) ([]byte, error) {
	a, _ := chacha20poly1305.NewX(s.key)
	n := make([]byte, a.NonceSize())
	_, _ = rand.Read(n)
	return a.Seal(n, n, p, nil), nil
}
func (s sealer) Open(c []byte) ([]byte, error) {
	a, _ := chacha20poly1305.NewX(s.key)
	return a.Open(nil, c[:a.NonceSize()], c[a.NonceSize():], nil)
}

type env struct {
	*auth.Store
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	e := &env{Store: auth.New(d.Pool(), sealer{k}), now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	e.Now = func() time.Time { return e.now }
	return e
}

const pw = "correct horse battery"

func TestRoles(t *testing.T) {
	cases := []struct {
		r    auth.Role
		p    auth.Perm
		want bool
	}{
		{auth.ReadOnly, auth.PermRead, true}, {auth.ReadOnly, auth.PermRestore, false},
		{auth.Restore, auth.PermRestore, true}, {auth.Restore, auth.PermOperate, false},
		{auth.Operator, auth.PermOperate, true}, {auth.Operator, auth.PermAdmin, false},
		{auth.Admin, auth.PermAdmin, true}, {auth.Role("root"), auth.PermRead, false},
	}
	for _, c := range cases {
		if got := c.r.Allows(c.p); got != c.want {
			t.Errorf("%s allows %d = %v, want %v", c.r, c.p, got, c.want)
		}
	}
}

func TestLoginSessionAndExpiry(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.CreateUser(ctx, "anna", "short", auth.Admin, "", "", false); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password accepted: %v", err)
	}
	if _, err := e.CreateUser(ctx, "anna", pw, auth.Admin, "Anna", "a@x", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateUser(ctx, "ANNA", pw, auth.Admin, "", "", false); !errors.Is(err, auth.ErrExists) {
		t.Fatalf("duplicate (case-insensitive) accepted: %v", err)
	}
	if _, err := e.Login(ctx, "anna", "wrong password!!", "", "1.2.3.4", "ua"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := e.Login(ctx, "nobody", pw, "", "", ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown user must look like a wrong password: %v", err)
	}
	s, err := e.Login(ctx, "Anna", pw, "", "1.2.3.4", "ua")
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.Authenticate(ctx, s.Token)
	if err != nil || p.Role != auth.Admin || p.CSRF != s.CSRF || p.User != "anna" {
		t.Fatalf("principal %+v err=%v", p, err)
	}
	// idle expiry
	e.now = e.now.Add(auth.IdleTimeout - time.Minute)
	if _, err := e.Authenticate(ctx, s.Token); err != nil {
		t.Fatalf("inside the idle window: %v", err)
	}
	e.now = e.now.Add(auth.IdleTimeout + time.Minute)
	if _, err := e.Authenticate(ctx, s.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("idle session still valid: %v", err)
	}
	// absolute expiry although activity keeps the idle window open
	s, _ = e.Login(ctx, "anna", pw, "", "", "")
	for i := 0; i < 30; i++ {
		e.now = e.now.Add(25 * time.Minute)
		if _, err := e.Authenticate(ctx, s.Token); err != nil {
			if e.now.Sub(s.Expires) < 0 {
				t.Fatalf("session ended early at %v: %v", e.now, err)
			}
			return
		}
	}
	t.Fatal("session outlived its absolute lifetime")
}

func TestLockoutAndRecovery(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, _ = e.CreateUser(ctx, "bo", pw, auth.Operator, "", "", false)
	for i := 0; i < 5; i++ {
		_, _ = e.Login(ctx, "bo", "nope nope nope", "", "", "")
	}
	if _, err := e.Login(ctx, "bo", pw, "", "", ""); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("right password during lockout must still be refused: %v", err)
	}
	e.now = e.now.Add(2 * time.Minute)
	if _, err := e.Login(ctx, "bo", pw, "", "", ""); err != nil {
		t.Fatalf("after the lock: %v", err)
	}
}

func TestTOTPAndRecoveryCodes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, _ = e.CreateUser(ctx, "cy", pw, auth.Admin, "", "", false)
	en, err := e.BeginTOTP(ctx, "cy")
	if err != nil || !strings.HasPrefix(en.URI, "otpauth://totp/") {
		t.Fatalf("%+v %v", en, err)
	}
	if _, err := e.ConfirmTOTP(ctx, "cy", "000000"); err == nil {
		t.Fatal("wrong confirmation code accepted")
	}
	code := auth.TOTPForTest(en.Secret, e.now)
	rec, err := e.ConfirmTOTP(ctx, "cy", code)
	if err != nil || len(rec) != 10 {
		t.Fatalf("recovery %v err=%v", rec, err)
	}
	if _, err := e.Login(ctx, "cy", pw, "", "", ""); !errors.Is(err, auth.ErrNeedTOTP) {
		t.Fatalf("2FA not demanded: %v", err)
	}
	// the code used to confirm cannot be replayed; the next step works
	if _, err := e.Login(ctx, "cy", pw, code, "", ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("replayed code accepted: %v", err)
	}
	e.now = e.now.Add(35 * time.Second)
	if _, err := e.Login(ctx, "cy", pw, auth.TOTPForTest(en.Secret, e.now), "", ""); err != nil {
		t.Fatalf("valid code: %v", err)
	}
	// a recovery code works exactly once
	if _, err := e.Login(ctx, "cy", pw, rec[0], "", ""); err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	if _, err := e.Login(ctx, "cy", pw, rec[0], "", ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("recovery code reused: %v", err)
	}
	if err := e.DisableTOTP(ctx, "cy"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Login(ctx, "cy", pw, "", "", ""); err != nil {
		t.Fatalf("after disabling 2FA: %v", err)
	}
}

func TestTokensAreCappedByOwnerRole(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, _ = e.CreateUser(ctx, "root", pw, auth.Admin, "", "", false)
	_, _ = e.CreateUser(ctx, "dan", pw, auth.Operator, "", "", false)
	if _, _, err := e.CreateToken(ctx, "dan", "t", auth.Admin, 0); err == nil {
		t.Fatal("operator minted an admin token")
	}
	secret, _, err := e.CreateToken(ctx, "dan", "ci", auth.Operator, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.AuthenticateToken(ctx, secret)
	if err != nil || p.Role != auth.Operator || p.Kind != "token" {
		t.Fatalf("%+v %v", p, err)
	}
	ro := auth.ReadOnly
	if err := e.UpdateUser(ctx, "dan", &ro, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.AuthenticateToken(ctx, secret); p == nil || p.Role != auth.ReadOnly {
		t.Fatalf("demotion must cap the token: %+v", p)
	}
	tr := true
	_ = e.UpdateUser(ctx, "dan", nil, nil, nil, &tr)
	if _, err := e.AuthenticateToken(ctx, secret); err == nil {
		t.Fatal("token of a disabled user works")
	}
	fa := false
	_ = e.UpdateUser(ctx, "dan", nil, nil, nil, &fa)
	e.now = e.now.Add(2 * time.Hour)
	if _, err := e.AuthenticateToken(ctx, secret); err == nil {
		t.Fatal("expired token works")
	}
	if _, err := e.AuthenticateToken(ctx, "ibt_garbage"); err == nil {
		t.Fatal("garbage token works")
	}
}

func TestLastAdminCannotBeRemoved(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, _ = e.CreateUser(ctx, "one", pw, auth.Admin, "", "", false)
	ro, tr := auth.ReadOnly, true
	if err := e.UpdateUser(ctx, "one", &ro, nil, nil, nil); err == nil {
		t.Fatal("demoted the last admin")
	}
	if err := e.UpdateUser(ctx, "one", nil, nil, nil, &tr); err == nil {
		t.Fatal("disabled the last admin")
	}
	if err := e.DeleteUser(ctx, "one"); err == nil {
		t.Fatal("deleted the last admin")
	}
	_, _ = e.CreateUser(ctx, "two", pw, auth.Admin, "", "", false)
	if err := e.DeleteUser(ctx, "one"); err != nil {
		t.Fatal(err)
	}
}

func TestSetupTokenWorksOnce(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tok, err := e.SetupToken(ctx)
	if err != nil || tok == "" {
		t.Fatalf("%q %v", tok, err)
	}
	if _, err := e.CompleteSetup(ctx, "wrong", "admin", pw); !errors.Is(err, auth.ErrSetup) {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := e.CompleteSetup(ctx, tok, "admin", pw); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CompleteSetup(ctx, tok, "again", pw); !errors.Is(err, auth.ErrSetup) {
		t.Fatalf("setup ran twice: %v", err)
	}
	if again, _ := e.SetupToken(ctx); again != "" {
		t.Fatal("setup token offered although users exist")
	}
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, _ = e.CreateUser(ctx, "eva", pw, auth.Restore, "", "", false)
	a, _ := e.Login(ctx, "eva", pw, "", "", "")
	b, _ := e.Login(ctx, "eva", pw, "", "", "")
	if err := e.ChangeOwnPassword(ctx, "eva", "wrong wrong wrong", "another long password", a.Token); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("%v", err)
	}
	if err := e.ChangeOwnPassword(ctx, "eva", pw, "another long password", a.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Authenticate(ctx, a.Token); err != nil {
		t.Fatalf("own session must survive: %v", err)
	}
	if _, err := e.Authenticate(ctx, b.Token); err == nil {
		t.Fatal("other session survived a password change")
	}
}

func TestAudit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for _, a := range []string{"login", "repository.create", "repository.delete"} {
		if err := e.Audit(ctx, auth.AuditEntry{Actor: "anna", Action: a, Target: "r", OK: true}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := e.ListAudit(ctx, auth.AuditFilter{Action: "repository."})
	if err != nil || len(l) != 2 || l[0].Action != "repository.delete" {
		t.Fatalf("%+v %v", l, err)
	}
}
