package mgmt

import (
	"encoding/json"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/server/auth"
	"github.com/unidoc/incus-backup/internal/server/jobs"
	"github.com/unidoc/incus-backup/internal/server/notify"
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/syncer"
)

// Route metadata: the single description of every /api/v1 route besides its role (perms.go). It drives
//
//   - repository scoping (Bind: how the repository a request touches is found, scope.go), and
//   - the OpenAPI document (openapi.go, GET /api/v1/openapi.json, `incus-backup-server openapi`).
//
// TestEveryRouteHasMetadata fails when a registered route has no entry here or an entry names a route
// that does not exist, so neither the permission model nor the documentation can drift.

// repoBinding says which repository a route acts on.
type repoBinding int

const (
	// bindGlobal: not bound to a repository; the principal's global role decides.
	bindGlobal repoBinding = iota
	// bindSelf: the caller's own session, account or tokens (any signed-in principal).
	bindSelf
	// bindQuery: ?repository=NAME, defaulting to the only repository (what withRepo resolves).
	bindQuery
	// bindPath: the {name} path value is the repository.
	bindPath
	// bindBody: the JSON body's "repository" field, else as bindQuery.
	bindBody
	// bindJob: {id} is a job; its repository decides (another repository's job is "not found").
	bindJob
	// bindAlert: {id} is an alert; its repository decides ("" = the global role).
	bindAlert
	// bindList: lists across repositories; every item is filtered by its repository (ListKey) or by
	// the handler (ListKey "").
	bindList
)

func (b repoBinding) String() string {
	return [...]string{"global", "self", "query", "path", "body", "job", "alert", "list"}[b]
}

// param is one query parameter.
type param struct {
	Name, Type, Desc string
	Required         bool
}

// apiRoute documents one route.
type apiRoute struct {
	Summary string
	Tag     string
	Bind    repoBinding
	// ListKey: for bindList, the JSON field of each array item that names its repository.
	ListKey string
	Query   []param
	Body    any // a Go value whose type is the JSON request body (nil: none)
	Resp    any // a Go value whose type is the JSON response (nil: 204 No Content unless Kind says otherwise)
	Status  int // success status (default 200, or 204 when Resp is nil)
	Kind    string
}

const (
	kindBinary = "binary" // application/octet-stream
	kindSSE    = "sse"    // text/event-stream
)

func qp(name, typ, desc string) param { return param{Name: name, Type: typ, Desc: desc} }

var (
	repoQ  = qp("repository", "string", "repository name; may be omitted when exactly one repository exists")
	limitQ = qp("limit", "integer", "maximum number of items")
	dryQ   = qp("dry_run", "string", "1 = only report what would happen")
)

