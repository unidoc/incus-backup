package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/unidoc/incus-backup/internal/client/app"
	"github.com/unidoc/incus-backup/internal/client/config"
)

// certNow is the clock of the cert commands (tests replace it).
var certNow = time.Now

func (c *cli) cmdCert() *cobra.Command {
	root := &cobra.Command{Use: "cert", Short: "Client certificates (renewed automatically by the daemon)"}
	status := &cobra.Command{Use: "status", Short: "Expiry and renewal state of the certificate for each server", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(c.cfgPath)
			if err != nil {
				return err
			}
			tw := table()
			fmt.Fprintln(tw, "SERVER\tSTATE\tEXPIRES\tDAYS\tISSUER\tLAST RENEWAL\tLAST ERROR")
			bad := 0
			for _, s := range app.CertStatuses(cfg, certNow()) {
				last := "never"
				if !s.LastSuccess.IsZero() {
					last = s.LastSuccess.Format("2006-01-02 15:04")
				}
				st := s.State
				if s.ConfirmPending {
					st += " (confirm pending)"
				}
				if s.Err != nil {
					s.LastError = s.Err.Error()
				}
				if s.State == "expired" || s.State == "missing" {
					bad++
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", s.Server, st, s.NotAfter.Format("2006-01-02"), s.DaysLeft, s.Issuer, last, dashIfEmpty(s.LastError))
			}
			_ = tw.Flush()
			if bad > 0 {
				return fmt.Errorf("%d certificate(s) expired or unreadable", bad)
			}
			return nil
		}}
	var server string
	renew := &cobra.Command{Use: "renew", Short: "Renew the client certificate now (every server, or --server)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(c.cfgPath)
			if err != nil {
				return err
			}
			out, err := app.RenewNow(cmd.Context(), cfg, server, certNow, c.log)
			for n, o := range out {
				conf := "confirmed, the old certificate is revoked"
				if !o.Confirmed {
					conf = "NOT confirmed yet (the daemon retries; the old certificate stays valid until then)"
				}
				fmt.Printf("%s: renewed, valid until %s, %s\n", n, o.NotAfter.Format("2006-01-02"), conf)
			}
			return err
		}}
	renew.Flags().StringVar(&server, "server", "", "only this server (as named in the configuration)")
	root.AddCommand(status, renew)
	return root
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
