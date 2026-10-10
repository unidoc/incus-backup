package datastore

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// A repository belongs to exactly one incus-backup server at a time. `owner.json` records
// which one, so two independent servers pointed at the same directory or bucket (a
// copy/paste of an S3 prefix, a restored VM that still runs the old server) cannot both
// adopt it and run GC or retention against each other's data.

// ErrForeignOwner is returned when a repository belongs to another server.
var ErrForeignOwner = errors.New("repository belongs to another incus-backup server")

// Owner is owner.json.
type Owner struct {
	ServerID string    `json:"server_id"`
	Host     string    `json:"host,omitempty"` // informational
	Since    time.Time `json:"since"`
	// Previous is the owner this server replaced when it was explicitly adopted.
	Previous string `json:"previous,omitempty"`
}

const ownerKey = "owner.json"

// ReadOwner returns owner.json, or nil if the repository has no owner yet.
func ReadOwner(b Backend) (*Owner, error) {
	data, err := b.Get(ownerKey)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var o Owner
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("corrupt %s: %w", ownerKey, err)
	}
	return &o, nil
}

// ClaimOwner makes serverID the owner of b. A repository with no owner is claimed with a
// create-if-absent write (two servers racing: exactly one wins). One owned by someone else
// is refused unless adopt is set, which replaces the owner and records who it was.
func ClaimOwner(b Backend, serverID, host string, adopt bool) error {
	cur, err := ReadOwner(b)
	if err != nil {
		return err
	}
	mine := Owner{ServerID: serverID, Host: host, Since: time.Now().UTC()}
	write := func(o Owner, ifAbsent bool) error {
		data, _ := json.Marshal(o)
		return b.Put(ownerKey, data, ifAbsent)
	}
	switch {
	case cur == nil:
		if err := write(mine, true); err != nil {
			if errors.Is(err, ErrExists) {
				return ClaimOwner(b, serverID, host, adopt) // lost a race: re-read who won
			}
			return err
		}
		return nil
	case cur.ServerID == serverID:
		return nil
	case adopt:
		mine.Previous = cur.ServerID
		return write(mine, false)
	default:
		return fmt.Errorf("%w (owner %s since %s, host %q); use `repo attach --adopt` only if that server is gone for good",
			ErrForeignOwner, cur.ServerID, cur.Since.Format(time.RFC3339), cur.Host)
	}
}

// OpenOwned opens the repository on b for serverID, claiming it if it is unowned.
func OpenOwned(b Backend, serverID, host string, adopt bool) (*Store, error) {
	st, err := OpenBackend(b)
	if err != nil {
		return nil, err
	}
	if err := ClaimOwner(b, serverID, host, adopt); err != nil {
		return nil, err
	}
	return st, nil
}

// InitOwned creates a repository on b owned by serverID.
func InitOwned(b Backend, serverID, host string) (*Meta, error) {
	m, err := InitBackend(b)
	if err != nil {
		return nil, err
	}
	if err := ClaimOwner(b, serverID, host, false); err != nil {
		return nil, err
	}
	return m, nil
}
