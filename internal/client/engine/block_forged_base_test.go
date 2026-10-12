package engine_test

// A datastore writer that swaps the stored base index of a block incremental must not decide which
// chunk ids the next snapshot copies: planDisk opens the base manifest and compares IndexDigest
// (as Locate does), and without a trusted base reads the whole disk.

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/format"
)

func forgedBaseRun(t *testing.T, forge bool) string {
	w := newWorld(t, "cikarang")
	w.policy("prod", []string{"cikarang"}, "all")
	setupVM(w, 1)
	root := find(w.objects(), "instance/bosch/root")
	snap1, rs := w.backupVM("bosch")
	mustOK(t, rs)

	n := w.nodes["cikarang"]
	st, err := n.Svc.Store(n.Repo)
	if err != nil {
		t.Fatal(err)
	}
	key := "groups/" + w.keys.GroupID(root.Project, root.Kind, root.Name) + "/" + snap1 + "/index"
	genuine, err := st.Backend().Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if forge { // genuine ids of this key domain, rotated by one position; trailer recomputed (no key needed)
		idx, _ := format.ParseIndex(genuine)
		f := format.NewFixedIndex(idx.Size, idx.ChunkSize)
		for i := range f.Entries {
			f.Entries[i].ID = idx.Entries[(i+1)%len(idx.Entries)].ID
		}
		fb, _ := f.Marshal()
		if err := st.Backend().Put(key, fb, false); err != nil {
			t.Fatal(err)
		}
	}

	w.now = w.now.Add(time.Hour)
	w.inc.GuestWrite("default", "bosch", "root", 5*MiB, rnd(100000, 2)) // dirties position 1 only
	snap2, rs := w.backupVM("bosch")
	mustOK(t, rs)
	full := rs[0].Full

	// the forger puts the genuine index back: nothing on the server is visibly wrong any more
	if err := st.Backend().Put(key, genuine, false); err != nil {
		t.Fatal(err)
	}
	tg := w.eng.Targets["cikarang"]
	locate := "ok"
	if _, err := w.eng.Locate(context.Background(), tg, root.Project, root.Name, snap2); err != nil {
		locate = "refused"
	}
	equal := bytes.Equal(w.fetch("cikarang", root, snap2), w.inc.Disk("default", "bosch", "root"))
	rep, err := w.eng.Verify(context.Background(), tg, 0)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("full=%v | locate=%s | snap2EqualsDisk=%v | verifyProblems=%d", full, locate, equal, len(rep.Problems))
}

func TestForgedBaseControlNoForgery(t *testing.T) {
	got := forgedBaseRun(t, false)
	if want := "full=false | locate=ok | snap2EqualsDisk=true | verifyProblems=0"; got != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}

func TestForgedBaseIndexForcesFullRead(t *testing.T) {
	got := forgedBaseRun(t, true)
	// safe outcome: a base index that does not match its manifest is "doubt", so the run reads in full
	if want := "full=true | locate=ok | snap2EqualsDisk=true | verifyProblems=0"; got != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}
