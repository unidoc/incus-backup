package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/jobs"
	"github.com/unidoc/incus-backup/internal/jobspec"
)

// restore-test: run restore tests by hand (the daemon runs them on the policies' restore_test_schedule).
func (c *cli) cmdRestoreTest() *cobra.Command {
	root := &cobra.Command{Use: "restore-test", Short: "Prove that backups restore (deep, extract or full restore into scratch objects)"}
	var object, project, mode, server string
	var all bool
	run := &cobra.Command{Use: "run", Short: "Run restore tests now: one object (--object), every object (--all), or what the schedule has due", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			a, err := c.load(ctx, object == "" || mode == jobspec.ModeFull) // deep/extract of one object work without Incus
			if err != nil {
				return err
			}
			var planned []jobs.Planned
			switch {
			case object != "":
				targets := a.Cfg.ServerNames()
				if server != "" {
					targets = []string{server}
				}
				m := mode
				if m == "" {
					m = jobspec.ModeDeep
				}
				for _, t := range targets {
					planned = append(planned, jobs.Planned{TestObject: jobs.TestObject{Target: t, Project: project, Name: object}, Mode: m})
				}
			default:
				objs, err := a.Plan(ctx)
				if err != nil {
					return err
				}
				if all {
					hist, _ := a.History()
					recs, _ := hist.Snapshot()
					for _, o := range a.RestoreTestObjects(objs) {
						if server != "" && o.Target != server {
							continue
						}
						m := mode
						if m == "" {
							m = jobs.ModeFor(o.Kind, recs[o.Key()].Count)
						}
						planned = append(planned, jobs.Planned{TestObject: o, Mode: m})
					}
				} else {
					p, _, err := a.PlanRestoreTests(objs, a.Eng.Now())
					if err != nil {
						return err
					}
					planned = p
				}
			}
			if len(planned) == 0 {
				fmt.Println("Nothing to test (use --object or --all).")
				return nil
			}
			failed := 0
			for _, o := range a.RunRestoreTests(ctx, planned) {
				if o.Result == nil {
					fmt.Printf("FAIL %v\n", o.Err)
					failed++
					continue
				}
				r := o.Result
				status := "ok  "
				if o.Err != nil {
					status = "FAIL"
					failed++
				}
				fmt.Printf("%s %-8s %-7s %s/%s@%s  %s, %d chunk(s), %d file(s), %s\n", status, o.Target, r.Method, r.Project, r.Object, r.Snapshot,
					human(r.Bytes), r.Chunks, r.Files, time.Duration(r.DurationMs)*time.Millisecond)
				if o.Err != nil {
					fmt.Printf("     %s\n", engine.Detail(o.Err))
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d restore test(s) failed", failed)
			}
			return nil
		}}
	run.Flags().StringVar(&object, "object", "", "storage object, e.g. instance/vm1/root or volume/data")
	run.Flags().StringVar(&project, "project", "default", "Incus project of --object")
	run.Flags().StringVar(&mode, "mode", "", "deep | extract | full (default: deep for --object, the schedule's choice otherwise)")
	run.Flags().StringVar(&server, "server", "", "only this server")
	run.Flags().BoolVar(&all, "all", false, "test every backed-up object now")
	list := &cobra.Command{Use: "list", Short: "Show the local restore-test history", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := c.load(cmd.Context(), false)
			if err != nil {
				return err
			}
			h, err := a.History()
			if err != nil {
				return err
			}
			recs, _ := h.Snapshot()
			keys := make([]string, 0, len(recs))
			for k := range recs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				r := recs[k]
				st := "passed"
				if !r.Passed {
					st = "FAILED"
				}
				fmt.Printf("%-50s %-7s %-7s %s  (%d test(s)) %s\n", strings.Replace(k, "|", " ", 1), r.Mode, st, r.Last.Local().Format(time.RFC3339), r.Count, r.Error)
			}
			return nil
		}}
	root.AddCommand(run, list)
	return root
}
