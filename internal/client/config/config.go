// Package config loads the incus-backup client YAML configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/unidoc/incus-backup/internal/schedule"
)

// DefaultPath is where the client looks for its configuration.
const DefaultPath = "/etc/incus-backup/client.yaml"

// Config is client.yaml.
type Config struct {
	Host           string            `yaml:"host"`
	StateDir       string            `yaml:"state_dir"`
	SpoolDir       string            `yaml:"spool_dir"`
	KeyFile        string            `yaml:"key_file"`
	PassphraseFile string            `yaml:"passphrase_file,omitempty"`
	MaxFreeze      string            `yaml:"max_freeze"`
	MaxLag         string            `yaml:"max_target_lag"`
	VerifySchedule string            `yaml:"verify_schedule"`
	Incus          Incus             `yaml:"incus"`
	Servers        map[string]Server `yaml:"servers"`
	Control        string            `yaml:"control"`
	Policies       map[string]Policy `yaml:"policies,omitempty"`
	DefaultPolicy  string            `yaml:"default_policy,omitempty"`
	Filesystem     Filesystem        `yaml:"filesystem,omitempty"`
	Recovery       Recovery          `yaml:"recovery,omitempty"`
	Performance    Performance       `yaml:"performance,omitempty"`
	Jobs           Jobs              `yaml:"jobs,omitempty"`
}

// Jobs controls which jobs servers may ask this host to run (docs/jobs.md). By default the host
// accepts backup_now, restore_test and deep_verify; types that restore objects into Incus or hand out
// file contents (restore_instance, restore_volume, guest_files, fs_extract) must be listed in Allow.
type Jobs struct {
	Disabled bool     `yaml:"disabled,omitempty"` // never take jobs from any server
	Allow    []string `yaml:"allow,omitempty"`    // when set: exactly these types are accepted
}

// Performance tunes the client's worker pools; 0 (or absent) means the built-in default, which
// is what docs/performance.md measured.
type Performance struct {
	// EncodeWorkers: chunks compressed, encrypted and hashed at once (CPU; default 4).
	EncodeWorkers int `yaml:"encode_workers,omitempty"`
	// UploadWorkers: chunk uploads in flight to one server (network; default 16).
	UploadWorkers int `yaml:"upload_workers,omitempty"`
}

// Limits of the performance settings.
const (
	MaxEncodeWorkers = 64
	MaxUploadWorkers = 128
)

// Recovery configures granular file recovery from VM disk backups (a disposable VM reads the disk).
type Recovery struct {
	Image  string `yaml:"image,omitempty"`  // e.g. "images:alpine/edge/cloud" or a local alias
	Pool   string `yaml:"pool,omitempty"`   // storage pool for the scratch volumes
	Memory string `yaml:"memory,omitempty"` // default 512MiB
	CPU    string `yaml:"cpu,omitempty"`    // default 1
}

// Filesystem tunes the backup of containers and filesystem volumes.
type Filesystem struct {
	// Format is "catalog" (default: file catalog, change detection, browsing, single-file restore) or
	// "tar" (the original single archive stream, no catalog).
	Format string `yaml:"format,omitempty"`
	// FullEvery forces a run that reads every file again when the last one is older (default 168h).
	FullEvery string `yaml:"full_every,omitempty"`
	// TrustCtime (default true) also compares ctime and inode number when deciding that a file is unchanged.
	TrustCtime *bool `yaml:"trust_ctime,omitempty"`
}

// Incus describes how to reach Incus.
type Incus struct {
	Socket   string   `yaml:"socket"`
	Projects []string `yaml:"projects,omitempty"`
}

// Server is one target.
type Server struct {
	Address string `yaml:"address"`
	KeyFile string `yaml:"key_file,omitempty"` // different key domain than the default
}

// Policy is a locally defined policy (the server may define the same name).
type Policy struct {
	Schedule       string   `yaml:"schedule"`
	Targets        []string `yaml:"targets"`
	CommitRequired string   `yaml:"commit_required"`
}

