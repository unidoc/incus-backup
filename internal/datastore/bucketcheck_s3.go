package datastore

import (
	"context"
	"fmt"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

// BucketFinding is an observation about the bucket's own configuration (versioning, lifecycle
// rules, object lock). None of them makes the bucket unusable, so they are never a probe failure;
// some of them can silently cost money or destroy backups, which an operator must know before
// trusting the repository.
type BucketFinding struct {
	// ID is stable: "versioning", "lifecycle", "object-lock".
	ID string `json:"id"`
	// Level: "pass" (fine), "warn" (read the detail), "unknown" (the setting could not be read).
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

// BucketConfig reads the bucket settings that interact with how the repository deletes objects
// (GC, retention, forget). Reading them needs GetBucketVersioning / GetLifecycleConfiguration /
// GetBucketObjectLockConfiguration; a credential without those permissions gets "unknown", not an
// error. Nothing is written.
func (b *S3Backend) BucketConfig(ctx context.Context) []BucketFinding {
	c := b.cl()
	out := []BucketFinding{}

	// versioning
	vc, verr := c.GetBucketVersioning(ctx, b.bucket)
	versioned := false
	switch {
	case verr != nil:
		out = append(out, unknownFinding("versioning", "bucket versioning", verr))
	case vc.Status == minio.Enabled:
		versioned = true
		out = append(out, BucketFinding{ID: "versioning", Level: "warn", Detail: "Versioning is enabled. Chunks that GC or retention delete stay in the bucket as non-current versions: " +
			"they are still stored (and billed), so GC frees no space. That is useful protection against deletion, but add a lifecycle rule that expires " +
			"non-current versions after a number of days if you want the space back."})
	case vc.Status == minio.Suspended:
		out = append(out, BucketFinding{ID: "versioning", Level: "warn", Detail: "Versioning is suspended. Versions written while it was enabled stay stored until they are deleted or expire."})
	default:
		out = append(out, BucketFinding{ID: "versioning", Level: "pass", Detail: "Versioning is off: space freed by GC and retention is released immediately."})
	}

	// lifecycle rules that touch the repository's objects
	lc, lerr := c.GetBucketLifecycle(ctx, b.bucket)
	switch {
	case lerr != nil && isNoConfig(lerr, "NoSuchLifecycleConfiguration"):
		out = append(out, BucketFinding{ID: "lifecycle", Level: "pass", Detail: "No lifecycle rules: nothing expires or moves repository objects behind the server's back."})
	case lerr != nil:
		out = append(out, unknownFinding("lifecycle", "lifecycle rules", lerr))
	default:
		out = append(out, lifecycleFinding(lc, b.prefix, versioned))
	}

	// object lock
	enabled, mode, validity, unit, oerr := c.GetObjectLockConfig(ctx, b.bucket)
	switch {
	case oerr != nil && isNoConfig(oerr, "ObjectLockConfigurationNotFoundError"):
		out = append(out, BucketFinding{ID: "object-lock", Level: "pass", Detail: "Object lock is not configured."})
	case oerr != nil:
		out = append(out, unknownFinding("object-lock", "object lock", oerr))
	case enabled == "Enabled" && mode != nil && validity != nil:
		u := "days"
		if unit != nil && *unit == minio.Years {
			u = "years"
		}
		out = append(out, BucketFinding{ID: "object-lock", Level: "warn", Detail: fmt.Sprintf("Object lock is enabled with a default %s retention of %d %s. "+
			"Every chunk and snapshot is locked for that long: GC, retention and forget cannot free its space before the lock ends "+
			"(a delete only adds a delete marker). Size the bucket for it, and keep the lock shorter than your shortest retention.", strings.ToLower(string(*mode)), *validity, u)})
	case enabled == "Enabled":
		out = append(out, BucketFinding{ID: "object-lock", Level: "pass", Detail: "Object lock is enabled without a default retention: objects are not locked unless something sets a retention on them."})
	default:
		out = append(out, BucketFinding{ID: "object-lock", Level: "pass", Detail: "Object lock is not enabled."})
	}
	return out
}

func isNoConfig(err error, code string) bool {
	return minio.ToErrorResponse(err).Code == code
}

func unknownFinding(id, what string, err error) BucketFinding {
	code := minio.ToErrorResponse(err).Code
	if code == "" {
		code = err.Error()
	}
	return BucketFinding{ID: id, Level: "unknown", Detail: fmt.Sprintf("Could not read the %s (%s). The credential may lack the permission to read it, or the service does not support it; check the setting in the provider's console.", what, code)}
}

// ruleApplies reports whether an enabled lifecycle rule can match objects under the repository prefix
// (prefix "" or ending in "/"). Rules filtered by tag never match: the repository writes no tags.
func ruleApplies(r lifecycle.Rule, prefix string) bool {
	if !strings.EqualFold(r.Status, "Enabled") {
		return false
	}
	if r.RuleFilter.Tag.Key != "" || len(r.RuleFilter.And.Tags) > 0 {
		return false
	}
	p := r.Prefix
	if r.RuleFilter.Prefix != "" {
		p = r.RuleFilter.Prefix
	}
	if r.RuleFilter.And.Prefix != "" {
		p = r.RuleFilter.And.Prefix
	}
	return p == "" || strings.HasPrefix(prefix, p) || strings.HasPrefix(p, prefix)
}

func lifecycleFinding(lc *lifecycle.Configuration, prefix string, versioned bool) BucketFinding {
	var expire, transition, noncurrent []string
	for _, r := range lc.Rules {
		if !ruleApplies(r, prefix) {
			continue
		}
		name := r.ID
		if name == "" {
			name = "(unnamed)"
		}
		// an expiry of current objects (days or date) or of all versions destroys data; expiring only
		// delete markers does not
		if !r.Expiration.IsDaysNull() || !r.Expiration.IsDateNull() || r.Expiration.DeleteAll.IsEnabled() || !r.AllVersionsExpiration.IsNull() {
			expire = append(expire, name)
		}
		if !r.Transition.IsNull() {
			transition = append(transition, name)
		}
		if !r.NoncurrentVersionExpiration.IsDaysNull() {
			noncurrent = append(noncurrent, name)
		}
	}
	switch {
	case len(expire) > 0:
		return BucketFinding{ID: "lifecycle", Level: "warn", Detail: fmt.Sprintf("Lifecycle rule %s expires objects under the repository prefix. "+
			"It would delete chunks that committed snapshots still need, and those backups would fail verification and restore. "+
			"Remove the rule or limit it to another prefix; retention and GC already delete what is no longer needed.", strings.Join(expire, ", "))}
	case len(transition) > 0:
		return BucketFinding{ID: "lifecycle", Level: "warn", Detail: fmt.Sprintf("Lifecycle rule %s moves repository objects to another storage class. "+
			"Objects in an archive class (for example Glacier) cannot be read until they are restored: verification, restore and sync would fail.", strings.Join(transition, ", "))}
	case len(noncurrent) > 0 && versioned:
		return BucketFinding{ID: "lifecycle", Level: "pass", Detail: fmt.Sprintf("Lifecycle rule %s expires non-current versions: space freed by GC comes back after that delay.", strings.Join(noncurrent, ", "))}
	default:
		return BucketFinding{ID: "lifecycle", Level: "pass", Detail: "No lifecycle rule expires or moves objects under the repository prefix."}
	}
}
