package mgmt_test

import (
	"os"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/server/mgmt"
)

const (
	routesBegin = "<!-- routes:begin (generated: IB_WRITE_API_DOC=1 go test ./internal/server/mgmt -run APIDoc) -->\n"
	routesEnd   = "<!-- routes:end -->"
)

// docs/api.md carries the full route table generated from the route metadata, so the prose document
// cannot silently miss a route (the audit found 15 missing ones).
func TestAPIDocListsEveryRoute(t *testing.T) {
	const path = "../../../docs/api.md"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	i, j := strings.Index(doc, routesBegin), strings.Index(doc, routesEnd)
	if i < 0 || j < i {
		t.Fatalf("docs/api.md has no generated route table (markers %q ... %q)", routesBegin, routesEnd)
	}
	want := mgmt.RoutesMarkdown()
	got := doc[i+len(routesBegin) : j]
	if got == want {
		return
	}
	if os.Getenv("IB_WRITE_API_DOC") != "" {
		if err := os.WriteFile(path, []byte(doc[:i+len(routesBegin)]+want+doc[j:]), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("docs/api.md route table is stale: run IB_WRITE_API_DOC=1 go test ./internal/server/mgmt -run APIDoc")
}
