// Package config loads the incus-backup-server YAML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/unidoc/incus-backup/internal/server/webtls"
)

// DefaultPath is where the server looks for its configuration.
const DefaultPath = "/etc/incus-backup/server.yaml"

// Config is server.yaml.
type Config struct {
	Listen      string   `yaml:"listen"`
	StateDir    string   `yaml:"state_dir"`
	DatabaseURL string   `yaml:"database_url"`
	SANs        []string `yaml:"sans,omitempty"`
	// AdminSocket is the unix socket of the local administration API (used by the management UI).
	// Empty = <state_dir>/admin.sock; "off" disables it.
	AdminSocket string `yaml:"admin_socket,omitempty"`
	// WebListen is the HTTPS address of the management UI and /api/v1 ("" = ":8443", "off" disables it).
	WebListen string `yaml:"web_listen,omitempty"`
	// Web is the TLS mode of that listener (self-signed by default; files; ACME).
	Web webtls.Config `yaml:"web,omitempty"`
	// MetricsListen is an extra plain-HTTP listener that serves only GET /metrics (Prometheus), e.g.
	// "127.0.0.1:9187" ("" = none; the web listener serves /api/v1/metrics to API tokens anyway).
	MetricsListen string `yaml:"metrics_listen,omitempty"`
	// MetricsUnauthenticated lets that listener answer without an API token. Off by default; only for a
	// listener bound to a private address.
	MetricsUnauthenticated bool `yaml:"metrics_unauthenticated,omitempty"`
}

// WebAddr returns the web listen address ("" = disabled).
func (c *Config) WebAddr() string {
	switch c.WebListen {
	case "off":
		return ""
	case "":
		return ":8443"
	}
	return c.WebListen
}

// AdminSocketPath returns where the admin API listens ("" = disabled).
func (c *Config) AdminSocketPath() string {
	switch c.AdminSocket {
	case "off":
		return ""
	case "":
		return filepath.Join(c.StateDir, "admin.sock")
	}
	return c.AdminSocket
}

// Defaults returns the default configuration. The file holds only what the server needs
// before it can reach PostgreSQL; everything operational (repositories, S3 settings, policies,
// schedules, retention, verification, sync, notes) is stored in PostgreSQL and managed through
// the API.
func Defaults() Config {
	return Config{Listen: ":8007", StateDir: "/var/lib/incus-backup-server"}
}

// Load reads path (missing file = defaults) and overlays environment:
// INCUS_BACKUP_DATABASE_URL / DATABASE_URL override database_url.
func Load(path string) (*Config, error) {
	c := Defaults()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if err := yaml.Unmarshal(b, &c); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", path, err)
			}
		}
	}
	for _, e := range []string{"INCUS_BACKUP_DATABASE_URL", "DATABASE_URL"} {
		if v := os.Getenv(e); v != "" {
			c.DatabaseURL = v
			break
		}
	}
	return &c, c.Validate()
}

// Validate checks values that would otherwise fail late.
func (c *Config) Validate() error {
	if c.StateDir == "" {
		return fmt.Errorf("state_dir must be set")
	}
	return nil
}

// Save writes the configuration with mode 0600 (it may contain a database password).
func (c *Config) Save(path string) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
