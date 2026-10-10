// Package memprobe measures the memory a phase of work needs: the peak Go heap in use (sampled,
// independent of GOMEMLIMIT and GOGC tuning of the limit) and the peak resident set size of the
// process (VmHWM after resetting it through /proc/self/clear_refs). The memory tests and
// scripts/membench.sh use it to prove that data paths are streamed with bounded buffers.
package memprobe

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Result is the memory used by one phase, relative to the state when it started.
type Result struct {
	Phase string
	// BaseHeap is HeapInuse right after a forced GC at the start; PeakHeap the highest sampled
	// HeapInuse during the phase. HeapDelta = PeakHeap - BaseHeap is what the phase added.
	BaseHeap, PeakHeap, HeapDelta uint64
	// BaseLive/PeakLive: the live heap as marked by the last completed GC cycle (what really is
	// retained, without the garbage the pacer lets accumulate). LiveDelta = PeakLive - BaseLive.
	BaseLive, PeakLive, LiveDelta uint64
	// BaseRSS is VmRSS at the start, PeakRSS the VmHWM at the end (the peak since the start, when
	// the kernel allowed resetting it; RSSReset says whether it did). RSSDelta = PeakRSS - BaseRSS.
	BaseRSS, PeakRSS, RSSDelta uint64
	RSSReset                   bool
	TotalAlloc                 uint64 // bytes allocated during the phase (GC churn)
	Duration                   time.Duration
}

// Probe samples one phase.
type Probe struct {
	phase      string
	start      time.Time
	baseHeap   uint64
	baseAlloc  uint64
	baseRSS    uint64
	reset      bool
	baseLive   uint64
	mu         sync.Mutex
	peak       uint64
	peakLive   uint64
	stop, done chan struct{}
	gcPercent  int // restored by Stop when tight
	tight      bool
}

// StartTight is Start with the collector running at GOGC=10 during the phase, so the heap in use
// tracks the live heap closely: it is for measuring the bytes per entry of in-memory structures
// (a catalog), not the garbage a default collector lets accumulate.
func StartTight(phase string) *Probe {
	old := debug.SetGCPercent(10)
	p := Start(phase)
	p.tight, p.gcPercent = true, old
	return p
}

// Interval between heap samples.
var Interval = 5 * time.Millisecond

// Start forces a GC, records the baseline and samples HeapInuse until Stop.
func Start(phase string) *Probe {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p := &Probe{phase: phase, start: time.Now(), baseHeap: ms.HeapInuse, baseAlloc: ms.TotalAlloc, peak: ms.HeapInuse,
		stop: make(chan struct{}), done: make(chan struct{})}
	p.baseLive = live()
	p.peakLive = p.baseLive
	p.reset = os.WriteFile("/proc/self/clear_refs", []byte("5"), 0) == nil
	p.baseRSS = status("VmRSS")
	go func() {
		defer close(p.done)
		t := time.NewTicker(Interval)
		defer t.Stop()
		var m runtime.MemStats
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				runtime.ReadMemStats(&m)
				p.mu.Lock()
				if m.HeapInuse > p.peak {
					p.peak = m.HeapInuse
				}
				if l := live(); l > p.peakLive {
					p.peakLive = l
				}
				p.mu.Unlock()
			}
		}
	}()
	return p
}

// Stop ends the phase and returns its result.
func (p *Probe) Stop() Result {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	close(p.stop)
	<-p.done
	if p.tight {
		debug.SetGCPercent(p.gcPercent)
	}
	p.mu.Lock()
	peak, peakLive := p.peak, p.peakLive
	p.mu.Unlock()
	if ms.HeapInuse > peak {
		peak = ms.HeapInuse
	}
	r := Result{Phase: p.phase, BaseHeap: p.baseHeap, PeakHeap: peak, BaseLive: p.baseLive, PeakLive: peakLive, BaseRSS: p.baseRSS, PeakRSS: status("VmHWM"), RSSReset: p.reset,
		TotalAlloc: ms.TotalAlloc - p.baseAlloc, Duration: time.Since(p.start)}
	if r.PeakHeap > r.BaseHeap {
		r.HeapDelta = r.PeakHeap - r.BaseHeap
	}
	if r.PeakLive > r.BaseLive {
		r.LiveDelta = r.PeakLive - r.BaseLive
	}
	if r.PeakRSS > r.BaseRSS {
		r.RSSDelta = r.PeakRSS - r.BaseRSS
	}
	return r
}

var liveSample = []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
var liveMu sync.Mutex

// live returns the heap marked live by the last GC cycle.
func live() uint64 {
	liveMu.Lock()
	defer liveMu.Unlock()
	metrics.Read(liveSample)
	if liveSample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return liveSample[0].Value.Uint64()
}

// status returns a /proc/self/status field in bytes (0 when unavailable).
func status(field string) uint64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, field+":") {
			continue
		}
		f := strings.Fields(l[len(field)+1:])
		if len(f) == 0 {
			return 0
		}
		v, _ := strconv.ParseUint(f[0], 10, 64)
		return v << 10
	}
	return 0
}

// MiB formats bytes as MiB with one decimal.
func MiB(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<20)) }

// Header is the table header matching Row.
const Header = "| scenario | size | phase | heap base MiB | heap in use, peak delta MiB | live heap, peak delta MiB | RSS peak delta MiB | allocated MiB | time |\n|---|---|---|---|---|---|---|---|---|"

// Row formats a result as one markdown table row.
func (r Result) Row(scenario, size string) string {
	rss := MiB(r.RSSDelta)
	if !r.RSSReset {
		rss += " (no reset)"
	}
	return fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s | %s |", scenario, size, r.Phase, MiB(r.BaseHeap), MiB(r.HeapDelta), MiB(r.LiveDelta), rss,
		MiB(r.TotalAlloc), r.Duration.Round(10*time.Millisecond))
}

// Report appends rows to the file named by IB_MEMBENCH_OUT (scripts/membench.sh collects them).
func Report(rows ...string) {
	fn := os.Getenv("IB_MEMBENCH_OUT")
	if fn == "" {
		return
	}
	f, err := os.OpenFile(fn, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	for _, r := range rows {
		fmt.Fprintln(f, r)
	}
}

// Sizes parses a comma-separated list of numbers from env (default def): the memory scenarios
// run at every listed size. The suite runs small sizes; scripts/membench.sh passes large ones.
func Sizes(env string, def []float64) []float64 {
	v := os.Getenv(env)
	if v == "" {
		return def
	}
	var out []float64
	for _, s := range strings.Split(v, ",") {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err == nil && f > 0 {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
