package mgmt_test

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/server/traffic"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

// End to end: a rule created through the management API throttles a real host's chunk uploads over the
// mTLS agent API, and /traffic/current reports the host's throughput.
func TestTrafficRuleThrottlesRealUploads(t *testing.T) {
	n := testsupport.NewNode(t)
	c, _ := newClient(t, n)
	var rule traffic.Rule
	c.must("POST", "/api/v1/traffic/rules", map[string]any{"name": "benfica-up", "scope": "host", "hosts": "benfica", "direction": "upload",
		"rate_bps": 256 << 10, "burst_bytes": 64 << 10}, &rule)
	if !rule.Enabled || rule.Timezone != "UTC" || rule.Hosts != "benfica" {
		t.Fatalf("rule: %+v", rule)
	}
	if code, body := c.do("POST", "/api/v1/traffic/rules", map[string]any{"name": "bad", "scope": "host", "direction": "upload", "rate_bps": 10}, nil); code != 400 {
		t.Fatalf("bad rule: %d %s", code, body)
	}
	if code, _ := c.do("POST", "/api/v1/traffic/rules", map[string]any{"name": "benfica-up", "scope": "host", "direction": "upload", "rate_bps": 1 << 20}, nil); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	part := func() string {
		b := make([]byte, 512<<10)
		_, _ = rand.Read(b)
		return string(b)
	}
	start := time.Now()
	backup(t, n, masterKeys(), "20261007120000-00000001", n.Now, part(), part()) // 1 MiB of incompressible chunks
	took := time.Since(start)
	want := time.Duration(float64(1<<20-64<<10) / float64(256<<10) * float64(time.Second))
	if took < want*80/100 || took > want*130/100 {
		t.Fatalf("1 MiB backup at 256 KiB/s took %v, want about %v", took, want)
	}
	t.Logf("1 MiB backup at 256 KiB/s took %v (expected %v)", took.Round(time.Millisecond), want)
	var cur struct {
		Hosts []traffic.HostRate
		Rules []traffic.ActiveRule
	}
	c.must("GET", "/api/v1/traffic/current", nil, &cur)
	if len(cur.Hosts) != 1 || cur.Hosts[0].Host != "benfica" || cur.Hosts[0].Repository != "custody" || cur.Hosts[0].UploadTotal < 1<<20 {
		t.Fatalf("current: %+v", cur)
	}
	if len(cur.Rules) != 1 || !cur.Rules[0].Active {
		t.Fatalf("rules: %+v", cur.Rules)
	}
	// delete: the next backup is not throttled
	c.must("DELETE", "/api/v1/traffic/rules/benfica-up", nil, nil)
	start = time.Now()
	backup(t, n, masterKeys(), "20261007130000-00000002", n.Now.Add(time.Hour), part(), part())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("unthrottled backup took %v", d)
	}
	if code, _ := c.do("DELETE", "/api/v1/traffic/rules/benfica-up", nil, nil); code != 404 {
		t.Fatalf("second delete: %d", code)
	}
}
