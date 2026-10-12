package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/unidoc/incus-backup/internal/server/auth"
)

func (a *app) authStore() *auth.Store {
	return auth.New(a.db.Pool(), a.svc.Sealer("user-totp"))
}

// readPassword takes a password from --password-stdin, or prompts (twice) on a terminal.
func readPassword(fromStdin bool) (string, error) {
	if fromStdin {
		l, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && l == "" {
			return "", fmt.Errorf("reading the password from stdin: %w", err)
		}
		return strings.TrimRight(l, "\r\n"), nil
	}
	if !term.IsTerminal(int(syscall.Stdin)) {
		return "", fmt.Errorf("no terminal: pass --password-stdin")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	p1, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Again: ")
	p2, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(p1) != string(p2) {
		return "", fmt.Errorf("the passwords differ")
	}
	return string(p1), nil
}

func (a *app) cmdUser() *cobra.Command {
	root := &cobra.Command{Use: "user", Short: "Manage the people who may sign in to the web UI and API (database only; no daemon needed)"}
	run := func(fn func(ctx context.Context, st *auth.Store, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			ctx, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer a.close()
			return fn(ctx, a.authStore(), args)
		}
	}

	root.AddCommand(&cobra.Command{
		Use: "list", Short: "List users", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, st *auth.Store, _ []string) error {
			us, err := st.Users(ctx)
			if err != nil {
				return err
			}
			for _, u := range us {
				flags := ""
				if u.TOTP {
					flags += " 2fa"
				}
				if u.Disabled {
					flags += " disabled"
				}
				fmt.Printf("%-20s %-9s%s\n", u.Name, u.Role, flags)
			}
			return nil
		}),
	})

	var role string
	var stdin bool
	create := &cobra.Command{
		Use: "create <name>", Short: "Create a user (the first administrator can also be created in the web UI with the setup token)", Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, st *auth.Store, args []string) error {
			pw, err := readPassword(stdin)
			if err != nil {
				return err
			}
			u, err := st.CreateUser(ctx, args[0], pw, auth.Role(role), "", "", false)
			if err != nil {
				return err
			}
			_ = st.Audit(ctx, auth.AuditEntry{Actor: "local-cli", Kind: "local", Action: "user.create", Target: u.Name, OK: true})
			fmt.Printf("Created %s (%s).\n", u.Name, u.Role)
			return nil
		}),
	}
	create.Flags().StringVar(&role, "role", "admin", "admin | operator | restore | readonly | none (none: only per-repository roles, docs/permissions.md)")
	create.Flags().BoolVar(&stdin, "password-stdin", false, "read the password from stdin")
	root.AddCommand(create)

	pwc := &cobra.Command{
		Use: "set-password <name>", Short: "Set a user's password and end their sessions", Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, st *auth.Store, args []string) error {
			pw, err := readPassword(stdin)
			if err != nil {
				return err
			}
			if err := st.SetPassword(ctx, args[0], pw, false, ""); err != nil {
				return err
			}
			_ = st.Audit(ctx, auth.AuditEntry{Actor: "local-cli", Kind: "local", Action: "user.password", Target: args[0], OK: true})
			fmt.Println("Password changed; open sessions of that user were ended.")
			return nil
		}),
	}
	pwc.Flags().BoolVar(&stdin, "password-stdin", false, "read the password from stdin")
	root.AddCommand(pwc)

	root.AddCommand(&cobra.Command{
		Use: "disable-2fa <name>", Short: "Remove two-factor authentication from a user who lost the device", Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, st *auth.Store, args []string) error {
			if err := st.DisableTOTP(ctx, args[0]); err != nil {
				return err
			}
			_ = st.Audit(ctx, auth.AuditEntry{Actor: "local-cli", Kind: "local", Action: "user.2fa-reset", Target: args[0], OK: true})
			fmt.Println("Two-factor authentication removed.")
			return nil
		}),
	})
	root.AddCommand(&cobra.Command{
		Use: "delete <name>", Short: "Delete a user", Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, st *auth.Store, args []string) error {
			if err := st.DeleteUser(ctx, args[0]); err != nil {
				return err
			}
			_ = st.Audit(ctx, auth.AuditEntry{Actor: "local-cli", Kind: "local", Action: "user.delete", Target: args[0], OK: true})
			fmt.Println("Deleted.")
			return nil
		}),
	})
	root.AddCommand(&cobra.Command{
		Use: "setup-token", Short: "Print a one-time token for creating the first administrator in the web UI (only while no user exists)", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, st *auth.Store, _ []string) error {
			t, err := st.SetupToken(ctx)
			if err != nil {
				return err
			}
			if t == "" {
				return fmt.Errorf("users already exist: sign in, or reset a password with `user set-password`")
			}
			fmt.Println(t)
			return nil
		}),
	})
	addUserAuthCommands(root, run) // passkeys and the single-sign-on break-glass (cmds_user_auth.go)
	return root
}
