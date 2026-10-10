//go:build !js

package fscat

import (
	"errors"
	"fmt"
	"testing"
)

// The walk's look-ahead bound and the streamed run hand-over change nothing: with a tiny bound
// (listings park all the time and are released by the assembly) and runs delivered through OnRun,
// the catalog, the runs and the counts are exactly the sequential scanner's.
func TestBoundedWalkAndOnRunEqualSequential(t *testing.T) {
	root := equivTree(t)
	prev0, err := origScan(root, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prev, err := Decode(encodeOrFail(t, prev0.Tree))
	if err != nil {
		t.Fatal(err)
	}
	before := parkedTotal.Load()
	defer func() {
		if parkedTotal.Load() == before {
			t.Error("no listing was ever parked: the bound was not exercised")
		}
	}()
	for _, ahead := range []int{1, 7, 1000} {
		for _, w := range []int{1, 4, 16} {
			for _, o := range []ScanOptions{{}, {Prev: prev, TrustCtimeInode: true}, {SmallFile: 1000, PackMin: 2000, PackMax: 9000, PackDiv: 3}} {
				want, err := origScan(root, o)
				if err != nil {
					t.Fatal(err)
				}
				o.Workers, o.Ahead = w, ahead
				label := fmt.Sprintf("ahead=%d workers=%d prev=%v", ahead, w, o.Prev != nil)
				got, err := Scan(root, o)
				if err != nil {
					t.Fatal(err)
				}
				sameResult(t, label, want, got)
				var runs []Run
				o.OnRun = func(r *Run) error { runs = append(runs, *r); return nil }
				streamed, err := Scan(root, o)
				if err != nil {
					t.Fatal(err)
				}
				if len(streamed.Runs) != 0 {
					t.Fatalf("%s: runs were collected although OnRun is set", label)
				}
				streamed.Runs = runs
				sameResult(t, label+" onrun", want, streamed)
			}
		}
	}
}

// An error from OnRun ends the scan with that error.
func TestOnRunErrorEndsTheScan(t *testing.T) {
	root := equivTree(t)
	stop := errors.New("stop")
	n := 0
	_, err := Scan(root, ScanOptions{Ahead: 3, OnRun: func(*Run) error {
		n++
		if n == 2 {
			return stop
		}
		return nil
	}})
	if !errors.Is(err, stop) || n != 2 {
		t.Fatalf("err=%v after %d runs", err, n)
	}
}
