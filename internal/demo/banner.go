package demo

import (
	"fmt"
	"strings"
	"time"
)

// Banner is the human summary printed at start: URLs, credentials, the key and what was seeded.
func (s *Stack) Banner(startup time.Duration) string {
	var b strings.Builder
	line := strings.Repeat("=", 78)
	fmt.Fprintf(&b, "\n%s\n incus-backup DEV DEMO (real server stack, seeded data; never use for real backups)\n%s\n", line, line)
	fmt.Fprintf(&b, " Web UI + API   %s/   (plain HTTP, Secure cookies off)\n", s.WebURL)
	if s.Opt.UIDir != "" {
		fmt.Fprintf(&b, " UI served from %s (re-read on every request)\n", s.Opt.UIDir)
	} else {
		fmt.Fprintf(&b, " UI             embedded build (pass --ui-dir web/dist to serve a Vite build)\n")
	}
	fmt.Fprintf(&b, " Agent API      https://%s   (mutual TLS, hosts and sync peers)\n", s.AgentAddr)
	fmt.Fprintf(&b, " Admin socket   %s   (curl --unix-socket %s http://x/api/v1/overview)\n", s.SocketPath, s.SocketPath)
	if s.S3Endpoint != "" {
		fmt.Fprintf(&b, " MinIO (S3)     http://%s   access %s / secret %s, bucket offsite\n", s.S3Endpoint, minioAccess, minioSecret)
	}
	fmt.Fprintf(&b, " Data dir       %s\n", s.Opt.Dir)
	fmt.Fprintf(&b, "\n Users (password for all: %s)\n", s.Opt.Password)
	for _, u := range Users {
		fmt.Fprintf(&b, "   %-9s role %s\n", u.Name, u.Role)
	}
	if s.State.RecoveryKey != "" {
		fmt.Fprintf(&b, "\n Key domain     %s\n", s.State.DomainID)
		fmt.Fprintf(&b, " Recovery key   %s\n", s.State.RecoveryKey)
		fmt.Fprintf(&b, "                (what `incus-backup key recovery` prints; the UI's loadRecoveryKey takes it)\n")
		fmt.Fprintf(&b, " Master (hex)   %s\n", s.State.MasterHex)
		fmt.Fprintf(&b, " Escrow private %s\n", s.State.EscrowPriv)
		fmt.Fprintf(&b, "                (opens the escrow blob bosch stored on the server: GET via the agent API only)\n")
	}
	if f := s.State.Facts; f != nil {
		fmt.Fprintf(&b, "\n Seeded (%s profile, %.0f s): %d snapshots, %d backup runs, repositories %s (offsite: %s)\n",
			s.State.Profile, f.SeedDurationSec, f.Snapshots, f.BackupRuns, strings.Join(f.Repositories, ", "), s.State.Offsite)
		fmt.Fprintf(&b, "   offline host        %s (last seen 3 days before the seed)\n", f.OfflineHost)
		fmt.Fprintf(&b, "   failing backups     %s (every retry fails)\n", f.FailingObject)
		fmt.Fprintf(&b, "   stale (overdue)     %s (excluded 3 days ago) + everything on %s\n", f.StaleObject, f.OfflineHost)
		fmt.Fprintf(&b, "   never backed up     %s (policy none)\n", f.NeverBackedUp)
		fmt.Fprintf(&b, "   hourly object       %s\n", f.HourlyObject)
		fmt.Fprintf(&b, "   protected snapshot  %s @ %s\n", f.Protected.Object, f.Protected.Snapshot)
		fmt.Fprintf(&b, "   failed verification %s @ %s\n", f.FailedVerify.Object, f.FailedVerify.Snapshot)
		fmt.Fprintf(&b, "   browsable files     %s @ %s (try %q)\n", f.FSObject.Object, f.FSObject.Snapshot, f.FSFile)
		for _, n := range []string{"offsite", "offsite-archive"} {
			fmt.Fprintf(&b, "   sync job %-16s last run %s\n", n, f.SyncJobs[n])
		}
		if s.State.SeededAt != nil {
			age := time.Since(*s.State.SeededAt).Round(time.Minute)
			fmt.Fprintf(&b, "   seeded %s ago", age)
			if age > 3*time.Hour {
				fmt.Fprintf(&b, " -- the situations decay with real time (hosts go offline after 6 h): use --reset for E2E")
			}
			b.WriteString("\n")
		}
	}
	if len(s.JobHosts) > 0 {
		fmt.Fprintf(&b, "\n Host jobs      executors running on %s (every job type allowed, guest_files excepted: no recovery VM)\n", strings.Join(s.JobHosts, ", "))
	} else {
		fmt.Fprintf(&b, "\n Host jobs      no executor runs (--no-jobs or unseeded): queued jobs stay queued\n")
	}
	fmt.Fprintf(&b, " Several repositories exist: repository-scoped API calls need ?repository=custody|offsite\n")
	fmt.Fprintf(&b, " Ready in %s. Ctrl-C stops everything.\n%s\n", startup.Round(time.Millisecond), line)
	return b.String()
}