// apiRoutes is the metadata table. Extra files add their routes in init().
var apiRoutes = map[string]apiRoute{
	// ---- system
	"GET /api/v1/ping":            {Summary: "Liveness and version (no login)", Tag: "System", Bind: bindSelf, Resp: docPing{}},
	"GET /api/v1/events":          {Summary: "Live updates as Server-Sent Events (job, alert, repository); filtered by the caller's repository access", Tag: "System", Bind: bindList, Kind: kindSSE},
	"GET /api/v1/about":           {Summary: "Version, uptime and whether PostgreSQL answers", Tag: "System", Resp: About{}},
	"GET /api/v1/setup-checklist": {Summary: "First-run checklist (what is configured and what is missing)", Tag: "System", Resp: service.Checklist{}},
	"GET /api/v1/openapi.json":    {Summary: "This OpenAPI 3.1 document", Tag: "System", Bind: bindSelf, Resp: map[string]any{}},
	"GET /api/v1/settings":        {Summary: "Server settings (key/value)", Tag: "Settings", Resp: map[string]string{}},
	"PUT /api/v1/settings":        {Summary: "Set server settings (keys not sent are unchanged)", Tag: "Settings", Body: map[string]string{}},
	"GET /api/v1/schedule/preview": {Summary: "Parse a schedule and list its next firings by the server's clock", Tag: "Settings",
		Query: []param{{Name: "spec", Type: "string", Desc: "schedule expression", Required: true}, qp("count", "integer", "number of firings (1-20, default 5)"), qp("after", "string", "RFC 3339: report whether it fired since then")},
		Resp:  SchedulePreview{}},

	// ---- dashboard and capacity
	"GET /api/v1/overview":        {Summary: "Dashboard model across the repositories the caller may see", Tag: "Dashboard", Bind: bindList, Resp: service.Overview{}},
	"GET /api/v1/status":          {Summary: "Repository status counters", Tag: "Dashboard", Bind: bindQuery, Query: []param{repoQ}, Resp: service.Status{}},
	"GET /api/v1/capacity":        {Summary: "Capacity, growth and fill forecast of a repository", Tag: "Capacity", Bind: bindQuery, Query: []param{repoQ}, Resp: capacityView{}},
	"GET /api/v1/capacity/series": {Summary: "Capacity samples over time", Tag: "Capacity", Bind: bindQuery, Query: []param{repoQ, qp("days", "integer", "history length in days")}, Resp: []service.CapacityPoint{}},
	"GET /api/v1/snapshot-stats":  {Summary: "Recorded statistics of every snapshot (optionally ?group=a,b)", Tag: "Snapshots", Bind: bindQuery, Query: []param{repoQ, qp("group", "string", "comma-separated group ids")}, Resp: []service.SnapshotStats{}},
	"GET /api/v1/runs/backups":    {Summary: "Backup runs reported by hosts, newest first", Tag: "Hosts", Bind: bindQuery, Query: []param{repoQ, qp("host", "string", "host name"), qp("status", "string", "ok, partial or failed"), qp("before", "string", "RFC 3339 cursor"), limitQ}, Resp: []service.BackupRunRow{}},
	"GET /api/v1/objects/stats":   {Summary: "Every storage object with freshness, latest snapshot and size history", Tag: "Objects", Bind: bindQuery, Query: []param{repoQ, qp("host", "string", "host name")}, Resp: []service.ObjectStat{}},

	// ---- repositories and credentials
	"GET /api/v1/repositories":               {Summary: "Repositories the caller may see (never credentials)", Tag: "Repositories", Bind: bindList, ListKey: "name", Resp: []RepoView{}},
	"POST /api/v1/repositories":              {Summary: "Create a repository (filesystem or S3)", Tag: "Repositories", Body: docCreateRepo{}, Resp: RepoView{}, Status: 201},
	"POST /api/v1/repositories/attach":       {Summary: "Attach an existing datastore (adopt=true takes over ownership)", Tag: "Repositories", Body: docCreateRepo{}, Resp: RepoView{}, Status: 201},
	"POST /api/v1/repositories/{name}/probe": {Summary: "Run the storage probe (put-if-absent, listing, read-after-write...)", Tag: "Repositories", Bind: bindPath, Resp: []datastore.ProbeResult{}},
	"POST /api/v1/repositories/check":        {Summary: "Check connection settings before creating a repository", Tag: "Repositories", Body: service.RepoCheckRequest{}, Resp: service.RepoCheckReport{}},
	"POST /api/v1/repositories/{name}/check": {Summary: "Check an existing repository's storage", Tag: "Repositories", Bind: bindPath, Resp: service.RepoCheckReport{}},
	"GET /api/v1/repositories/{name}/info":   {Summary: "Repository identity and owner marker", Tag: "Repositories", Bind: bindPath, Resp: service.RepoInfo{}},
	"PUT /api/v1/repositories/{name}/mode":   {Summary: "Set maintenance mode (online, read-only, offline)", Tag: "Repositories", Bind: bindPath, Body: docMode{}},
	"GET /api/v1/credentials":                {Summary: "S3 credentials (names and key ids, never secrets)", Tag: "Credentials", Resp: []service.Credential{}},
	"POST /api/v1/credentials":               {Summary: "Store an S3 credential (sealed)", Tag: "Credentials", Body: docCred{}, Resp: service.Credential{}, Status: 201},
	"PUT /api/v1/credentials/{name}":         {Summary: "Rotate an S3 credential", Tag: "Credentials", Body: docCred{}},
	"DELETE /api/v1/credentials/{name}":      {Summary: "Delete an unused S3 credential", Tag: "Credentials"},

	// ---- hosts, enrolment
	"GET /api/v1/hosts":                          {Summary: "Enrolled hosts of a repository", Tag: "Hosts", Bind: bindQuery, Query: []param{repoQ}, Resp: []service.HostRow{}},
	"POST /api/v1/hosts/{name}/{scope}/{action}": {Summary: "Enable or disable a host certificate ({action} = enable|disable, {scope} = backup|restore)", Tag: "Hosts", Bind: bindQuery, Query: []param{repoQ}},
	"POST /api/v1/tokens":                        {Summary: "Create a one-time enrolment token for a host", Tag: "Hosts", Bind: bindQuery, Query: []param{repoQ}, Body: docEnrolReq{}, Resp: docEnrolResp{}, Status: 201},
	"POST /api/v1/onboarding":                    {Summary: "Enrolment token plus the exact command to run on the new host", Tag: "Hosts", Bind: bindQuery, Query: []param{repoQ}, Body: docEnrolReq{}, Resp: docOnboarding{}, Status: 201},
	"GET /api/v1/onboarding/pending":             {Summary: "Enrolment tokens issued and not used yet (metadata only)", Tag: "Hosts", Bind: bindQuery, Query: []param{repoQ}, Resp: []service.PendingEnrolment{}},
	"DELETE /api/v1/onboarding/pending/{id}":     {Summary: "Revoke an unused enrolment token", Tag: "Hosts", Bind: bindQuery, Query: []param{repoQ}},

	// ---- policies
	"GET /api/v1/policies":           {Summary: "Policies of a repository", Tag: "Policies", Bind: bindQuery, Query: []param{repoQ}, Resp: []api.Policy{}},
	"PUT /api/v1/policies/{name}":    {Summary: "Create or replace a policy", Tag: "Policies", Bind: bindQuery, Query: []param{repoQ}, Body: api.Policy{}},
	"DELETE /api/v1/policies/{name}": {Summary: "Delete a policy", Tag: "Policies", Bind: bindQuery, Query: []param{repoQ}},
	"GET /api/v1/policies/effective": {Summary: "The retention policy that applies to every object, and why", Tag: "Policies", Bind: bindQuery, Query: []param{repoQ}, Resp: []service.EffectivePolicy{}},
	"GET /api/v1/assignments":        {Summary: "Policy assignments (host, project, object)", Tag: "Policies", Bind: bindQuery, Query: []param{repoQ}, Resp: []service.AssignmentRow{}},
	"PUT /api/v1/assignments":        {Summary: "Assign a policy", Tag: "Policies", Bind: bindQuery, Query: []param{repoQ}, Body: service.AssignmentRow{}},
	"DELETE /api/v1/assignments":     {Summary: "Remove an assignment", Tag: "Policies", Bind: bindQuery, Query: []param{repoQ}, Body: service.AssignmentRow{}},

	// ---- objects and snapshots
	"GET /api/v1/objects":                                {Summary: "Storage objects (backup groups) of a repository", Tag: "Objects", Bind: bindQuery, Query: []param{repoQ}, Resp: []service.ObjectRow{}},
	"PUT /api/v1/objects/{group}/retention":              {Summary: "Pin a retention policy to an object", Tag: "Objects", Bind: bindQuery, Query: []param{repoQ}, Body: docPolicyName{}},
	"POST /api/v1/objects/{group}/transfer":              {Summary: "Transfer an object to another backup host", Tag: "Objects", Bind: bindQuery, Query: []param{repoQ}, Body: docHost{}},
	"PUT /api/v1/objects/{group}/notes":                  {Summary: "Set an object's notes", Tag: "Objects", Bind: bindQuery, Query: []param{repoQ}, Body: docNotes{}},
	"GET /api/v1/snapshots":                              {Summary: "Snapshots of a repository (optionally one ?group=)", Tag: "Snapshots", Bind: bindQuery, Query: []param{repoQ, qp("group", "string", "group id")}, Resp: []api.SnapshotRef{}},
	"PUT /api/v1/snapshots/{group}/{snap}/protected":     {Summary: "Protect or unprotect a snapshot", Tag: "Snapshots", Bind: bindQuery, Query: []param{repoQ}, Body: docProtected{}},
	"PUT /api/v1/snapshots/{group}/{snap}/notes":         {Summary: "Set a snapshot's notes", Tag: "Snapshots", Bind: bindQuery, Query: []param{repoQ}, Body: docNotes{}},
	"GET /api/v1/snapshots/{group}/{snap}/verifications": {Summary: "Verification history of a snapshot", Tag: "Snapshots", Bind: bindQuery, Query: []param{repoQ}, Resp: []service.SnapshotVerification{}},
	"DELETE /api/v1/snapshots/{group}/{snap}":            {Summary: "Forget a snapshot (refused while protected)", Tag: "Snapshots", Bind: bindQuery, Query: []param{repoQ}},
	"GET /api/v1/snapshots/{group}/{snap}/index":         {Summary: "Encrypted index of a snapshot (the browser decrypts it)", Tag: "Snapshots", Bind: bindQuery, Query: []param{repoQ}, Kind: kindBinary},
	"GET /api/v1/snapshots/{group}/{snap}/manifest":      {Summary: "Encrypted manifest of a snapshot (the browser decrypts it)", Tag: "Snapshots", Bind: bindQuery, Query: []param{repoQ}, Kind: kindBinary},
	"GET /api/v1/chunks/{id}":                            {Summary: "One encrypted chunk of the repository", Tag: "Snapshots", Bind: bindQuery, Query: []param{repoQ}, Kind: kindBinary},

	// ---- maintenance
	"GET /api/v1/maintenance": {Summary: "Maintenance schedules and GC grace", Tag: "Maintenance", Bind: bindQuery, Query: []param{repoQ}, Resp: docMaintenance{}},
	"PUT /api/v1/maintenance": {Summary: "Change maintenance schedules (fields not sent are unchanged)", Tag: "Maintenance", Bind: bindQuery, Query: []param{repoQ}, Body: docMaintenancePut{}},
	"GET /api/v1/runs":        {Summary: "Maintenance runs (prune, GC, verify, reconcile...), newest first", Tag: "Maintenance", Bind: bindQuery, Query: []param{repoQ, limitQ}, Resp: []service.RunRow{}},
	"POST /api/v1/prune":      {Summary: "Apply retention (dry_run=1 returns the plan with a reason per snapshot)", Tag: "Maintenance", Bind: bindQuery, Query: []param{repoQ, dryQ}, Resp: service.PruneResult{}},
	"POST /api/v1/gc":         {Summary: "Garbage-collect unreferenced chunks", Tag: "Maintenance", Bind: bindQuery, Query: []param{repoQ, dryQ, qp("grace_hours", "number", "override the grace period")}, Resp: datastore.GCStats{}},
	"POST /api/v1/verify":     {Summary: "Verify snapshots (mode all, new or stale)", Tag: "Maintenance", Bind: bindQuery, Query: []param{repoQ}, Body: docVerifyReq{}, Resp: docVerifyResp{}},
	"POST /api/v1/reconcile":  {Summary: "Rebuild PostgreSQL rows from the datastore", Tag: "Maintenance", Bind: bindQuery, Query: []param{repoQ}, Resp: service.ReconcileResult{}},
	"GET /api/v1/corrupt":     {Summary: "Quarantined chunks and the snapshots they affect", Tag: "Maintenance", Bind: bindQuery, Query: []param{repoQ, qp("healed", "string", "1 = include healed chunks")}, Resp: []service.CorruptChunk{}},
	"DELETE /api/v1/escrow":   {Summary: "Delete the escrowed key material of a repository", Tag: "Maintenance", Bind: bindQuery, Query: []param{repoQ}},

	// ---- sync
	"GET /api/v1/sync":                 {Summary: "Sync jobs of a repository", Tag: "Sync", Bind: bindQuery, Query: []param{repoQ}, Resp: []syncer.Job{}},
	"POST /api/v1/sync":                {Summary: "Add a sync job (the peer token is used once)", Tag: "Sync", Bind: bindQuery, Query: []param{repoQ}, Body: docSyncAdd{}},
	"PUT /api/v1/sync/{name}":          {Summary: "Change a sync job's options", Tag: "Sync", Bind: bindQuery, Query: []param{repoQ}, Body: syncer.JobOptions{}},
	"POST /api/v1/sync/{name}/run":     {Summary: "Run a sync job now", Tag: "Sync", Bind: bindQuery, Query: []param{repoQ}, Resp: syncer.Result{}},
	"GET /api/v1/sync/{name}":          {Summary: "Sync job status", Tag: "Sync", Bind: bindQuery, Query: []param{repoQ}, Resp: syncer.JobStatus{}},
	"DELETE /api/v1/sync/{name}":       {Summary: "Remove a sync job", Tag: "Sync", Bind: bindQuery, Query: []param{repoQ}},
	"GET /api/v1/sync/{name}/runs":     {Summary: "Run history of a sync job", Tag: "Sync", Bind: bindQuery, Query: []param{repoQ, limitQ}, Resp: []syncer.RunRecord{}},
	"PUT /api/v1/sync/{name}/schedule": {Summary: "Set a sync job's schedule", Tag: "Sync", Bind: bindQuery, Query: []param{repoQ}, Body: syncer.ScheduleUpdate{}},
	"POST /api/v1/sync/check-remote":   {Summary: "Check a sync peer without spending its token", Tag: "Sync", Body: docCheckRemote{}, Resp: syncer.RemoteCheckResult{}},

	// ---- people
	"POST /api/v1/session":               {Summary: "Sign in (sets the session cookie; returns the CSRF token)", Tag: "Auth", Bind: bindSelf, Body: docLogin{}, Resp: sessionView{}},
	"GET /api/v1/session":                {Summary: "Who am I: user, CSRF token and repository access", Tag: "Auth", Bind: bindSelf, Resp: sessionView{}},
	"DELETE /api/v1/session":             {Summary: "Sign out", Tag: "Auth", Bind: bindSelf},
	"GET /api/v1/setup":                  {Summary: "Whether first-run setup is needed (no login)", Tag: "Auth", Bind: bindSelf, Resp: docSetupNeeded{}},
	"POST /api/v1/setup":                 {Summary: "Create the first administrator with the setup token (no login)", Tag: "Auth", Bind: bindSelf, Body: docSetup{}, Resp: auth.User{}, Status: 201},
	"GET /api/v1/users":                  {Summary: "Users", Tag: "Users", Resp: []auth.User{}},
	"POST /api/v1/users":                 {Summary: "Create a user", Tag: "Users", Body: docUserCreate{}, Resp: auth.User{}, Status: 201},
	"PUT /api/v1/users/{name}":           {Summary: "Change a user's global role, name, e-mail or disabled flag", Tag: "Users", Body: docUserUpdate{}},
	"DELETE /api/v1/users/{name}":        {Summary: "Delete a user", Tag: "Users"},
	"POST /api/v1/users/{name}/password": {Summary: "Set a user's password", Tag: "Users", Body: docSetPassword{}},
	"DELETE /api/v1/users/{name}/totp":   {Summary: "Remove a user's two-factor enrolment", Tag: "Users"},
	"GET /api/v1/users/{name}/access":    {Summary: "A user's global role and per-repository roles", Tag: "Users", Resp: auth.UserAccess{}},
	"PUT /api/v1/users/{name}/access":    {Summary: "Replace a user's per-repository roles (scoped=false: the global role applies everywhere)", Tag: "Users", Body: docAccessPut{}, Resp: auth.UserAccess{}},
	"GET /api/v1/audit":                  {Summary: "Audit log, newest first", Tag: "Users", Query: []param{qp("actor", "string", "actor name"), qp("action", "string", "action prefix"), qp("before", "integer", "id cursor"), limitQ}, Resp: []auth.AuditEntry{}},
	"GET /api/v1/api-tokens":             {Summary: "Every user's API tokens (never the secret)", Tag: "Users", Resp: []auth.AdminTokenInfo{}},
	"DELETE /api/v1/api-tokens/{id}":     {Summary: "Revoke any API token", Tag: "Users"},
	"POST /api/v1/me/password":           {Summary: "Change my password", Tag: "Me", Bind: bindSelf, Body: docOwnPassword{}},
	"POST /api/v1/me/totp":               {Summary: "Begin two-factor enrolment (password needed when the sign-in is older than 10 minutes)", Tag: "Me", Bind: bindSelf, Body: docPasswordOpt{}, Resp: auth.TOTPEnrolment{}},
	"POST /api/v1/me/totp/confirm":       {Summary: "Confirm two-factor enrolment; returns recovery codes once (password needed when the sign-in is older than 10 minutes)", Tag: "Me", Bind: bindSelf, Body: docCodeReauth{}, Resp: docRecovery{}},
	"DELETE /api/v1/me/totp":             {Summary: "Remove my two-factor enrolment (needs the password)", Tag: "Me", Bind: bindSelf, Body: docPasswordOnly{}},
	"GET /api/v1/me/sessions":            {Summary: "My sessions", Tag: "Me", Bind: bindSelf, Resp: []auth.SessionInfo{}},
	"DELETE /api/v1/me/sessions/{id}":    {Summary: "End one of my sessions", Tag: "Me", Bind: bindSelf},
	"GET /api/v1/me/tokens":              {Summary: "My API tokens", Tag: "Me", Bind: bindSelf, Resp: []auth.TokenInfo{}},
	"POST /api/v1/me/tokens":             {Summary: "Create an API token (optionally restricted to repositories; 90 days unless ttl_hours, at most 365 days; password needed when the sign-in is older than 10 minutes; not for single sign-on accounts); the secret is shown once", Tag: "Me", Bind: bindSelf, Body: docMeToken{}, Resp: docMeTokenResp{}, Status: 201},
	"DELETE /api/v1/me/tokens/{id}":      {Summary: "Revoke one of my API tokens", Tag: "Me", Bind: bindSelf},

	// ---- jobs and restore tests
	"GET /api/v1/jobs":               {Summary: "Host jobs, newest first", Tag: "Jobs", Bind: bindList, ListKey: "repository", Query: []param{qp("repository", "string", "only this repository"), qp("host", "string", "host name"), qp("type", "string", "job type"), qp("state", "string", "comma-separated states"), limitQ}, Resp: []jobs.Job{}},
	"POST /api/v1/jobs":              {Summary: "Start a host job (restore, file listing...)", Tag: "Jobs", Bind: bindBody, Query: []param{repoQ}, Body: docJobCreate{}, Resp: jobs.Job{}, Status: 201},
	"GET /api/v1/jobs/{id}":          {Summary: "One job", Tag: "Jobs", Bind: bindJob, Resp: jobs.Job{}},
	"POST /api/v1/jobs/{id}/cancel":  {Summary: "Cancel a job", Tag: "Jobs", Bind: bindJob, Resp: jobs.Job{}},
	"GET /api/v1/jobs/{id}/delivery": {Summary: "A job's sealed result (IBS2, decrypted by the browser)", Tag: "Jobs", Bind: bindJob, Kind: kindBinary},
	"GET /api/v1/restore-tests":      {Summary: "Restore tests (latest=1: the latest per object of one repository)", Tag: "Jobs", Bind: bindList, ListKey: "repository", Query: []param{qp("repository", "string", "only this repository"), qp("latest", "string", "1 = latest per object"), qp("host", "string", "host name"), qp("object", "string", "project/name"), qp("state", "string", "state"), limitQ}, Resp: []jobs.RestoreTest{}},
	"POST /api/v1/restore-tests/run": {Summary: "Run a restore test now", Tag: "Jobs", Bind: bindBody, Query: []param{repoQ}, Body: docRestoreTestRun{}, Resp: jobs.Job{}, Status: 201},

	// ---- certificates
	"GET /api/v1/certificates":             {Summary: "CA, server and sync peer certificates with expiry", Tag: "Certificates", Resp: docCerts{}},
	"PUT /api/v1/certificates/sans":        {Summary: "Set the server certificate's subject alternative names", Tag: "Certificates", Body: docSANs{}},
	"POST /api/v1/certificates/ca/rotate":  {Summary: "Start a CA rotation (new CA trusted alongside the old)", Tag: "Certificates", Resp: docRotate{}},
	"POST /api/v1/certificates/ca/promote": {Summary: "Promote the next CA (force=1 even if hosts lag)", Tag: "Certificates", Query: []param{qp("force", "string", "1 = promote even when hosts have not renewed")}},

	// ---- notifications and alerts
	"GET /api/v1/notifications/channels":               {Summary: "Notification channels (secrets masked)", Tag: "Notifications", Resp: []notify.Channel{}},
	"POST /api/v1/notifications/channels":              {Summary: "Create a channel", Tag: "Notifications", Body: notify.ChannelInput{}, Resp: notify.Channel{}, Status: 201},
	"PUT /api/v1/notifications/channels/{id}":          {Summary: "Replace a channel", Tag: "Notifications", Body: notify.ChannelInput{}, Resp: notify.Channel{}},
	"DELETE /api/v1/notifications/channels/{id}":       {Summary: "Delete a channel", Tag: "Notifications"},
	"POST /api/v1/notifications/channels/{id}/test":    {Summary: "Send a test message (the receiver's answer is the result)", Tag: "Notifications", Resp: docChannelTest{}},
	"GET /api/v1/notifications/rules":                  {Summary: "Routing rules", Tag: "Notifications", Resp: []notify.Rule{}},
	"POST /api/v1/notifications/rules":                 {Summary: "Create a rule", Tag: "Notifications", Body: notify.Rule{}, Resp: notify.Rule{}, Status: 201},
	"PUT /api/v1/notifications/rules/{id}":             {Summary: "Replace a rule", Tag: "Notifications", Body: notify.Rule{}, Resp: notify.Rule{}},
	"DELETE /api/v1/notifications/rules/{id}":          {Summary: "Delete a rule", Tag: "Notifications"},
	"GET /api/v1/notifications/deliveries":             {Summary: "Delivery attempts", Tag: "Notifications", Query: []param{qp("channel", "string", "channel id"), qp("state", "string", "state"), qp("alert", "integer", "alert id"), limitQ}, Resp: []notify.Delivery{}},
	"POST /api/v1/notifications/deliveries/{id}/retry": {Summary: "Retry a failed delivery", Tag: "Notifications"},
	"GET /api/v1/notifications/settings":               {Summary: "Engine settings and alert conditions", Tag: "Notifications", Resp: docNotifySettings{}},
	"PUT /api/v1/notifications/settings":               {Summary: "Set engine settings", Tag: "Notifications", Body: notify.Settings{}},
	"PUT /api/v1/notifications/conditions/{type}":      {Summary: "Enable, disable or tune one alert condition", Tag: "Notifications", Body: docCondition{}},
	"POST /api/v1/notifications/tick":                  {Summary: "Run one evaluation and delivery round now", Tag: "Notifications", Resp: notify.TickResult{}},
	"POST /api/v1/notifications/simulate":              {Summary: "Which rules and channels an event would reach", Tag: "Notifications", Body: notify.SimulateInput{}, Resp: notify.SimulateResult{}},
	"GET /api/v1/alerts":                               {Summary: "Alerts the caller may see", Tag: "Alerts", Bind: bindList, ListKey: "repository", Query: []param{qp("type", "string", "alert type"), qp("severity", "string", "warning or critical"), qp("host", "string", "host"), qp("state", "string", "comma-separated states"), qp("active", "string", "1 = firing or acknowledged only"), limitQ}, Resp: []notify.Alert{}},
	"GET /api/v1/alerts/{id}":                          {Summary: "One alert with its history", Tag: "Alerts", Bind: bindAlert, Resp: notify.AlertDetail{}},
	"POST /api/v1/alerts/{id}/ack":                     {Summary: "Acknowledge (or undo=true: unacknowledge) an alert", Tag: "Alerts", Bind: bindAlert, Body: docAck{}, Resp: notify.Alert{}},
	"GET /api/v1/silences":                             {Summary: "Silences (all=1 includes ended ones)", Tag: "Alerts", Query: []param{qp("all", "string", "1 = include ended")}, Resp: []notify.Silence{}},
	"POST /api/v1/silences":                            {Summary: "Create a silence", Tag: "Alerts", Body: notify.Silence{}, Resp: notify.Silence{}, Status: 201},
	"DELETE /api/v1/silences/{id}":                     {Summary: "End a silence", Tag: "Alerts"},
}

