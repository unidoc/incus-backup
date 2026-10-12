// Package notify is the alerting and notification engine of incus-backup-server.
//
// Alerts are a state machine, the way a monitoring system works, not one message per event:
// a condition evaluated over the control plane (a missed backup, an offline host, a corrupt
// snapshot ...) or an emitted event opens an alert identified by a stable key; it stays firing
// while the condition holds, can be acknowledged or silenced, and resolves by itself when the
// condition clears. Rules decide which channels hear about which alerts, with reminders, quiet
// hours, flapping suppression and an optional "resolved" message. Deliveries go through a durable
// queue in PostgreSQL with retries, exponential backoff, a dead-letter state and a per-channel
// rate limit.
//
// Every threshold is data (notify_settings, notify_conditions, notify_rules); nothing is read
// from a configuration file. Secrets (SMTP passwords, tokens, webhook URLs and signing secrets)
// live sealed in the service secret store and are never returned by any function of this package.
//
// Time comes from service.Now only, so tests drive the engine with a fake clock.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/server/service"
)

// Severities, lowest first.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Alert states.
const (
	StateFiring       = "firing"
	StateAcknowledged = "acknowledged"
	StateSilenced     = "silenced"
	StateResolved     = "resolved"
)

// Alert types evaluated by the Runner (conditions) and the ones fed through Emit (events).
const (
	TypeBackupFailed          = "backup_failed"
	TypeBackupMissed          = "backup_missed"
	TypeHostOffline           = "host_offline"
	TypeMaintenanceFailed     = "maintenance_failed"
	TypeCorruption            = "corruption"
	TypeSyncFailed            = "sync_failed"
	TypeSyncLag               = "sync_lag"
	TypeCapacityWarning       = "capacity_warning"
	TypeRepositoryUnreachable = "repository_unreachable"
	TypeCertExpiry            = "cert_expiry"
	TypeRestoreTestFailed     = "restore_test_failed"
	TypeUpdateAvailable       = "update_available"
	TypeLoginSecurity         = "login_security" // event
	TypeJobFailed             = "job_failed"     // event
)

// conditionTypes are evaluated by Tick; an Event may not use them.
var conditionTypes = map[string]bool{
	TypeBackupFailed: true, TypeBackupMissed: true, TypeHostOffline: true, TypeMaintenanceFailed: true, TypeCorruption: true,
	TypeSyncFailed: true, TypeSyncLag: true, TypeCapacityWarning: true, TypeRepositoryUnreachable: true, TypeCertExpiry: true,
	TypeRestoreTestFailed: true, TypeUpdateAvailable: true,
}

func sevRank(s string) int {
	switch s {
	case SeverityInfo:
		return 0
	case SeverityWarning:
		return 1
	case SeverityCritical:
		return 2
	}
	return -1
}

func validSeverity(s string) bool { return sevRank(s) >= 0 }

// Errors (wrapping the service errors so the HTTP layer maps them the same way).
var (
	ErrNotFound   = service.ErrNotFound
	ErrBadRequest = service.ErrBadRequest
	ErrConflict   = service.ErrConflict
)

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, a...))
}

// CertItem is one certificate the server depends on (wired by the daemon: server TLS, CA, ACME ...).
type CertItem struct {
	Name     string    `json:"name"`
	Subject  string    `json:"subject"`
	NotAfter time.Time `json:"not_after"`
	Err      string    `json:"err,omitempty"` // the certificate could not be read
}

// RestoreTestResult is the latest restore test of one object.
type RestoreTestResult struct {
	Repository string    `json:"repository"`
	Host       string    `json:"host"`
	Object     string    `json:"object"` // project/name
	GroupID    string    `json:"group_id"`
	OK         bool      `json:"ok"`
	At         time.Time `json:"at"`
	Error      string    `json:"error"`
}

// RestoreTestSource reports the latest restore test per object (the restore_tests table is owned elsewhere).
type RestoreTestSource interface {
	LatestRestoreTests(ctx context.Context) ([]RestoreTestResult, error)
}

// Engine is the notification engine: API, condition Runner (Tick) and delivery queue.
type Engine struct {
	Svc *service.Service
	// CertStatus reports certificate expiry (nil = not wired: cert_expiry is not evaluated).
	CertStatus func(ctx context.Context) ([]CertItem, error)
	// RestoreTests reports restore test results (nil = not wired).
	RestoreTests RestoreTestSource
	// UpdateStatus reports the opt-in release check (nil = not wired: update_available is not evaluated).
	UpdateStatus func(ctx context.Context) (latest, current, url string, available bool)
	// Probe checks a repository (default Svc.ProbeRepository).
	Probe func(ctx context.Context, repo string) ([]datastore.ProbeResult, error)
	// ProbeTimeout bounds one repository probe (default 60 s).
	ProbeTimeout time.Duration
	// HTTPTransport overrides the base transport of HTTP channels (tests); nil = default.
	HTTPTransport http.RoundTripper

	hub  hub
	wake chan struct{}
	log  *slog.Logger
	once sync.Once
}

// New returns an engine over the service.
func New(svc *service.Service) *Engine {
	e := &Engine{Svc: svc}
	e.init()
	return e
}

func (e *Engine) init() {
	e.once.Do(func() {
		e.wake = make(chan struct{}, 1)
		e.log = e.Svc.Log
		if e.log == nil {
			e.log = slog.Default()
		}
		if e.Probe == nil {
			e.Probe = e.Svc.ProbeRepository
		}
		if e.ProbeTimeout == 0 {
			e.ProbeTimeout = 60 * time.Second
		}
	})
}

func (e *Engine) now() time.Time { return e.Svc.Now() }

// Wake signals that deliveries were queued outside a Tick (Emit); Run delivers them at once.
func (e *Engine) Wake() <-chan struct{} { e.init(); return e.wake }

func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run calls Tick every interval (the daemon passes one minute) and delivers queued messages as soon
// as Emit queues them, until ctx ends.
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	e.init()
	t := time.NewTicker(interval)
	defer t.Stop()
	tick := func() {
		if _, err := e.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			e.log.Error("notify tick failed", "err", err)
		}
	}
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		case <-e.wake:
			e.Deliver(ctx)
		}
	}
}
