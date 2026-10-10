package metrics_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/server/metrics"
	"github.com/unidoc/incus-backup/internal/server/metrics/promtest"
)

func TestExpositionEscapesAndValidates(t *testing.T) {
	set := &metrics.Set{}
	f := set.F("x_info", "help with \\ and\nnewline", metrics.Gauge)
	f.Add(1, "name", "a \"quoted\" \\ back\nslash", "b", "plain")
	f.Add(2, "name", "other")
	set.Gauge("x_empty_family", "never written", 0) // one sample: written
	set.F("x_none", "no samples: left out", metrics.Gauge)
	st := metrics.NewHTTPStats()
	st.Observe("GET /api/v1/x", "GET", 200, 3*time.Millisecond)
	st.Observe("GET /api/v1/x", "GET", 200, 300*time.Millisecond)
	st.Observe("GET /api/v1/x", "GET", 404, time.Minute)
	st.Collect(set, "x_")
	var b bytes.Buffer
	if err := set.Write(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, `name="a \"quoted\" \\ back\nslash"`) || strings.Contains(out, "x_none") {
		t.Fatalf("exposition:\n%s", out)
	}
	fams, err := promtest.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := fams["x_info"].Samples[0].Labels["name"]; got != "a \"quoted\" \\ back\nslash" {
		t.Fatalf("round trip %q", got)
	}
	h := fams["x_http_request_duration_seconds"]
	if h == nil || h.Type != "histogram" {
		t.Fatal("no histogram")
	}
	var count, inf float64
	for _, s := range h.Samples {
		if s.Name == "x_http_request_duration_seconds_count" {
			count = s.Value
		}
		if s.Labels["le"] == "+Inf" {
			inf = s.Value
		}
		if s.Labels["le"] == "0.005" && s.Value != 1 {
			t.Fatalf("bucket 0.005 = %v", s.Value)
		}
	}
	if count != 3 || inf != 3 {
		t.Fatalf("count %v inf %v", count, inf)
	}
	c := fams["x_http_requests_total"]
	if len(c.Samples) != 2 {
		t.Fatalf("counter samples %+v", c.Samples)
	}
}

// The parser itself must reject what Prometheus rejects (negative controls).
func TestParserRejectsBrokenExpositions(t *testing.T) {
	bad := map[string]string{
		"split family":      "# HELP a x\n# TYPE a gauge\na 1\n# HELP b y\n# TYPE b gauge\nb 1\n# HELP a x\n# TYPE a gauge\na{x=\"1\"} 2\n",
		"double type":       "# TYPE a gauge\n# TYPE a gauge\na 1\n",
		"undeclared sample": "# TYPE a gauge\na 1\nb 2\n",
		"bad escape":        "# TYPE a gauge\na{x=\"\\q\"} 1\n",
		"unquoted":          "# TYPE a gauge\na{x=1} 1\n",
		"bad value":         "# TYPE a gauge\na one\n",
		"duplicate series":  "# TYPE a gauge\na{x=\"1\"} 1\na{x=\"1\"} 2\n",
		"non-cumulative":    "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"2\"} 3\nh_bucket{le=\"+Inf\"} 5\nh_sum 1\nh_count 5\n",
		"count differs":     "# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"+Inf\"} 2\nh_sum 1\nh_count 3\n",
		"no inf":            "# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_sum 1\nh_count 1\n",
	}
	for name, in := range bad {
		if _, err := promtest.Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := promtest.Parse(strings.NewReader("# HELP a x\n# TYPE a gauge\na{x=\"1\"} 1\na{x=\"2\"} +Inf\n")); err != nil {
		t.Fatal(err)
	}
}
