package httpapi

import (
	"testing"
	"time"
)

func TestLimiterRefillsAndIsolatesAddresses(t *testing.T) {
	l := newLimiter(60, 3) // 1 token per second, burst 3
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.allow("1.1.1.1") {
			t.Fatalf("burst call %d refused", i)
		}
	}
	if l.allow("1.1.1.1") {
		t.Fatal("fourth call must be refused")
	}
	if !l.allow("2.2.2.2") {
		t.Fatal("another address must have its own bucket")
	}
	now = now.Add(2 * time.Second)
	if !l.allow("1.1.1.1") || !l.allow("1.1.1.1") || l.allow("1.1.1.1") {
		t.Fatal("two tokens must have refilled after two seconds")
	}
}
