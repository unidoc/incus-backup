package incusx

import (
	"os"
	"testing"
)

func TestMountOwnerAndLiveness(t *testing.T) {
	if pid, ok := mountOwner("snap-1234-abcd"); !ok || pid != 1234 {
		t.Fatalf("%d %v", pid, ok)
	}
	for _, bad := range []string{"snap-", "snap-x-1", "other-12-x", "snap-12"} {
		if _, ok := mountOwner(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
	if !processAlive(os.Getpid()) {
		t.Fatal("own process reported dead")
	}
	if processAlive(1 << 30) {
		t.Fatal("a pid that cannot exist reported alive")
	}
}
