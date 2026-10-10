// Package metrics writes the Prometheus text exposition format (version 0.0.4) without any external
// dependency, and counts the management API's HTTP requests (counter + latency histogram per route
// pattern, method and status).
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Type of a metric family.
type Type string

// Family types.
const (
	Gauge     Type = "gauge"
	Counter   Type = "counter"
	Histogram Type = "histogram"
)

// Labels are a sample's labels (written sorted by name).
type Labels map[string]string

// Sample is one value of a family. For a histogram family, Name is the full sample name
// (family_bucket, family_sum, family_count).
type Sample struct {
	Suffix string // "", "_bucket", "_sum", "_count"
	Labels Labels
	Value  float64
}

// Family is a named metric with its samples.
type Family struct {
	Name    string
	Help    string
	Type    Type
	Samples []Sample
}

// Add appends a sample.
func (f *Family) Add(v float64, kv ...string) {
	f.Samples = append(f.Samples, Sample{Labels: pairs(kv), Value: v})
}

func pairs(kv []string) Labels {
	if len(kv) == 0 {
		return nil
	}
	l := Labels{}
	for i := 0; i+1 < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return l
}

// Set is an ordered collection of families (each name once).
type Set struct {
	fams  []*Family
	index map[string]*Family
}

// F returns the family name, creating it on first use.
func (s *Set) F(name, help string, t Type) *Family {
	if s.index == nil {
		s.index = map[string]*Family{}
	}
	if f, ok := s.index[name]; ok {
		return f
	}
	f := &Family{Name: name, Help: help, Type: t}
	s.index[name] = f
	s.fams = append(s.fams, f)
	return f
}

// Gauge sets a single unlabelled gauge.
func (s *Set) Gauge(name, help string, v float64, kv ...string) {
	s.F(name, help, Gauge).Add(v, kv...)
}

// EscapeLabel escapes a label value: backslash, double quote and newline.
func EscapeLabel(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func escapeHelp(v string) string {
	return strings.NewReplacer("\\", `\\`, "\n", `\n`).Replace(v)
}

// FormatValue writes a float as Prometheus expects (+Inf, -Inf, NaN).
func FormatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func writeLabels(w io.Writer, l Labels) {
	if len(l) == 0 {
		return
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	io.WriteString(w, "{")
	for i, k := range keys {
		if i > 0 {
			io.WriteString(w, ",")
		}
		fmt.Fprintf(w, `%s="%s"`, k, EscapeLabel(l[k]))
	}
	io.WriteString(w, "}")
}

// Write renders the set. Families without samples are left out.
func (s *Set) Write(w io.Writer) error {
	for _, f := range s.fams {
		if len(f.Samples) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.Name, escapeHelp(f.Help), f.Name, f.Type); err != nil {
			return err
		}
		for _, sm := range f.Samples {
			io.WriteString(w, f.Name+sm.Suffix)
			writeLabels(w, sm.Labels)
			if _, err := fmt.Fprintf(w, " %s\n", FormatValue(sm.Value)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- HTTP request statistics -------------------------------------------------------------

// DefaultBuckets are the latency buckets (seconds) of the request histogram.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type reqKey struct {
	route, method, code string
}

type hist struct {
	counts []uint64 // per bucket (non-cumulative)
	sum    float64
	n      uint64
}

// HTTPStats counts requests per route pattern (never the raw path: bounded cardinality).
type HTTPStats struct {
	mu      sync.Mutex
	buckets []float64
	h       map[reqKey]*hist
}

// NewHTTPStats returns empty statistics.
func NewHTTPStats() *HTTPStats {
	return &HTTPStats{buckets: DefaultBuckets, h: map[reqKey]*hist{}}
}

// Observe records one request.
func (s *HTTPStats) Observe(route, method string, code int, d time.Duration) {
	k := reqKey{route, method, strconv.Itoa(code)}
	sec := d.Seconds()
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.h[k]
	if x == nil {
		x = &hist{counts: make([]uint64, len(s.buckets))}
		s.h[k] = x
	}
	for i, b := range s.buckets {
		if sec <= b {
			x.counts[i]++
			break
		}
	}
	x.sum += sec
	x.n++
}

// Collect adds the request counter and the latency histogram to set.
func (s *HTTPStats) Collect(set *Set, prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]reqKey, 0, len(s.h))
	for k := range s.h {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.code < b.code
	})
	cnt := set.F(prefix+"http_requests_total", "Management API requests by route pattern, method and status code.", Counter)
	hf := set.F(prefix+"http_request_duration_seconds", "Management API request latency by route pattern and method.", Histogram)
	type hk struct{ route, method string }
	merged := map[hk]*hist{}
	var order []hk
	for _, k := range keys {
		x := s.h[k]
		cnt.Add(float64(x.n), "route", k.route, "method", k.method, "code", k.code)
		m := merged[hk{k.route, k.method}]
		if m == nil {
			m = &hist{counts: make([]uint64, len(s.buckets))}
			merged[hk{k.route, k.method}] = m
			order = append(order, hk{k.route, k.method})
		}
		for i := range x.counts {
			m.counts[i] += x.counts[i]
		}
		m.sum += x.sum
		m.n += x.n
	}
	for _, k := range order {
		m := merged[k]
		var cum uint64
		for i, b := range s.buckets {
			cum += m.counts[i]
			hf.Samples = append(hf.Samples, Sample{Suffix: "_bucket", Labels: Labels{"route": k.route, "method": k.method, "le": FormatValue(b)}, Value: float64(cum)})
		}
		hf.Samples = append(hf.Samples,
			Sample{Suffix: "_bucket", Labels: Labels{"route": k.route, "method": k.method, "le": "+Inf"}, Value: float64(m.n)},
			Sample{Suffix: "_sum", Labels: Labels{"route": k.route, "method": k.method}, Value: m.sum},
			Sample{Suffix: "_count", Labels: Labels{"route": k.route, "method": k.method}, Value: float64(m.n)})
	}
}
