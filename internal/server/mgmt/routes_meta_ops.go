package mgmt

import (
	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/sysinfo"
	"github.com/unidoc/incus-backup/internal/server/tasks"
)

// Route metadata of the operations routes (tasks, system, metrics, repository lifecycle): see routes_meta.go.
func init() {
	type removeBody struct {
		Mode string `json:"mode"`
		service.RemoveRequest
	}
	type groupDelete struct {
		ForceProtected bool `json:"force_protected"`
	}
	apiRoutes["DELETE /api/v1/repositories/{name}"] = apiRoute{Summary: "Detach (forget, keep the data) or destroy (delete every backup in) an offline repository", Tag: "Repositories", Bind: bindPath, Body: removeBody{}, Status: 202}
	apiRoutes["DELETE /api/v1/objects/{group}"] = apiRoute{Summary: "Delete a whole backup group (all snapshots) as a task", Tag: "Objects", Bind: bindQuery, Query: []param{repoQ}, Body: groupDelete{}, Status: 202}

	apiRoutes["GET /api/v1/tasks"] = apiRoute{Summary: "Tasks (long operations), newest first", Tag: "Tasks", Bind: bindList, ListKey: "repository",
		Query: []param{qp("state", "string", "queued, running, succeeded, failed or cancelled"), qp("kind", "string", "task kind"), repoQ, qp("started_by", "string", "actor"), qp("before", "integer", "id cursor"), limitQ}, Resp: []tasks.Task{}}
	apiRoutes["GET /api/v1/tasks/{id}"] = apiRoute{Summary: "A task with its log (?after=<seq> tails it)", Tag: "Tasks", Query: []param{qp("after", "integer", "log sequence number to continue after"), limitQ}, Resp: tasks.Detail{}}
	apiRoutes["POST /api/v1/tasks/{id}/cancel"] = apiRoute{Summary: "Cancel a task", Tag: "Tasks", Resp: tasks.Detail{}}

	apiRoutes["GET /api/v1/metrics"] = apiRoute{Summary: "Prometheus text exposition (use a bearer token)", Tag: "System", Kind: kindBinary}
	apiRoutes["GET /api/v1/system/status"] = apiRoute{Summary: "Host, resources, database, listeners and version of this server", Tag: "System", Resp: SystemStatus{}}
	apiRoutes["GET /api/v1/system/samples"] = apiRoute{Summary: "Per-minute resource samples (7 days)", Tag: "System", Query: []param{qp("hours", "number", "history length"), qp("points", "integer", "maximum points")}, Resp: []sysinfo.Sample{}}
	apiRoutes["GET /api/v1/system/logs"] = apiRoute{Summary: "Recent server log lines (secrets redacted)", Tag: "System", Query: []param{qp("level", "string", "minimum level"), qp("q", "string", "substring filter"), qp("after", "integer", "sequence cursor"), limitQ}}
	apiRoutes["POST /api/v1/system/support-report"] = apiRoute{Summary: "Download a redacted support report (.tar.gz)", Tag: "System", Kind: kindBinary}
	apiRoutes["GET /api/v1/system/update"] = apiRoute{Summary: "Update notice state (the check is off unless enabled)", Tag: "System"}
	apiRoutes["POST /api/v1/system/update/check"] = apiRoute{Summary: "Check for a newer release now", Tag: "System"}
}
