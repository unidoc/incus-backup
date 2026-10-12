package auth_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/server/auth"
)

// Argon2id allocates Argon2Memory per computation: concurrent password checks must be bounded or a burst of
// requests (any signed-in user can send POST /me/password) exhausts the server's memory.
func TestArgon2ConcurrencyIsBounded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.CreateUser(ctx, "eve", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	auth.ArgonPeakForTest()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.Login(ctx, "nobody", "wrong password here", "", "", "") // the dummy-hash path
		}()
	}
	wg.Wait()
	peak, slots := auth.ArgonPeakForTest(), auth.ArgonSlotsForTest()
	if peak < 1 || peak > int64(slots) {
		t.Fatalf("%d Argon2id computations ran at once, bound is %d", peak, slots)
	}
}

// A wrong current password (change password, disable TOTP) counts like a failed login: a stolen session must
// not be an unthrottled password oracle.
func TestPasswordRecheckFailuresLockTheAccount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.CreateUser(ctx, "eve", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := e.ChangeOwnPassword(ctx, "eve", "guess number one", "a new password 123", ""); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	if err := e.VerifyPassword(ctx, "eve", pw); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("after 5 wrong guesses the right password must be refused while locked: %v", err)
	}
	if _, err := e.Login(ctx, "eve", pw, "", "", ""); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("login while locked: %v", err)
	}
	e.now = e.now.Add(auth.IdleTimeout) // well past the first lock (1 minute)
	if err := e.VerifyPassword(ctx, "eve", pw); err != nil {
		t.Fatalf("after the lock: %v", err)
	}
	if _, err := e.Login(ctx, "eve", pw, "", "", ""); err != nil {
		t.Fatalf("login after the lock: %v", err)
	}
}

// Lockout must not tell whether a name exists (review F11): an unknown name and a single sign-on account
// answer 401 five times and then 429, like a real account, and unlock on the same schedule.
func TestLockoutIsTheSameForUnknownNames(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.CreateUser(ctx, "real", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateUser(ctx, "sso", pw, auth.ReadOnly, "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Exec(ctx, `UPDATE users SET password_hash='' WHERE name='sso'`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"real", "nobody", "NoBody2", "sso"} {
		for i := 0; i < 5; i++ {
			if _, err := e.Login(ctx, name, "wrong guess number "+name, "", "", ""); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatalf("%s guess %d: %v", name, i, err)
			}
		}
		if _, err := e.Login(ctx, name, "wrong guess number "+name, "", "", ""); !errors.Is(err, auth.ErrLocked) {
			t.Fatalf("%s after five failures: %v, want ErrLocked", name, err)
		}
		if _, err := e.Login(ctx, strings.ToUpper(name), pw, "", "", ""); !errors.Is(err, auth.ErrLocked) {
			t.Fatalf("%s (other case) while locked: %v", name, err)
		}
	}
	e.now = e.now.Add(auth.IdleTimeout) // past the first lock
	if _, err := e.Login(ctx, "nobody", "wrong", "", "", ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("an unknown name unlocks after the interval: %v", err)
	}
	if _, err := e.Login(ctx, "real", pw, "", "", ""); err != nil {
		t.Fatalf("the real account unlocks: %v", err)
	}
}

// API tokens always expire, never outlive MaxTokenTTL, and a user without a local password gets none
// (review F8).
func TestTokenLifetimeAndSSOOnlyRule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.CreateUser(ctx, "anna", pw, auth.Admin, "", "", false); err != nil {
		t.Fatal(err)
	}
	_, ti, err := e.CreateToken(ctx, "anna", "default", auth.ReadOnly, 0)
	if err != nil || ti.Expires == nil || !ti.Expires.Equal(e.now.Add(auth.DefaultTokenTTL)) {
		t.Fatalf("zero ttl: %v %+v", err, ti)
	}
	if _, ti, err = e.CreateToken(ctx, "anna", "max", auth.ReadOnly, auth.MaxTokenTTL); err != nil || ti.Expires == nil {
		t.Fatalf("max ttl: %v", err)
	}
	if _, _, err = e.CreateToken(ctx, "anna", "long", auth.ReadOnly, auth.MaxTokenTTL+time.Second); !errors.Is(err, auth.ErrBad) {
		t.Fatalf("over the maximum: %v", err)
	}
	if _, err := e.Pool.Exec(ctx, `UPDATE users SET password_hash='' WHERE name='anna'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = e.CreateToken(ctx, "anna", "sso", auth.ReadOnly, time.Hour); !errors.Is(err, auth.ErrBad) {
		t.Fatalf("SSO-only user: %v", err)
	}
	if _, err := e.BeginTOTP(ctx, "anna"); !errors.Is(err, auth.ErrBad) {
		t.Fatalf("SSO-only user TOTP: %v", err)
	}
}
