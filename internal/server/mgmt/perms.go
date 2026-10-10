package mgmt

import "github.com/unidoc/incus-backup/internal/server/auth"

const (
	rd = auth.PermRead
	rs = auth.PermRestore
	op = auth.PermOperate
	ad = auth.PermAdmin
)

// perms is the role decision for every route, in one place so it can be reviewed (and tested) as a table:
// readonly sees everything but credentials; restore adds browsing and restoring; operator runs jobs and
// edits policies, schedules, notes and protection; admin does everything that creates, deletes or exposes
// infrastructure (repositories, secrets, users, enrolment tokens, forgetting, pruning, GC).
var perms = map[string]auth.Perm{
	"GET /api/v1/ping":   rd,
	"GET /api/v1/events": rd,
	"GET /api/v1/status": rd, "GET /api/v1/capacity": rd, "GET /api/v1/capacity/series": rd, "GET /api/v1/overview": rd,
	"PUT /api/v1/repositories/{name}/mode": ad,
	"GET /api/v1/repositories":             rd, "POST /api/v1/repositories": ad, "POST /api/v1/repositories/attach": ad,
	"POST /api/v1/repositories/{name}/probe": op,
	"GET /api/v1/credentials":                ad, "POST /api/v1/credentials": ad, "PUT /api/v1/credentials/{name}": ad, "DELETE /api/v1/credentials/{name}": ad,
	"GET /api/v1/hosts": rd, "POST /api/v1/hosts/{name}/{scope}/{action}": op, "POST /api/v1/tokens": ad,
	"GET /api/v1/policies": rd, "PUT /api/v1/policies/{name}": op, "DELETE /api/v1/policies/{name}": ad,
	"GET /api/v1/assignments": rd, "PUT /api/v1/assignments": op, "DELETE /api/v1/assignments": op,
	"GET /api/v1/objects": rd, "PUT /api/v1/objects/{group}/retention": op, "POST /api/v1/objects/{group}/transfer": ad, "PUT /api/v1/objects/{group}/notes": op,
	"GET /api/v1/snapshots": rd, "PUT /api/v1/snapshots/{group}/{snap}/protected": op, "PUT /api/v1/snapshots/{group}/{snap}/notes": op,
	"GET /api/v1/snapshots/{group}/{snap}/verifications": rd, "DELETE /api/v1/snapshots/{group}/{snap}": ad,
	"GET /api/v1/maintenance": rd, "PUT /api/v1/maintenance": ad, "GET /api/v1/runs": rd,
	"POST /api/v1/prune": ad, "POST /api/v1/gc": ad, "POST /api/v1/verify": op, "POST /api/v1/reconcile": ad,
	"GET /api/v1/corrupt": rd, "DELETE /api/v1/escrow": ad,
	"GET /api/v1/sync": rd, "POST /api/v1/sync": ad, "PUT /api/v1/sync/{name}": op, "POST /api/v1/sync/{name}/run": op,
	"GET /api/v1/sync/{name}": rd, "DELETE /api/v1/sync/{name}": ad,

	// people and automation
	"POST /api/v1/session": rd, "GET /api/v1/session": rd, "DELETE /api/v1/session": rd,
	"GET /api/v1/setup": rd, "POST /api/v1/setup": rd,
	"GET /api/v1/users": ad, "POST /api/v1/users": ad, "PUT /api/v1/users/{name}": ad, "DELETE /api/v1/users/{name}": ad,
	"POST /api/v1/users/{name}/password": ad, "DELETE /api/v1/users/{name}/totp": ad,
	"POST /api/v1/me/password": rd, "POST /api/v1/me/totp": rd, "POST /api/v1/me/totp/confirm": rd, "DELETE /api/v1/me/totp": rd,
	"GET /api/v1/me/sessions": rd, "DELETE /api/v1/me/sessions/{id}": rd,
	"GET /api/v1/me/tokens": rd, "POST /api/v1/me/tokens": rd, "DELETE /api/v1/me/tokens/{id}": rd,
	"GET /api/v1/audit": ad,
	"GET /api/v1/jobs":  rd, "POST /api/v1/jobs": rs, "GET /api/v1/jobs/{id}": rd, "POST /api/v1/jobs/{id}/cancel": op, "GET /api/v1/jobs/{id}/delivery": rs,
	"GET /api/v1/restore-tests": rd, "POST /api/v1/restore-tests/run": op,
	"GET /api/v1/certificates": rd, "PUT /api/v1/certificates/sans": ad, "POST /api/v1/certificates/ca/rotate": ad, "POST /api/v1/certificates/ca/promote": ad,
	"GET /api/v1/notifications/channels": op, "POST /api/v1/notifications/channels": ad, "PUT /api/v1/notifications/channels/{id}": ad,
	"DELETE /api/v1/notifications/channels/{id}": ad, "POST /api/v1/notifications/channels/{id}/test": ad,
	"GET /api/v1/notifications/rules": op, "POST /api/v1/notifications/rules": ad, "PUT /api/v1/notifications/rules/{id}": ad, "DELETE /api/v1/notifications/rules/{id}": ad,
	"GET /api/v1/alerts": rd, "GET /api/v1/alerts/{id}": rd, "POST /api/v1/alerts/{id}/ack": op,
	"GET /api/v1/silences": rd, "POST /api/v1/silences": op, "DELETE /api/v1/silences/{id}": op,
	"GET /api/v1/notifications/deliveries": op, "POST /api/v1/notifications/deliveries/{id}/retry": ad,
	"GET /api/v1/notifications/settings": op, "PUT /api/v1/notifications/settings": ad, "PUT /api/v1/notifications/conditions/{type}": ad,
	"GET /api/v1/settings": ad, "PUT /api/v1/settings": ad, "POST /api/v1/onboarding": ad,
	"GET /api/v1/snapshots/{group}/{snap}/index": rs, "GET /api/v1/snapshots/{group}/{snap}/manifest": rs, "GET /api/v1/chunks/{id}": rs,
}
