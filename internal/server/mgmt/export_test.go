package mgmt

import "github.com/unidoc/incus-backup/internal/server/auth"

// Perms exposes the route table to the external tests.
func Perms() map[string]auth.Perm { return perms }

// PublicRoutes exposes the routes that need no login.
func PublicRoutes() map[string]bool { return publicRoutes }

// SplitCommaForTest exposes the query-list splitter.
var SplitCommaForTest = splitComma
