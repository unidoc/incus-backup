package state

import (
	"testing"
	"time"
)

func TestPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.Update(func(st *State) {
		st.Objects["p/block/vm"] = &ObjectState{LastRun: now, LastStatus: "ok", LastSnap: "x"}
		st.Runs["nightly"] = now
	}); err != nil {
		t.Fatal(err)
	}
	r, err := Load(dir)
	if err != nil || r.Object("p/block/vm").LastSnap != "x" || !r.Runs["nightly"].Equal(now) {
		t.Fatalf("%v %+v", err, r)
	}
	if r.Object("unknown").LastStatus != "" {
		t.Fatal("unknown object must be empty")
	}
}
