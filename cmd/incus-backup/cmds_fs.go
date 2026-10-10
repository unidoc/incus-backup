package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/fscat"
)

// openFS opens a catalog snapshot of an object for browsing; no Incus is needed.
func (c *cli) openFS(ctx context.Context, server, project, object, snap string) (*engine.FSView, *engine.Located, error) {
	a, err := c.load(ctx, false)
	if err != nil {
		return nil, nil, err
	}
	t, err := a.Eng.PickTarget(server)
	if err != nil {
		return nil, nil, err
	}
	loc, err := a.Eng.Locate(ctx, t, project, object, snap)
	if err != nil {
		return nil, nil, err
	}
	v, err := a.Eng.OpenFS(ctx, loc)
	return v, loc, err
}

func (c *cli) cmdFS() *cobra.Command {
	root := &cobra.Command{Use: "fs", Short: "Browse a filesystem backup and restore single files (catalog snapshots; no Incus needed)",
		Long: `The file catalog of a snapshot is encrypted like the data: the server never sees a file name. Browsing
downloads only the catalog; extracting a file downloads only the chunks that hold its bytes, and every
extracted file is checked against its recorded SHA-256.`}
	var server, project, snap string
	common := func(f *pflag.FlagSet) {
		f.StringVar(&server, "server", "", "server to read from")
		f.StringVar(&project, "project", "default", "Incus project of the object")
		f.StringVar(&snap, "snapshot", "", "snapshot id (default: latest)")
	}
	var long bool
	ls := &cobra.Command{Use: "ls OBJECT [PATH]", Short: "List a directory of a backup (OBJECT as in `list`, e.g. instance/web/root or volume/docs)", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			v, loc, err := c.openFS(ctx, server, project, args[0], snap)
			if err != nil {
				return err
			}
			p := ""
			if len(args) == 2 {
				p = args[1]
			}
			ents, err := v.Tree.List(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "%s@%s %s\n", args[0], loc.Snapshot, "/"+p)
			for _, e := range ents {
				if long {
					fmt.Println(e.Line(e.Name))
				} else if e.Kind == fscat.KDir {
					fmt.Println(e.Name + "/")
				} else {
					fmt.Println(e.Name)
				}
			}
			return nil
		}}
	ls.Flags().BoolVarP(&long, "long", "l", false, "long listing")
	common(ls.Flags())
	find := &cobra.Command{Use: "find OBJECT PATTERN", Short: "Find paths whose name matches a glob (e.g. '*.conf')", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			v, _, err := c.openFS(ctx, server, project, args[0], snap)
			if err != nil {
				return err
			}
			var below string
			if cmd.Flags().Changed("below") {
				below, _ = cmd.Flags().GetString("below")
			}
			found, err := v.Tree.Find(below, args[1])
			if err != nil {
				return err
			}
			for _, f := range found {
				fmt.Println("/" + f)
			}
			return nil
		}}
	find.Flags().String("below", "", "only below this directory")
	common(find.Flags())
	stat := &cobra.Command{Use: "stat OBJECT PATH", Short: "Show the recorded metadata of one entry", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			v, _, err := c.openFS(ctx, server, project, args[0], snap)
			if err != nil {
				return err
			}
			e, err := v.Tree.Lookup(args[1])
			if err != nil {
				return err
			}
			fmt.Println(e.Line("/" + args[1]))
			fmt.Printf("inode %d  ctime %d  mtime %d (ns)\n", e.Inode, e.CtimeNs, e.MtimeNs)
			for _, x := range e.Xattrs {
				fmt.Printf("xattr %s (%d bytes)\n", x.Name, len(x.Value))
			}
			if e.Size > 0 {
				fmt.Printf("sha256 %x\n", e.Sum)
			}
			return nil
		}}
	common(stat.Flags())
	cat := &cobra.Command{Use: "cat OBJECT PATH", Short: "Write one file of a backup to standard output (verified)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			v, _, err := c.openFS(ctx, server, project, args[0], snap)
			if err != nil {
				return err
			}
			return v.Cat(io.Writer(os.Stdout), args[1])
		}}
	common(cat.Flags())
	var to string
	var noChown bool
	extract := &cobra.Command{Use: "extract OBJECT PATH...", Short: "Restore single files or directories into a local directory", Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			if to == "" {
				return fmt.Errorf("--to DIRECTORY is required")
			}
			if err := os.MkdirAll(to, 0o755); err != nil {
				return err
			}
			v, loc, err := c.openFS(ctx, server, project, args[0], snap)
			if err != nil {
				return err
			}
			if err := v.Extract(to, args[1:], noChown || os.Getuid() != 0); err != nil {
				return err
			}
			fmt.Printf("Extracted %d path(s) of %s@%s into %s\n", len(args)-1, args[0], loc.Snapshot, to)
			return nil
		}}
	extract.Flags().StringVar(&to, "to", "", "destination directory (created if needed; existing files are never overwritten)")
	extract.Flags().BoolVar(&noChown, "no-chown", false, "do not change ownership (automatic when not root)")
	common(extract.Flags())
	root.AddCommand(ls, find, stat, cat, extract)
	return root
}
