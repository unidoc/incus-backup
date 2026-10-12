package jobs

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/schedule"
)

// Automatic restore tests. A policy's restore_test_schedule ("monthly", "weekly", "03:00", ...)
// defines the PERIOD within which every object of the policy must have been restore-tested at least
// once, on every target of the policy. Testing everything at once would read the whole repository
// every period on one day, so the work is spread: the planner runs a "slot" at most once a day (or
// once per period for periods shorter than a day) and each slot tests a rotating share of the
// objects, oldest test first, ceil(N / slots-per-period) of them, plus any object whose last test is
// already older than one period and one slot (missed slots while the daemon was down). So every
// object is tested at least once per period plus one slot (a day). Method: filesystem objects
// are extracted (every file checked against the catalog); VM disks and block volumes get a deep test
// every period and a full restore into a scratch volume every 4th time; instance objects (stored
// configuration) get a deep test.
//
// Policies defined only in client.yaml carry no restore_test_schedule and are never tested
// automatically (only server-side policies have one).

// TestObject is one (object, target) pair the planner considers.
type TestObject struct {
	Target   string
	Project  string
	Name     string // logical name
	Kind     string // block | filesystem | instance
	Policy   string
	Schedule string // the policy's restore_test_schedule
}

// Key identifies the pair in the history.
func (o TestObject) Key() string { return o.Target + "|" + o.Project + "/" + o.Name }

// Planned is one test to run.
type Planned struct {
	TestObject
	Mode string
}

// TestRecord is what the history remembers about one pair.
type TestRecord struct {
	Last     time.Time `json:"last"`
	Count    int       `json:"count"` // tests run so far (any method)
	Mode     string    `json:"mode"`
	Passed   bool      `json:"passed"`
	Snapshot string    `json:"snapshot,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// History is <state>/restore-tests.json.
type History struct {
	mu      sync.Mutex
	path    string
	Objects map[string]*TestRecord `json:"objects"`
	Slots   map[string]time.Time   `json:"slots"` // policy -> last planning slot
}

// LoadHistory reads the history (missing = empty).
func LoadHistory(stateDir string) (*History, error) {
	h := &History{path: filepath.Join(stateDir, "restore-tests.json"), Objects: map[string]*TestRecord{}, Slots: map[string]time.Time{}}
	b, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, h); err != nil {
		return nil, err
	}
	if h.Objects == nil {
		h.Objects = map[string]*TestRecord{}
	}
	if h.Slots == nil {
		h.Slots = map[string]time.Time{}
	}
	return h, nil
}

// Record stores a finished test and persists.
func (h *History) Record(target string, r *jobspec.RestoreTestResult, at time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := target + "|" + r.Project + "/" + r.Object
	rec := h.Objects[k]
	if rec == nil {
		rec = &TestRecord{}
		h.Objects[k] = rec
	}
	rec.Last, rec.Mode, rec.Passed, rec.Snapshot = at.UTC(), r.Method, r.Passed, r.Snapshot
	rec.Count++
	rec.Error = ""
	if e, ok := r.Details["error"].(string); ok {
		rec.Error = e
	}
	return h.saveLocked()
}

// MarkSlot remembers that a policy's slot ran at now.
func (h *History) MarkSlot(policy string, now time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Slots[policy] = now.UTC()
	return h.saveLocked()
}

// Snapshot returns a copy for planning.
func (h *History) Snapshot() (map[string]TestRecord, map[string]time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	o := map[string]TestRecord{}
	for k, v := range h.Objects {
		o[k] = *v
	}
	s := map[string]time.Time{}
	for k, v := range h.Slots {
		s[k] = v
	}
	return o, s
}

func (h *History) saveLocked() error {
	if h.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o750); err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

// Period returns the length of one period of a schedule (0 for a manual one).
func Period(spec string, now time.Time) time.Duration {
	sp, err := schedule.Parse(spec)
	if err != nil || sp.Manual() {
		return 0
	}
	a := sp.Next(now)
	b := sp.Next(a)
	if a.IsZero() || b.IsZero() {
		return 0
	}
	return b.Sub(a)
}

// ModeFor picks the method of the next test of an object that was tested count times before.
func ModeFor(kind string, count int) string {
	switch kind {
	case "filesystem":
		return jobspec.ModeExtract
	case "block":
		if count%4 == 3 {
			return jobspec.ModeFull
		}
		return jobspec.ModeDeep
	}
	return jobspec.ModeDeep
}

// Plan returns the tests due at now, grouped by policy (objects with a manual or invalid schedule
// are never planned), and the policies whose slot ran (the caller marks them after running). It is
// pure: history and now are passed in.
func Plan(objs []TestObject, hist map[string]TestRecord, slots map[string]time.Time, now time.Time) ([]Planned, []string) {
	byPolicy := map[string][]TestObject{}
	for _, o := range objs {
		byPolicy[o.Policy] = append(byPolicy[o.Policy], o)
	}
	var policies []string
	for p := range byPolicy {
		policies = append(policies, p)
	}
	sort.Strings(policies)
	var out []Planned
	var ran []string
	for _, pol := range policies {
		group := byPolicy[pol]
		period := Period(group[0].Schedule, now)
		if period <= 0 {
			continue
		}
		slot := 24 * time.Hour
		if period < slot {
			slot = period
		}
		if last, ok := slots[pol]; ok && now.Sub(last) < slot && !now.Before(last) {
			continue
		}
		perPeriod := int(math.Round(float64(period) / float64(slot)))
		if perPeriod < 1 {
			perPeriod = 1
		}
		quota := (len(group) + perPeriod - 1) / perPeriod
		sort.Slice(group, func(i, j int) bool {
			a, b := hist[group[i].Key()], hist[group[j].Key()]
			if a.Last.Equal(b.Last) {
				return group[i].Key() < group[j].Key()
			}
			return a.Last.Before(b.Last) // never tested (zero time) first
		})
		picked := 0
		for _, o := range group {
			rec, tested := hist[o.Key()]
			// overdue (missed slots, e.g. the daemon was down): one slot of slack, so a period that
			// is shorter than the last one (February) does not turn a whole rotation overdue at once
			overdue := tested && !rec.Last.IsZero() && !now.Before(rec.Last.Add(period+slot))
			if picked < quota || overdue {
				out = append(out, Planned{TestObject: o, Mode: ModeFor(o.Kind, rec.Count)})
				picked++
			}
		}
		ran = append(ran, pol)
	}
	return out, ran
}
