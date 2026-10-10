package mgmt_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/server/mgmt"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

// The documented request bodies are mirrors of the handlers' inline structs. The handlers reject
// unknown fields, so sending every documented body (all fields present) to the real handler proves no
// documented field is one the server would refuse. (A field the server accepts but the mirror lacks is
// not caught.)
func TestDocumentedRequestBodiesAreAccepted(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	checked := 0
	for pat, body := range mgmt.RouteBodies() {
		method, path, _ := strings.Cut(pat, " ")
		path = strings.NewReplacer("{name}", "nobody-x", "{scope}", "backup", "{action}", "enable", "{group}", "0123456789abcdef0123456789abcdef",
			"{snap}", "s", "{id}", "1", "{type}", "stale_backup").Replace(path)
		b, _ := json.Marshal(body)
		var m any
		_ = json.Unmarshal(b, &m)
		code, resp := c.do(method, path+"?repository=custody", m, nil)
		if strings.Contains(resp, "unknown field") {
			t.Errorf("%s: the documented body has a field the handler refuses (%d %s)", pat, code, resp)
		}
		checked++
	}
	if checked < 40 {
		t.Fatalf("only %d bodies checked", checked)
	}
	t.Logf("%d documented request bodies accepted by their handlers", checked)
}
