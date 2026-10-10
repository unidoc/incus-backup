package mgmt_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/mgmt"
)

// The OpenAPI document covers every route with its role, and every $ref resolves. (An external
// validator run is described in docs/api.md; this test needs no network.)
func TestOpenAPIDocument(t *testing.T) {
	raw := mgmt.OpenAPI("test")
	var doc struct {
		OpenAPI    string                                `json:"openapi"`
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi %q", doc.OpenAPI)
	}
	roles := map[auth.Perm]string{auth.PermRead: "readonly", auth.PermRestore: "restore", auth.PermOperate: "operator", auth.PermAdmin: "admin"}
	ops := 0
	for pat, perm := range mgmt.Perms() {
		method, path, _ := strings.Cut(pat, " ")
		var op struct {
			Summary   string                      `json:"summary"`
			Role      string                      `json:"x-role"`
			Responses map[string]json.RawMessage  `json:"responses"`
			Params    []struct{ Name, In string } `json:"parameters"`
		}
		b, ok := doc.Paths[path][strings.ToLower(method)]
		if !ok {
			t.Errorf("%s missing from the document", pat)
			continue
		}
		ops++
		if err := json.Unmarshal(b, &op); err != nil {
			t.Fatal(err)
		}
		if op.Summary == "" || op.Role != roles[perm] || len(op.Responses) < 3 {
			t.Errorf("%s: summary %q role %q (want %s), %d responses", pat, op.Summary, op.Role, roles[perm], len(op.Responses))
		}
		for _, m := range regexp.MustCompile(`\{([a-z_]+)\}`).FindAllStringSubmatch(path, -1) {
			found := false
			for _, p := range op.Params {
				found = found || (p.In == "path" && p.Name == m[1])
			}
			if !found {
				t.Errorf("%s: path parameter %s undocumented", pat, m[1])
			}
		}
	}
	if ops != len(mgmt.Perms()) {
		t.Fatalf("%d operations for %d routes", ops, len(mgmt.Perms()))
	}
	for _, m := range regexp.MustCompile(`"\$ref": "#/components/schemas/([^"]+)"`).FindAllStringSubmatch(string(raw), -1) {
		if _, ok := doc.Components.Schemas[m[1]]; !ok {
			t.Errorf("dangling $ref %s", m[1])
		}
	}
	// spot checks of the reflection: pointer -> nullable, omitempty -> optional, embedded inline, time format
	var job struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	_ = json.Unmarshal(doc.Components.Schemas["jobs.Job"], &job)
	if job.Properties["created_at"]["format"] != "date-time" || job.Properties["id"]["type"] != "string" {
		t.Fatalf("jobs.Job: %v", job.Properties)
	}
	if st, _ := json.Marshal(job.Properties["started_at"]["type"]); string(st) != `["string","null"]` {
		t.Fatalf("pointer time not nullable: %s", st)
	}
	for _, r := range job.Required {
		if r == "result" || r == "started_at" {
			t.Fatalf("%s is optional (omitempty)", r)
		}
	}
	var m struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(doc.Components.Schemas["mgmt.docMaintenance"], &m)
	if m.Properties["gc_grace_hours"] == nil || len(m.Properties) < 4 {
		t.Fatalf("embedded pointer struct not inlined: %v", m.Properties)
	}
}
