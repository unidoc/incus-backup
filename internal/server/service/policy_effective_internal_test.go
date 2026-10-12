package service

import (
	"math/rand"
	"testing"

	"github.com/unidoc/incus-backup/internal/api"
)

// explainPolicy must pick exactly what ResolvePolicy picks (it only adds the reason).
func TestExplainPolicyAgreesWithResolvePolicy(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	vals := []string{"", "a", "b"}
	for i := 0; i < 20000; i++ {
		var as []api.Assignment
		for j := r.Intn(5); j > 0; j-- {
			as = append(as, api.Assignment{Project: vals[r.Intn(3)], Object: vals[r.Intn(3)], Policy: []string{"p1", "p2", "p3"}[r.Intn(3)]})
		}
		proj, obj := vals[1+r.Intn(2)], vals[1+r.Intn(2)]
		a, rank := explainPolicy(as, proj, obj)
		got := ""
		if rank >= 0 {
			got = a.Policy
		}
		if want := ResolvePolicy(as, proj, obj); got != want {
			t.Fatalf("%+v %s/%s: explain %q (rank %d), resolve %q", as, proj, obj, got, rank, want)
		}
	}
}