// Defaults returns the default configuration.
func Defaults() Config {
	h, _ := os.Hostname()
	return Config{
		Host:           h,
		StateDir:       "/var/lib/incus-backup",
		SpoolDir:       "/var/lib/incus-backup/spool",
		KeyFile:        "/etc/incus-backup/key.json",
		MaxFreeze:      "15m",
		MaxLag:         "168h",
		VerifySchedule: "weekly",
		Incus:          Incus{Socket: ""},
		Servers:        map[string]Server{},
	}
}

// Load reads path; a missing file yields defaults (and Validate may then fail).
func Load(path string) (*Config, error) {
	c := Defaults()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := yaml.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
	}
	return &c, nil
}

// Validate checks everything that would otherwise fail late.
func (c *Config) Validate() error {
	if c.Host == "" {
		return errors.New("host must be set")
	}
	if _, err := time.ParseDuration(c.MaxFreeze); err != nil {
		return fmt.Errorf("max_freeze: %w", err)
	}
	if _, err := time.ParseDuration(c.MaxLag); err != nil {
		return fmt.Errorf("max_target_lag: %w", err)
	}
	if _, err := schedule.Parse(c.VerifySchedule); err != nil {
		return fmt.Errorf("verify_schedule: %w", err)
	}
	switch c.Filesystem.Format {
	case "", "catalog", "tar":
	default:
		return fmt.Errorf("filesystem.format must be catalog or tar, not %q", c.Filesystem.Format)
	}
	if c.Filesystem.FullEvery != "" {
		if d, err := time.ParseDuration(c.Filesystem.FullEvery); err != nil || d <= 0 {
			return fmt.Errorf("filesystem.full_every: not a positive duration: %q", c.Filesystem.FullEvery)
		}
	}
	if w := c.Performance.EncodeWorkers; w < 0 || w > MaxEncodeWorkers {
		return fmt.Errorf("performance.encode_workers must be between 0 (default) and %d, not %d", MaxEncodeWorkers, w)
	}
	if w := c.Performance.UploadWorkers; w < 0 || w > MaxUploadWorkers {
		return fmt.Errorf("performance.upload_workers must be between 0 (default) and %d, not %d", MaxUploadWorkers, w)
	}
	if len(c.Servers) == 0 {
		return errors.New("no servers configured: run `incus-backup init --server HOST:PORT --token TOKEN`")
	}
	for n, s := range c.Servers {
		if s.Address == "" {
			return fmt.Errorf("servers.%s: address is required", n)
		}
	}
	if c.Control != "" {
		if _, ok := c.Servers[c.Control]; !ok {
			return fmt.Errorf("control: unknown server %q", c.Control)
		}
	}
	for n, p := range c.Policies {
		if _, err := schedule.Parse(p.Schedule); err != nil {
			return fmt.Errorf("policies.%s: %w", n, err)
		}
		if p.CommitRequired != "" && p.CommitRequired != "all" && p.CommitRequired != "any" {
			return fmt.Errorf("policies.%s: commit_required must be all or any", n)
		}
		for _, t := range p.Targets {
			if _, ok := c.Servers[t]; !ok {
				return fmt.Errorf("policies.%s: unknown target %q", n, t)
			}
		}
	}
	return nil
}

// ControlServer returns the server that supplies desired state.
func (c *Config) ControlServer() string {
	if c.Control != "" {
		return c.Control
	}
	return c.ServerNames()[0]
}

// ServerNames returns the server names in a stable order.
func (c *Config) ServerNames() []string {
	var n []string
	for k := range c.Servers {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Freeze returns max_freeze.
func (c *Config) Freeze() time.Duration { d, _ := time.ParseDuration(c.MaxFreeze); return d }

// Lag returns max_target_lag.
func (c *Config) Lag() time.Duration { d, _ := time.ParseDuration(c.MaxLag); return d }

// ServerDir is where a server's credentials live.
func (c *Config) ServerDir(name string) string { return filepath.Join(c.StateDir, "servers", name) }

// KeyFor returns the key file used for a server.
func (c *Config) KeyFor(server string) string {
	if s, ok := c.Servers[server]; ok && s.KeyFile != "" {
		return s.KeyFile
	}
	return c.KeyFile
}

// Save writes the configuration (0600).
func (c *Config) Save(path string) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
