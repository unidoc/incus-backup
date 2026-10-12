package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/unidoc/incus-backup/internal/server/mgmt"
)

// cmdOpenAPI prints the OpenAPI 3.1 document of the management API (the same document the server
// serves at GET /api/v1/openapi.json). It needs no configuration or database.
func cmdOpenAPI() *cobra.Command {
	return &cobra.Command{
		Use:   "openapi",
		Short: "Print the OpenAPI 3.1 document of the management API (/api/v1)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := os.Stdout.Write(mgmt.OpenAPI(version))
			return err
		},
	}
}
