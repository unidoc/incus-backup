package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/unidoc/incus-backup/internal/server/auth"
)

// addUserAuthCommands adds the passkey reset and the local sign-in switch (docs/authentication.md):
// both work on the database directly, so they are the break-glass path when nobody can sign in.
func addUserAuthCommands(root *cobra.Command, run func(func(context.Context, *auth.Store, []string) error) func(*cobra.Command, []string) error) {
	root.AddCommand(&cobra.Command{
		Use: "webauthn-reset <name>", Short: "Remove every passkey and security key of a user who lost them", Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, st *auth.Store, args []string) error {
			n, err := st.ResetWebAuthn(ctx, args[0])
			if err != nil {
				return err
			}
			_ = st.Audit(ctx, auth.AuditEntry{Actor: "local-cli", Kind: "local", Action: "user.webauthn-reset", Target: args[0], OK: true,
				Detail: []byte(fmt.Sprintf(`{"removed":%d}`, n))})
			fmt.Printf("Removed %d passkey(s) / security key(s) of %s.\n", n, args[0])
			return nil
		}),
	})
	root.AddCommand(&cobra.Command{
		Use: "local-login <on|off>", Short: "Allow or refuse password and passkey sign-in on the web listener (off: single sign-on only)", Args: cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: run(func(ctx context.Context, st *auth.Store, args []string) error {
			var on bool
			switch args[0] {
			case "on":
				on = true
			case "off":
			default:
				return fmt.Errorf("local-login takes on or off")
			}
			if err := st.SetPolicy(ctx, auth.PolicyUpdate{LocalLogin: &on}); err != nil {
				return err
			}
			_ = st.Audit(ctx, auth.AuditEntry{Actor: "local-cli", Kind: "local", Action: "auth.local-login", Target: args[0], OK: true})
			fmt.Printf("Password and passkey sign-in: %s.\n", args[0])
			return nil
		}),
	})
}
