// Package migrations embeds the PostgreSQL schema migrations of
// incus-backup-server. Files are named YYYYMMDDHHMMSS_name.sql, applied once in
// filename order and tracked in the schema_migrations table (same convention as
// gorisk and isms). One migration per released version.
package migrations

import "embed"

// FS holds every *.sql migration.
//
//go:embed *.sql
var FS embed.FS
