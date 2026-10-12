package service

import (
	"errors"
	"fmt"

	"github.com/unidoc/incus-backup/internal/datastore"
)

// storeErr classifies an error of a datastore session write for the agent API: maintenance mode keeps
// its identity (503: the host retries later), invalid input is the client's mistake (400), and anything
// else is a server or storage fault (500, retryable), never "bad request".
func storeErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, datastore.ErrReadOnly), errors.Is(err, datastore.ErrOffline):
		return fmt.Errorf("%w (the repository is in maintenance; the host retries at its next run)", err)
	case errors.Is(err, datastore.ErrInvalid), errors.Is(err, datastore.ErrMissingChunks):
		return bad("%v", err)
	}
	return err
}
