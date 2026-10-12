package engine_test

import (
	"os"

	"github.com/unidoc/incus-backup/internal/client/engine"
)

func setUploadWorkers(e *engine.Engine, n int) { e.UploadWorkers = n }

func envOr(name, def string) string {
	if v := lookupEnv(name); v != "" {
		return v
	}
	return def
}

func lookupEnv(name string) string { return os.Getenv(name) }
