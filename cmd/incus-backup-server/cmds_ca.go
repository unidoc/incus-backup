package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unidoc/incus-backup/internal/server/service"
)

func (a *app) cmdCA() *cobra.Command {
	root := &cobra.Command{Use: "ca", Short: "Certificates: CA, server certificate, host and sync-peer certificates (docs/certificates.md)"}
	var asJSON bool
	status := &cobra.Command{Use: "status", Short: "Expiry and renewal state of every certificate", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			r, err := a.svc.CertificateStatus(ctx)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(r)
			}
			printCertReport(r)
			if r.Level == "critical" {
				return fmt.Errorf("a certificate needs attention now")
			}
			return nil
		}}
	status.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")

	sans := &cobra.Command{Use: "sans [NAME|IP ...]", Short: "Show the server certificate's SANs, or replace them (the certificate is reissued and served without a restart)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			if len(args) > 0 {
				var list []string
				for _, v := range args {
					list = append(list, strings.Split(v, ",")...)
				}
				if err := a.svc.SetServerSANs(ctx, list); err != nil {
					return err
				}
			}
			s, err := a.svc.ServerSANs(ctx)
			if err != nil {
				return err
			}
			fmt.Println(strings.Join(s, "\n"))
			return nil
		}}
	rotate := &cobra.Command{Use: "rotate", Short: "Start a CA rotation: a new CA signs every new client certificate; clients pick it up on renewal", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			fp, err := a.svc.RotateCA(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("New CA %s created and trusted.\nClients and sync peers renew from it within a day (their daemons check daily).\nWatch `incus-backup-server ca status`; when no identity is pending run `incus-backup-server ca promote`.\n", fp)
			return nil
		}}
	var force bool
	promote := &cobra.Command{Use: "promote", Short: "Finish a CA rotation: present the new CA, reissue the server certificate, stop trusting the old CA", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			if err := a.svc.PromoteCA(ctx, force); err != nil {
				return err
			}
			fp, _ := a.svc.CAFingerprint()
			fmt.Printf("The new CA %s is now presented. New tokens pin it.\n", fp)
			return nil
		}}
	promote.Flags().BoolVar(&force, "force", false, "promote although some identities have not renewed (they will stop working and must enrol again)")
	var job string
	renewPeer := &cobra.Command{Use: "renew-peer", Short: "Renew this server's certificate at the peer of a sync job now", Args: cobra.NoArgs,
		RunE: a.withRepo(func(ctx context.Context, repo string, _ []string) error {
			o, err := a.sync.RenewPeerNow(ctx, repo, job)
			if err != nil {
				return err
			}
			fmt.Printf("Renewed: valid until %s (confirmed: %v)\n", o.NotAfter.Format("2006-01-02"), o.Confirmed)
			return nil
		})}
	renewPeer.Flags().StringVar(&job, "job", "", "sync job name")
	_ = renewPeer.MarkFlagRequired("job")
	root.AddCommand(status, sans, rotate, promote, renewPeer)
	return root
}

func printCertReport(r *service.CertReport) {
	line := func(what string, c service.CertSummary, extra string) {
		fmt.Printf("%-9s %-40s %s  %4dd  %-8s %s\n", strings.ToUpper(c.Level), what, c.NotAfter.Format("2006-01-02"), c.DaysLeft, c.State, extra)
	}
	line("CA", r.CA, r.CA.Subject+" "+r.CA.Fingerprint)
	if r.Rotation != nil {
		line("next CA (rotation)", r.Rotation.Next, fmt.Sprintf("%s, %d identity(ies) pending: %s", r.Rotation.Next.Fingerprint[:16], len(r.Rotation.Pending), strings.Join(r.Rotation.Pending, ", ")))
	}
	extra := "SANs " + strings.Join(r.ServerSANs, ",")
	if strings.Join(r.ServerSANs, ",") != strings.Join(r.WantSANs, ",") {
		extra += " (configured: " + strings.Join(r.WantSANs, ",") + "; reissued by the daemon)"
	}
	line("server certificate", r.Server, extra)
	for _, h := range r.Hosts {
		e := "renewed " + ts(h.LastRenewed)
		if h.Disabled {
			e += ", DISABLED"
		}
		if h.OtherValid > 0 {
			e += fmt.Sprintf(", %d older certificate(s) still valid (unconfirmed renewal)", h.OtherValid)
		}
		if h.Untrusted {
			e += ", issued by a CA that is no longer trusted: enrol again with a new token"
		}
		if h.OnOldCA {
			e += ", not yet on the new CA"
		}
		line(fmt.Sprintf("%s/%s (%s)", h.Repository, h.Name, h.Scope), h.CertSummary, e)
	}
	for _, p := range r.SyncPeers {
		line("sync peer "+p.Repository+"/"+p.Job, p.CertSummary, p.Error)
	}
	fmt.Printf("overall: %s\n", r.Level)
}
