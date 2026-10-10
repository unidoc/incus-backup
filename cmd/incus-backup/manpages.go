package main

import (
	"fmt"
	"os"
	"regexp"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// cmdMan writes one man page per command (section 1). Hidden: it exists for
// packaging (the Alpine -doc subpackage) and honours SOURCE_DATE_EPOCH so the
// pages are reproducible.
func cmdMan(root *cobra.Command) *cobra.Command {
	return &cobra.Command{Use: "man DIR", Short: "Write the man pages into DIR (packaging)", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := os.MkdirAll(args[0], 0o755); err != nil {
				return err
			}
			root.DisableAutoGenTag = true
			escapeForMan(root)
			h := &doc.GenManHeader{Title: "INCUS-BACKUP", Section: "1", Source: "incus-backup " + version, Manual: "incus-backup manual"}
			if err := doc.GenManTree(root, h, args[0]); err != nil {
				return fmt.Errorf("writing man pages: %w", err)
			}
			return nil
		}}
}

// manPlaceholder matches "<host>"-style placeholders, which the markdown step of
// the man generator would otherwise swallow as HTML tags.
var manPlaceholder = regexp.MustCompile(`<([A-Za-z][A-Za-z0-9_-]*)>`)

func escapeForMan(c *cobra.Command) {
	for _, s := range []*string{&c.Long, &c.Short, &c.Example} {
		*s = manPlaceholder.ReplaceAllString(*s, `\<$1\>`)
	}
	for _, s := range c.Commands() {
		escapeForMan(s)
	}
}
