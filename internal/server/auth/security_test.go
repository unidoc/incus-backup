package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"

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
