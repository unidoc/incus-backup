package notify

import (
	"context"
	"fmt"
	"time"
)

// ---- update_available -----------------------------------------------------------------------

// evalUpdate fires (info by default) while the opt-in update check reports a newer release. With checks
// off it reports nothing, so an open alert resolves.
func (e *Engine) evalUpdate(ctx context.Context, c condCfg, now time.Time) ([]observation, error) {
	latest, current, url, ok := e.UpdateStatus(ctx)
	if !ok {
		return nil, nil
	}
	o := observation{Key: "update_available:" + latest, Title: fmt.Sprintf("incus-backup %s is available (this server runs %s)", latest, current),
		Detail: fmt.Sprintf("The release check found version %s; this server runs %s.", latest, current)}
	if url != "" {
		o.Detail += "\nRelease: " + url
	}
	return []observation{o}, nil
}
