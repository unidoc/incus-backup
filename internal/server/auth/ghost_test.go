package auth

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The ghost lockout map holds a fixed-size key per name, whatever the name's length (review: a 1 MiB name
// per wrong login grew it by a megabyte each).
func TestGhostLocksKeyIsFixedSize(t *testing.T) {
	var g ghostLocks
	now := time.Now()
	big := strings.Repeat("a", 1<<20)
	g.fail(big, now)
	g.fail(strings.ToUpper(big), now) // same name, other case: one entry
	if len(g.m) != 1 {
		t.Fatalf("entries: %d, want 1", len(g.m))
	}
	for k := range g.m {
		if len(k) != 32 {
			t.Fatalf("key is %d bytes", len(k))
		}
	}
}

// Entries expire, and a full map evicts instead of leaving new names untracked, so an unknown name still
// reaches its fifth failure (and the lock) when ten thousand others are live.
func TestGhostLocksExpireAndEvict(t *testing.T) {
	var g ghostLocks
	now := time.Now()
	g.fail("old", now)
	if g.locked("old", now.Add(ghostTTL+time.Second)) || len(g.m) != 0 {
		t.Fatalf("expired entry kept: %d", len(g.m))
	}
	for i := 0; i < maxGhostLocks; i++ {
		g.fail(fmt.Sprintf("n%d", i), now.Add(time.Duration(i)*time.Millisecond))
	}
	if len(g.m) != maxGhostLocks {
		t.Fatalf("entries: %d", len(g.m))
	}
	later := now.Add(time.Minute)
	for i := 0; i < 5; i++ {
		g.fail("newcomer", later)
	}
	if !g.locked("newcomer", later) {
		t.Fatal("a new name is not tracked while the map is full of live entries")
	}
	if len(g.m) > maxGhostLocks {
		t.Fatalf("map grew to %d", len(g.m))
	}
	// everything expires: a later failure sweeps the dead entries
	g.fail("fresh", later.Add(2*ghostTTL))
	if len(g.m) != 1 {
		t.Fatalf("dead entries kept: %d", len(g.m))
	}
}
