package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unidoc/incus-backup/internal/server/service"
	"github.com/unidoc/incus-backup/internal/server/tasks"
)

// Operations commands. Tasks, host jobs, alerts and the audit log live in the daemon and PostgreSQL;
// these commands are thin clients of the management API over the admin socket (the daemon must run).
// Repository mode, detach/destroy and group delete call the service directly when no daemon runs, and
// hand the request to the daemon otherwise (like gc and prune).

// apiCmd runs fn with the configuration loaded (for the admin socket path) but without touching the
// database: everything goes through the daemon.
func (a *app) apiCmd(fn func(ctx context.Context, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, err := a.open(cmd)
		if err != nil {
			return err
		}
		defer a.close()
		return fn(ctx, args)
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ---- task ------------------------------------------------------------------------------------

func (a *app) cmdTask() *cobra.Command {
	root := &cobra.Command{Use: "task", Short: "Server tasks: GC, verify, retention, reconcile, sync runs, destroys (list, show with log, cancel)"}
	var state, kind string
	var limit int
	var asJSON bool
	list := &cobra.Command{Use: "list", Short: "List tasks, newest first", Args: cobra.NoArgs,
		RunE: a.apiCmd(func(ctx context.Context, _ []string) error {
			q := url.Values{}
			if state != "" {
				q.Set("state", state)
			}
			if kind != "" {
				q.Set("kind", kind)
			}
			if a.repo != "" {
				q.Set("repository", a.repo)
			}
			q.Set("limit", fmt.Sprint(limit))
			var ts []tasks.Task
			if err := a.adminCall(ctx, "GET", "/api/v1/tasks", q, nil, &ts); err != nil {
				return err
			}
			if asJSON {
				return printJSON(ts)
			}
			tw := table()
			fmt.Fprintln(tw, "ID\tKIND\tREPOSITORY\tSTATE\tPROGRESS\tSTARTED BY\tSTARTED\tMESSAGE")
			for _, t := range ts {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d%%\t%s\t%s\t%s\n", t.ID, t.Kind, t.Repository, t.State, t.Progress, t.StartedBy, ago(&t.Created), short(firstNonEmpty(t.Error, t.Message), 60))
			}
			return tw.Flush()
		})}
	list.Flags().StringVar(&state, "state", "", "queued, running, succeeded, failed, cancelled or active")
	list.Flags().StringVar(&kind, "kind", "", "gc, verify, scrub, retention, reconcile, sync, capacity, probe, repository_destroy, group_delete")
	list.Flags().IntVar(&limit, "limit", 50, "at most this many")
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	var follow bool
	show := &cobra.Command{Use: "show ID", Short: "Show a task and its log (--follow tails it until it ends)", Args: cobra.ExactArgs(1),
		RunE: a.apiCmd(func(ctx context.Context, args []string) error {
			after := 0
			first := true
			for {
				var d tasks.Detail
				if err := a.adminCall(ctx, "GET", "/api/v1/tasks/"+url.PathEscape(args[0]), url.Values{"after": {fmt.Sprint(after)}, "limit": {"5000"}}, nil, &d); err != nil {
					return err
				}
				if first {
					if asJSON && !follow {
						return printJSON(d)
					}
					fmt.Printf("Task %s: %s on %s, started by %s at %s\n", d.ID, d.Kind, firstNonEmpty(d.Repository, "-"), d.StartedBy, ago(&d.Created))
					first = false
				}
				for _, l := range d.Log {
					fmt.Printf("%s %-5s %s\n", l.At.Local().Format("15:04:05"), strings.ToUpper(l.Level), l.Line)
				}
				after = d.NextSeq
				if d.Done() || !follow {
					fmt.Printf("State: %s (%d%%) %s\n", d.State, d.Progress, d.Message)
					if d.Error != "" {
						fmt.Printf("Error: %s\n", d.Error)
					}
					if len(d.Result) > 0 && string(d.Result) != "null" {
						fmt.Printf("Result: %s\n", d.Result)
					}
					if d.State == tasks.Failed {
						return errors.New("the task failed")
					}
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
			}
		})}
	show.Flags().BoolVarP(&follow, "follow", "f", false, "follow the log until the task ends")
	show.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cancel := &cobra.Command{Use: "cancel ID", Short: "Cancel a running task (it stops at its next check and ends as cancelled)", Args: cobra.ExactArgs(1),
		RunE: a.apiCmd(func(ctx context.Context, args []string) error {
			var t tasks.Task
			if err := a.adminCall(ctx, "POST", "/api/v1/tasks/"+url.PathEscape(args[0])+"/cancel", nil, nil, &t); err != nil {
				return err
			}
			fmt.Printf("Cancel requested for task %s (%s): state %s\n", t.ID, t.Kind, t.State)
			return nil
		})}
	root.AddCommand(list, show, cancel)
	return root
}

// ---- jobs ------------------------------------------------------------------------------------

func (a *app) cmdJobs() *cobra.Command {
	root := &cobra.Command{Use: "jobs", Short: "Host jobs (backup now, restore tests, deep verify ...): list, create, cancel"}
	var host, typ, state, params string
	var limit int
	var asJSON bool
	type job struct {
		ID         string     `json:"id"`
		Repository string     `json:"repository"`
		Host       string     `json:"host"`
		Type       string     `json:"type"`
		State      string     `json:"state"`
		Progress   int        `json:"progress"`
		Message    string     `json:"message"`
		CreatedBy  string     `json:"created_by"`
		CreatedAt  time.Time  `json:"created_at"`
		Finished   *time.Time `json:"finished_at"`
	}
	list := &cobra.Command{Use: "list", Short: "List host jobs, newest first", Args: cobra.NoArgs,
		RunE: a.apiCmd(func(ctx context.Context, _ []string) error {
			q := url.Values{"limit": {fmt.Sprint(limit)}}
			for k, v := range map[string]string{"host": host, "type": typ, "state": state, "repository": a.repo} {
				if v != "" {
					q.Set(k, v)
				}
			}
			var raw json.RawMessage
			if err := a.adminCall(ctx, "GET", "/api/v1/jobs", q, nil, &raw); err != nil {
				return err
			}
			if asJSON {
				fmt.Println(string(raw))
				return nil
			}
			var js []job
			if err := json.Unmarshal(raw, &js); err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "ID\tTYPE\tHOST\tREPOSITORY\tSTATE\tPROGRESS\tCREATED BY\tCREATED\tMESSAGE")
			for _, j := range js {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d%%\t%s\t%s\t%s\n", j.ID, j.Type, j.Host, j.Repository, j.State, j.Progress, j.CreatedBy, ago(&j.CreatedAt), short(j.Message, 60))
			}
			return tw.Flush()
		})}
	list.Flags().StringVar(&host, "host", "", "only this host")
	list.Flags().StringVar(&typ, "type", "", "only this job type")
	list.Flags().StringVar(&state, "state", "", "comma-separated states (queued,running,succeeded,failed,cancelled,expired)")
	list.Flags().IntVar(&limit, "limit", 50, "at most this many")
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	create := &cobra.Command{Use: "create TYPE HOST", Short: "Queue a job for a host (types: backup_now, restore_test, deep_verify, ...; see docs/jobs.md)", Args: cobra.ExactArgs(2),
		RunE: a.apiCmd(func(ctx context.Context, args []string) error {
			body := map[string]any{"type": args[0], "host": args[1]}
			if a.repo != "" {
				body["repository"] = a.repo
			}
			if params != "" {
				var p json.RawMessage
				if err := json.Unmarshal([]byte(params), &p); err != nil {
					return fmt.Errorf("--params must be a JSON object: %w", err)
				}
				body["params"] = p
			}
			var j job
			if err := a.adminCall(ctx, "POST", "/api/v1/jobs", nil, body, &j); err != nil {
				return err
			}
			fmt.Printf("Job %s queued (%s on %s). Follow it with `jobs list` or in the web UI.\n", j.ID, j.Type, j.Host)
			return nil
		})}
	create.Flags().StringVar(&params, "params", "", `job parameters as JSON, e.g. '{"project":"default"}' for backup_now`)
	cancel := &cobra.Command{Use: "cancel ID", Short: "Cancel a queued or running host job", Args: cobra.ExactArgs(1),
		RunE: a.apiCmd(func(ctx context.Context, args []string) error {
			if err := a.adminCall(ctx, "POST", "/api/v1/jobs/"+url.PathEscape(args[0])+"/cancel", nil, nil, nil); err != nil {
				return err
			}
			fmt.Printf("Cancel requested for job %s\n", args[0])
			return nil
		})}
	root.AddCommand(list, create, cancel)
	return root
}

// ---- alerts ----------------------------------------------------------------------------------

func (a *app) cmdAlerts() *cobra.Command {
	root := &cobra.Command{Use: "alerts", Short: "Alerts: list and acknowledge"}
	var all, asJSON bool
	var severity string
	list := &cobra.Command{Use: "list", Short: "List active alerts (--all: also resolved ones)", Args: cobra.NoArgs,
		RunE: a.apiCmd(func(ctx context.Context, _ []string) error {
			q := url.Values{"limit": {"500"}}
			if !all {
				q.Set("active", "1")
			}
			if severity != "" {
				q.Set("severity", severity)
			}
			var raw json.RawMessage
			if err := a.adminCall(ctx, "GET", "/api/v1/alerts", q, nil, &raw); err != nil {
				return err
			}
			if asJSON {
				fmt.Println(string(raw))
				return nil
			}
			var as []struct {
				ID       int64     `json:"id"`
				Type     string    `json:"type"`
				Severity string    `json:"severity"`
				State    string    `json:"state"`
				Title    string    `json:"title"`
				LastSeen time.Time `json:"last_seen"`
				AckedBy  string    `json:"acked_by"`
			}
			if err := json.Unmarshal(raw, &as); err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "ID\tSEVERITY\tSTATE\tTYPE\tLAST SEEN\tTITLE")
			for _, x := range as {
				st := x.State
				if x.AckedBy != "" {
					st += " (" + x.AckedBy + ")"
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", x.ID, x.Severity, st, x.Type, ago(&x.LastSeen), short(x.Title, 80))
			}
			return tw.Flush()
		})}
	list.Flags().BoolVar(&all, "all", false, "include resolved alerts")
	list.Flags().StringVar(&severity, "severity", "", "only info, warning or critical")
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	var comment string
	var undo bool
	ack := &cobra.Command{Use: "ack ID", Short: "Acknowledge an alert (stops repeat notifications; --undo reverts)", Args: cobra.ExactArgs(1),
		RunE: a.apiCmd(func(ctx context.Context, args []string) error {
			var out struct {
				State string `json:"state"`
				Title string `json:"title"`
			}
			if err := a.adminCall(ctx, "POST", "/api/v1/alerts/"+url.PathEscape(args[0])+"/ack", nil, map[string]any{"comment": comment, "undo": undo}, &out); err != nil {
				return err
			}
			fmt.Printf("Alert %s: %s (%s)\n", args[0], out.State, out.Title)
			return nil
		})}
	ack.Flags().StringVar(&comment, "comment", "", "why (shown to others)")
	ack.Flags().BoolVar(&undo, "undo", false, "remove the acknowledgement")
	root.AddCommand(list, ack)
	return root
}

// ---- audit -----------------------------------------------------------------------------------

func (a *app) cmdAudit() *cobra.Command {
	root := &cobra.Command{Use: "audit", Short: "The audit log of every change and sign-in"}
	var actor, action string
	var limit int
	var asJSON bool
	list := &cobra.Command{Use: "list", Short: "List audit entries, newest first", Args: cobra.NoArgs,
		RunE: a.apiCmd(func(ctx context.Context, _ []string) error {
			q := url.Values{"limit": {fmt.Sprint(limit)}}
			if actor != "" {
				q.Set("actor", actor)
			}
			if action != "" {
				q.Set("action", action)
			}
			var raw json.RawMessage
			if err := a.adminCall(ctx, "GET", "/api/v1/audit", q, nil, &raw); err != nil {
				return err
			}
			if asJSON {
				fmt.Println(string(raw))
				return nil
			}
			var es []struct {
				At     time.Time `json:"at"`
				Actor  string    `json:"actor"`
				Action string    `json:"action"`
				Target string    `json:"target"`
				IP     string    `json:"ip"`
				OK     bool      `json:"ok"`
			}
			if err := json.Unmarshal(raw, &es); err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "TIME\tACTOR\tACTION\tTARGET\tIP\tRESULT")
			for _, e := range es {
				res := "ok"
				if !e.OK {
					res = "FAILED"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", ago(&e.At), e.Actor, e.Action, e.Target, e.IP, res)
			}
			return tw.Flush()
		})}
	list.Flags().StringVar(&actor, "actor", "", "only this actor")
	list.Flags().StringVar(&action, "action", "", "only actions starting with this (e.g. \"delete /\")")
	list.Flags().IntVar(&limit, "limit", 100, "at most this many")
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	root.AddCommand(list)
	return root
}

// ---- repo mode / detach / destroy, object delete ------------------------------------------------

func (a *app) repoOpsCmds() []*cobra.Command {
	var reason string
	mode := &cobra.Command{Use: "mode NAME online|read-only|offline", Short: "Set a repository's maintenance mode", Args: cobra.ExactArgs(2),
		RunE: a.apiCmd(func(ctx context.Context, args []string) error {
			if err := a.svc.SetRepositoryMode(ctx, args[0], args[1], reason); err != nil {
				return err
			}
			fmt.Printf("Repository %s is %s (a running daemon applies it within 5 seconds).\n", args[0], args[1])
			return nil
		})}
	mode.Flags().StringVar(&reason, "reason", "", "why (shown in the UI and to hosts)")

	var req service.RemoveRequest
	var forceHosts string
	fill := func() {
		req.ForceHosts = nil
		for _, h := range strings.Split(forceHosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				req.ForceHosts = append(req.ForceHosts, h)
			}
		}
	}
	detach := &cobra.Command{Use: "detach NAME", Short: "Forget an OFFLINE repository in PostgreSQL and leave its datastore untouched (attach it again later)", Args: cobra.ExactArgs(1),
		RunE: a.apiCmd(func(ctx context.Context, args []string) error {
			fill()
			res, err := a.svc.DetachRepository(ctx, args[0], req)
			if errors.Is(err, service.ErrDaemonRunning) {
				res = &service.DetachResult{}
				err = a.adminCall(ctx, "DELETE", "/api/v1/repositories/"+url.PathEscape(args[0]), nil,
					map[string]any{"mode": "detach", "force_hosts": req.ForceHosts, "clear_owner": req.ClearOwner}, res)
			}
			if err != nil {
				return err
			}
			fmt.Printf("Repository %s detached; its datastore at %s is untouched (owner released: %v).\n", args[0], res.Location, res.OwnerCleared)
			return nil
		})}
	detach.Flags().BoolVar(&req.ClearOwner, "clear-owner", false, "release owner.json so another server can attach it without --adopt")
	detach.Flags().StringVar(&forceHosts, "force-hosts", "", "comma-separated list of EVERY enrolled host/peer name (they lose their enrolment)")
	destroy := &cobra.Command{Use: "destroy NAME", Short: "DELETE every backup of an OFFLINE repository (all datastore objects) and forget it", Args: cobra.ExactArgs(1),
		Long: `Deletes the whole datastore content (S3: every object under the prefix; filesystem: the directory's
contents) and then the repository's records. This cannot be undone. The repository must be offline
(repo mode NAME offline), --confirm must repeat its name, and every enrolled identity must be listed in
--force-hosts. With a running daemon the deletion runs there as a task (follow it with task show -f).`,
		RunE: a.apiCmd(func(ctx context.Context, args []string) error {
			fill()
			if _, err := a.svc.RemovePreflight(ctx, args[0], req, true); err != nil {
				return err
			}
			res, err := a.svc.DestroyRepository(ctx, args[0], req)
			if errors.Is(err, service.ErrDaemonRunning) {
				res = &service.DestroyResult{}
				err = a.adminCall(ctx, "DELETE", "/api/v1/repositories/"+url.PathEscape(args[0]), url.Values{"wait": {"1"}},
					map[string]any{"mode": "destroy", "confirm": req.Confirm, "force_hosts": req.ForceHosts}, res)
			}
			if err != nil {
				return err
			}
			fmt.Printf("Repository %s destroyed: %d objects (%s) deleted from %s.\n", args[0], res.ObjectsDeleted, human(res.BytesDeleted), res.Location)
			return nil
		})}
	destroy.Flags().StringVar(&req.Confirm, "confirm", "", "the repository name, typed again")
	destroy.Flags().StringVar(&forceHosts, "force-hosts", "", "comma-separated list of EVERY enrolled host/peer name")
	return []*cobra.Command{mode, detach, destroy}
}

func (a *app) cmdObjectDelete() *cobra.Command {
	var force bool
	var confirm string
	c := &cobra.Command{Use: "delete PROJECT/NAME", Short: "Delete a whole object: every snapshot of it (chunks are freed by the next GC)", Args: cobra.ExactArgs(1),
		RunE: a.withRepo(func(ctx context.Context, repo string, args []string) error {
			if confirm != args[0] {
				return fmt.Errorf("repeat the object with --confirm %s to delete every snapshot of it", args[0])
			}
			g, err := a.resolveObject(ctx, repo, args[0])
			if err != nil {
				return err
			}
			res, err := a.svc.DeleteGroup(ctx, repo, g, force)
			if errors.Is(err, service.ErrDaemonRunning) {
				res = &service.GroupDeleteResult{}
				q := waitQ(repo)
				err = a.adminCall(ctx, "DELETE", "/api/v1/objects/"+g, q, map[string]any{"force_protected": force}, res)
			}
			if err != nil {
				return err
			}
			fmt.Printf("Deleted %d snapshot(s) of %s (%d protected ones overruled). Run gc to free their chunks.\n", res.SnapshotsDeleted, args[0], res.ProtectedOverruled)
			return nil
		})}
	c.Flags().BoolVar(&force, "force-protected", false, "also delete protected snapshots")
	c.Flags().StringVar(&confirm, "confirm", "", "PROJECT/NAME again (typed confirmation)")
	return c
}