// ---- request and response shapes of handlers that use inline structs (mirrors; keep in sync) ----

type docPing struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}
type docCreateRepo struct {
	Name       string              `json:"name"`
	Kind       string              `json:"kind" doc:"filesystem or s3"`
	Path       string              `json:"path,omitempty"`
	S3         *service.S3Settings `json:"s3,omitempty"`
	Credential string              `json:"credential,omitempty"`
	AccessKey  string              `json:"access_key,omitempty"`
	SecretKey  string              `json:"secret_key,omitempty"`
	Adopt      bool                `json:"adopt,omitempty"`
}
type docCred struct {
	Name      string `json:"name"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}
type docMode struct {
	Mode   string `json:"mode" doc:"online, read-only or offline"`
	Reason string `json:"reason"`
}
type docEnrolReq struct {
	Host     string  `json:"host"`
	Scope    string  `json:"scope" doc:"backup or restore"`
	TTLHours float64 `json:"ttl_hours"`
}
type docEnrolResp struct {
	Token          string  `json:"token"`
	ExpiresInHours float64 `json:"expires_in_hours"`
	Note           string  `json:"note"`
}
type docOnboarding struct {
	Token          string  `json:"token"`
	Server         string  `json:"server"`
	Command        string  `json:"command"`
	ExpiresInHours float64 `json:"expires_in_hours"`
	Install        string  `json:"install"`
	Note           string  `json:"note"`
}
type docPolicyName struct {
	Policy string `json:"policy"`
}
type docHost struct {
	Host string `json:"host"`
}
type docNotes struct {
	Notes string `json:"notes"`
}
type docProtected struct {
	Protected bool `json:"protected"`
}
type docMaintenance struct {
	*service.MaintenanceSettings
	GCGraceHours float64 `json:"gc_grace_hours"`
}
type docMaintenancePut struct {
	Capacity            *string  `json:"capacity,omitempty"`
	Reconcile           *string  `json:"reconcile,omitempty"`
	Prune               *string  `json:"prune,omitempty"`
	GC                  *string  `json:"gc,omitempty"`
	Verify              *string  `json:"verify,omitempty"`
	Scrub               *string  `json:"scrub,omitempty"`
	GCGraceHours        *float64 `json:"gc_grace_hours,omitempty"`
	DefaultReverifyDays *int     `json:"default_reverify_days,omitempty"`
}
type docVerifyReq struct {
	Mode         string `json:"mode" doc:"all, new or stale"`
	StaleDays    int    `json:"stale_days"`
	NoQuarantine bool   `json:"no_quarantine"`
}
type docVerifyResp struct {
	OK    bool                   `json:"ok"`
	Error string                 `json:"error,omitempty"`
	Stats *datastore.VerifyStats `json:"stats"`
}
type docSyncAdd struct {
	syncer.Job
	Token string `json:"token"`
}
type docCheckRemote struct {
	Remote string `json:"remote"`
	Token  string `json:"token"`
}
type docLogin struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Code     string `json:"code,omitempty"`
}
type docSetupNeeded struct {
	Needed bool `json:"needed"`
}
type docSetup struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Password string `json:"password"`
}
type docUserCreate struct {
	Name       string    `json:"name"`
	Password   string    `json:"password"`
	Display    string    `json:"display,omitempty"`
	Email      string    `json:"email,omitempty"`
	Role       auth.Role `json:"role" doc:"admin, operator, restore, readonly or none"`
	MustChange bool      `json:"must_change,omitempty"`
}
type docUserUpdate struct {
	Role     *auth.Role `json:"role,omitempty"`
	Display  *string    `json:"display_name,omitempty"`
	Email    *string    `json:"email,omitempty"`
	Disabled *bool      `json:"disabled,omitempty"`
}
type docSetPassword struct {
	Password   string `json:"password"`
	MustChange bool   `json:"must_change"`
}
type docAccessPut struct {
	Scoped bool            `json:"scoped"`
	Scopes []auth.RepoRole `json:"scopes"`
}
type docOwnPassword struct {
	Current string `json:"current"`
	New     string `json:"new"`
}
type docCode struct {
	Code string `json:"code"`
}
type docCodeReauth struct {
	Code     string `json:"code"`
	Password string `json:"password,omitempty" doc:"needed when the sign-in is older than 10 minutes"`
}
type docPasswordOnly struct {
	Password string `json:"password"`
}
type docRecovery struct {
	RecoveryCodes []string `json:"recovery_codes"`
	Note          string   `json:"note"`
}
type docMeToken struct {
	Name         string    `json:"name"`
	Role         auth.Role `json:"role,omitempty"`
	TTLHours     float64   `json:"ttl_hours,omitempty" doc:"0 or missing = 90 days; at most 365 days (8760 hours)"`
	Repositories []string  `json:"repositories,omitempty" doc:"restrict the token to these repositories (no global rights)"`
	Password     string    `json:"password,omitempty" doc:"needed when the sign-in is older than 10 minutes"`
}
type docMeTokenResp struct {
	Token string         `json:"token"`
	Info  auth.TokenInfo `json:"info"`
	Note  string         `json:"note"`
}
type docJobCreate struct {
	Type       string          `json:"type"`
	Host       string          `json:"host"`
	Repository string          `json:"repository,omitempty"`
	Params     json.RawMessage `json:"params"`
}
type docRestoreTestRun struct {
	Host       string `json:"host"`
	Repository string `json:"repository,omitempty"`
	Project    string `json:"project"`
	Object     string `json:"object"`
	Snapshot   string `json:"snapshot,omitempty"`
	Mode       string `json:"mode,omitempty"`
}
type docCerts struct {
	Report service.CertReport `json:"report"`
	Web    map[string]any     `json:"web,omitempty"`
}
type docSANs struct {
	SANs []string `json:"sans"`
}
type docRotate struct {
	NewCAFingerprint string `json:"new_ca_fingerprint"`
}
type docChannelTest struct {
	OK       bool             `json:"ok"`
	Error    string           `json:"error,omitempty"`
	Delivery *notify.Delivery `json:"delivery"`
}
type docNotifySettings struct {
	Settings   notify.Settings    `json:"settings"`
	Conditions []notify.Condition `json:"conditions"`
}
type docCondition struct {
	Enabled  bool            `json:"enabled"`
	Severity string          `json:"severity"`
	Params   json.RawMessage `json:"params"`
}
type docAck struct {
	Comment string `json:"comment,omitempty"`
	Undo    bool   `json:"undo,omitempty"`
}
