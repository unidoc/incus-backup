// Package sysinfo reads the health of the machine and process the server runs on (Linux /proc and
// statfs; nothing needs root), samples it into PostgreSQL once a minute for the status page charts, and
// reports the database's own figures.
package sysinfo

import (
	"bufio"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Disk is the usage of one filesystem.
type Disk struct {
	Path       string `json:"path"`
	Label      string `json:"label"` // "state directory", "repository custody"
	TotalBytes int64  `json:"total_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
	Error      string `json:"error,omitempty"`
}

// StatFS measures the filesystem holding path.
func StatFS(path, label string) Disk {
	d := Disk{Path: path, Label: label}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		d.Error = err.Error()
		return d
	}
	bs := int64(st.Bsize)
	d.TotalBytes = int64(st.Blocks) * bs
	d.FreeBytes = int64(st.Bavail) * bs
	d.UsedBytes = d.TotalBytes - int64(st.Bfree)*bs
	return d
}

// Host is the machine.
type Host struct {
	Hostname      string  `json:"hostname"`
	Kernel        string  `json:"kernel"`
	OS            string  `json:"os"`
	UptimeSeconds float64 `json:"uptime_seconds"`
	Load1         float64 `json:"load1"`
	Load5         float64 `json:"load5"`
	Load15        float64 `json:"load15"`
	CPUs          int     `json:"cpus"`
	// CPUPercent is the whole machine's busy share since the previous reading (-1: unknown yet).
	CPUPercent     float64 `json:"cpu_percent"`
	MemTotalBytes  int64   `json:"mem_total_bytes"`
	MemAvailBytes  int64   `json:"mem_available_bytes"`
	MemUsedBytes   int64   `json:"mem_used_bytes"`
	SwapTotalBytes int64   `json:"swap_total_bytes"`
	SwapFreeBytes  int64   `json:"swap_free_bytes"`
}

// Process is this server process.
type Process struct {
	PID           int     `json:"pid"`
	RSSBytes      int64   `json:"rss_bytes"`
	OpenFDs       int     `json:"open_fds"`
	Goroutines    int     `json:"goroutines"`
	HeapBytes     uint64  `json:"heap_bytes"`
	HeapSysBytes  uint64  `json:"heap_sys_bytes"`
	GCCycles      uint32  `json:"gc_cycles"`
	GCPauseTotalS float64 `json:"gc_pause_total_seconds"`
	GoVersion     string  `json:"go_version"`
}

// cpu keeps the previous /proc/stat reading for the busy-share delta.
var cpu struct {
	mu          sync.Mutex
	busy, total uint64
	ok          bool
}

func readCPU() (busy, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fs := strings.Fields(sc.Text())
	if len(fs) < 5 || fs[0] != "cpu" {
		return 0, 0, false
	}
	var v []uint64
	for _, x := range fs[1:] {
		n, _ := strconv.ParseUint(x, 10, 64)
		v = append(v, n)
	}
	for i, n := range v {
		if i >= 8 { // guest time is already counted in user
			break
		}
		total += n
	}
	idle := v[3]
	if len(v) > 4 {
		idle += v[4] // iowait
	}
	return total - idle, total, true
}

// CPUPercent is the machine's busy share since the previous call (-1 on the first call).
func CPUPercent() float64 {
	b, t, ok := readCPU()
	if !ok {
		return -1
	}
	cpu.mu.Lock()
	defer cpu.mu.Unlock()
	pb, pt, had := cpu.busy, cpu.total, cpu.ok
	cpu.busy, cpu.total, cpu.ok = b, t, true
	if !had || t <= pt {
		return -1
	}
	return 100 * float64(b-pb) / float64(t-pt)
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func meminfo() map[string]int64 {
	out := map[string]int64{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fs := strings.Fields(v)
		if len(fs) == 0 {
			continue
		}
		n, _ := strconv.ParseInt(fs[0], 10, 64)
		if len(fs) > 1 && fs[1] == "kB" {
			n *= 1024
		}
		out[k] = n
	}
	return out
}

func osRelease() string {
	for _, l := range strings.Split(readFile("/etc/os-release"), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return runtime.GOOS
}

// ReadHost reads the machine's state. cpuPct is passed in (CPUPercent is stateful; the sampler owns it).
func ReadHost(cpuPct float64) Host {
	h := Host{CPUs: runtime.NumCPU(), CPUPercent: cpuPct, OS: osRelease(), Kernel: readFile("/proc/sys/kernel/osrelease")}
	h.Hostname, _ = os.Hostname()
	if fs := strings.Fields(readFile("/proc/uptime")); len(fs) > 0 {
		h.UptimeSeconds, _ = strconv.ParseFloat(fs[0], 64)
	}
	if fs := strings.Fields(readFile("/proc/loadavg")); len(fs) >= 3 {
		h.Load1, _ = strconv.ParseFloat(fs[0], 64)
		h.Load5, _ = strconv.ParseFloat(fs[1], 64)
		h.Load15, _ = strconv.ParseFloat(fs[2], 64)
	}
	m := meminfo()
	h.MemTotalBytes, h.MemAvailBytes = m["MemTotal"], m["MemAvailable"]
	h.MemUsedBytes = h.MemTotalBytes - h.MemAvailBytes
	h.SwapTotalBytes, h.SwapFreeBytes = m["SwapTotal"], m["SwapFree"]
	return h
}

// ReadProcess reads this process's state.
func ReadProcess() Process {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p := Process{PID: os.Getpid(), Goroutines: runtime.NumGoroutine(), HeapBytes: ms.HeapAlloc, HeapSysBytes: ms.HeapSys, GCCycles: ms.NumGC,
		GCPauseTotalS: float64(ms.PauseTotalNs) / 1e9, GoVersion: runtime.Version()}
	for _, l := range strings.Split(readFile("/proc/self/status"), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			fs := strings.Fields(v)
			if len(fs) > 0 {
				n, _ := strconv.ParseInt(fs[0], 10, 64)
				p.RSSBytes = n * 1024
			}
		}
	}
	if es, err := os.ReadDir("/proc/self/fd"); err == nil {
		p.OpenFDs = len(es)
	}
	return p
}

// Database is PostgreSQL as the server sees it.
type Database struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	Version     string `json:"version"`
	SizeBytes   int64  `json:"size_bytes"`
	Connections int    `json:"connections"` // backends connected to this database
	MaxConns    int    `json:"max_connections"`
	// Pool is this process's connection pool.
	PoolTotal  int32    `json:"pool_total"`
	PoolIdle   int32    `json:"pool_idle"`
	PoolInUse  int32    `json:"pool_in_use"`
	PoolMax    int32    `json:"pool_max"`
	Migrations []string `json:"migrations"`
	LatencyMs  float64  `json:"latency_ms"`
}

// ReadDatabase queries PostgreSQL's own figures.
func ReadDatabase(ctx context.Context, pool *pgxpool.Pool) Database {
	d := Database{Migrations: []string{}}
	st := pool.Stat()
	d.PoolTotal, d.PoolIdle, d.PoolInUse, d.PoolMax = st.TotalConns(), st.IdleConns(), st.AcquiredConns(), st.MaxConns()
	t0 := time.Now()
	err := pool.QueryRow(ctx, `SELECT current_setting('server_version'), pg_database_size(current_database()),
		(SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()), current_setting('max_connections')::int`).
		Scan(&d.Version, &d.SizeBytes, &d.Connections, &d.MaxConns)
	d.LatencyMs = float64(time.Since(t0).Microseconds()) / 1000
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.OK = true
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err == nil {
		for rows.Next() {
			var v string
			if rows.Scan(&v) == nil {
				d.Migrations = append(d.Migrations, v)
			}
		}
		rows.Close()
	}
	return d
}

// Sample is one row of system_samples.
type Sample struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	Load1      float64   `json:"load1"`
	MemUsed    int64     `json:"mem_used_bytes"`
	MemTotal   int64     `json:"mem_total_bytes"`
	DiskUsed   int64     `json:"disk_used_bytes"`
	DiskTotal  int64     `json:"disk_total_bytes"`
	DBBytes    int64     `json:"db_bytes"`
	RSS        int64     `json:"process_rss_bytes"`
	Goroutines int       `json:"goroutines"`
}

// Sampler writes a sample a minute and keeps KeepFor (default 7 days).
type Sampler struct {
	Pool     *pgxpool.Pool
	StateDir string
	Now      func() time.Time
	KeepFor  time.Duration
}

// Take records one sample now.
func (s *Sampler) Take(ctx context.Context) (*Sample, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	cpuPct := CPUPercent()
	if cpuPct < 0 {
		cpuPct = 0
	}
	h := ReadHost(cpuPct)
	p := ReadProcess()
	d := StatFS(s.StateDir, "state directory")
	var db int64
	_ = s.Pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&db)
	sm := &Sample{At: now().UTC().Truncate(time.Second), CPUPercent: cpuPct, Load1: h.Load1, MemUsed: h.MemUsedBytes, MemTotal: h.MemTotalBytes,
		DiskUsed: d.UsedBytes, DiskTotal: d.TotalBytes, DBBytes: db, RSS: p.RSSBytes, Goroutines: p.Goroutines}
	_, err := s.Pool.Exec(ctx, `INSERT INTO system_samples (taken_at, cpu_percent, load1, mem_used_bytes, mem_total_bytes, disk_used_bytes, disk_total_bytes, db_bytes, process_rss_bytes, goroutines)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (taken_at) DO NOTHING`,
		sm.At, sm.CPUPercent, sm.Load1, sm.MemUsed, sm.MemTotal, sm.DiskUsed, sm.DiskTotal, sm.DBBytes, sm.RSS, sm.Goroutines)
	if err != nil {
		return nil, err
	}
	keep := s.KeepFor
	if keep <= 0 {
		keep = 7 * 24 * time.Hour
	}
	_, _ = s.Pool.Exec(ctx, `DELETE FROM system_samples WHERE taken_at < $1`, now().Add(-keep))
	return sm, nil
}

// Loop samples once a minute until ctx ends.
func (s *Sampler) Loop(ctx context.Context) {
	CPUPercent() // prime the CPU delta
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = s.Take(ctx)
		}
	}
}

// Series returns the samples since `since`, at most maxPoints (evenly thinned, newest kept).
func Series(ctx context.Context, pool *pgxpool.Pool, since time.Time, maxPoints int) ([]Sample, error) {
	rows, err := pool.Query(ctx, `SELECT taken_at, cpu_percent, load1, mem_used_bytes, mem_total_bytes, disk_used_bytes, disk_total_bytes, db_bytes, process_rss_bytes, goroutines
		FROM system_samples WHERE taken_at >= $1 ORDER BY taken_at`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sample{}
	for rows.Next() {
		var x Sample
		var cpu, load float32
		if err := rows.Scan(&x.At, &cpu, &load, &x.MemUsed, &x.MemTotal, &x.DiskUsed, &x.DiskTotal, &x.DBBytes, &x.RSS, &x.Goroutines); err != nil {
			return nil, err
		}
		x.CPUPercent, x.Load1 = float64(cpu), float64(load)
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if maxPoints > 0 && len(out) > maxPoints {
		step := float64(len(out)) / float64(maxPoints)
		th := make([]Sample, 0, maxPoints)
		for i := 0; i < maxPoints; i++ {
			th = append(th, out[int(float64(i)*step)])
		}
		th[len(th)-1] = out[len(out)-1]
		out = th
	}
	return out, nil
}
