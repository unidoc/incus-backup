// Package updatecheck tells an administrator that a newer release exists. It is OFF by default: the
// server never contacts anything unless the setting update_check is "on". When on, it asks once a day
// (and on demand) for the latest version:
//
//   - a GitHub "releases/latest" URL (the default, https://github.com/unidoc/incus-backup/releases/latest)
//     is asked with HEAD and NOT followed: its redirect Location ends in /tag/vX.Y.Z;
//   - any other URL may answer JSON {"version": "X.Y.Z"} (or {"tag_name": "vX.Y.Z"}) to a GET.
//
// Nothing about this server is sent: no version, no id (only a fixed User-Agent).
package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultURL is the release feed asked when update_check_url is empty.
const DefaultURL = "https://github.com/unidoc/incus-backup/releases/latest"

// Status is what the UI and the alert condition read.
type Status struct {
	Enabled         bool       `json:"enabled"`
	URL             string     `json:"url"`
	Current         string     `json:"current"`
	Latest          string     `json:"latest,omitempty"`
	UpdateAvailable bool       `json:"update_available"`
	CheckedAt       *time.Time `json:"checked_at,omitempty"`
	Error           string     `json:"error,omitempty"`
	ReleaseURL      string     `json:"release_url,omitempty"`
}

// Checker caches the last answer.
type Checker struct {
	Current string
	// Settings returns (enabled, url) from the server settings at each check.
	Settings func(ctx context.Context) (bool, string)
	HTTP     *http.Client
	Now      func() time.Time

	mu   sync.Mutex
	last Status
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Status returns the cached state (enabled and url as currently configured).
func (c *Checker) Status(ctx context.Context) Status {
	en, u := c.Settings(ctx)
	if u == "" {
		u = DefaultURL
	}
	c.mu.Lock()
	st := c.last
	c.mu.Unlock()
	st.Enabled, st.URL, st.Current = en, u, c.Current
	if !en {
		st.UpdateAvailable = false
	}
	return st
}

// Check asks the feed now (refused when disabled).
func (c *Checker) Check(ctx context.Context) (Status, error) {
	en, u := c.Settings(ctx)
	if u == "" {
		u = DefaultURL
	}
	if !en {
		return c.Status(ctx), errors.New("update checks are off (setting update_check)")
	}
	latest, rel, err := c.fetch(ctx, u)
	now := c.now().UTC()
	c.mu.Lock()
	c.last.CheckedAt = &now
	if err != nil {
		c.last.Error = err.Error()
	} else {
		c.last.Error = ""
		c.last.Latest = latest
		c.last.ReleaseURL = rel
		c.last.UpdateAvailable = Newer(latest, c.Current)
	}
	c.mu.Unlock()
	return c.Status(ctx), err
}

// Loop checks once a day while enabled (first check a minute after start).
func (c *Checker) Loop(ctx context.Context) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if en, _ := c.Settings(ctx); en {
			_, _ = c.Check(ctx)
		}
		t.Reset(24 * time.Hour)
	}
}

func (c *Checker) fetch(ctx context.Context, u string) (latest, release string, err error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	cl := *hc
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if strings.Contains(u, "/releases/latest") {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		if err != nil {
			return "", "", err
		}
		req.Header.Set("User-Agent", "incus-backup-server update check")
		resp, err := cl.Do(req)
		if err != nil {
			return "", "", fmt.Errorf("asking %s: %w", u, err)
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode/100 != 3 || loc == "" {
			return "", "", fmt.Errorf("%s answered %d without a redirect to a release", u, resp.StatusCode)
		}
		i := strings.LastIndex(loc, "/tag/")
		if i < 0 {
			return "", "", fmt.Errorf("unexpected release redirect %q", loc)
		}
		v := strings.TrimPrefix(loc[i+5:], "v")
		if !validVersion(v) {
			return "", "", fmt.Errorf("unexpected release tag %q", loc[i+5:])
		}
		return v, loc, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "incus-backup-server update check")
	req.Header.Set("Accept", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("asking %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("%s answered %d", u, resp.StatusCode)
	}
	var body struct {
		Version string `json:"version"`
		TagName string `json:"tag_name"`
		URL     string `json:"url"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", "", fmt.Errorf("%s: not a version document: %w", u, err)
	}
	v := strings.TrimPrefix(firstNonEmpty(body.Version, body.TagName), "v")
	if !validVersion(v) {
		return "", "", fmt.Errorf("%s: no usable version in the answer", u)
	}
	return v, firstNonEmpty(body.URL, body.HTMLURL), nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func validVersion(v string) bool {
	_, ok := parse(v)
	return ok
}

// parse reads "1.2.3" (optionally with a "-suffix", which sorts before the plain release).
func parse(v string) ([4]int, bool) {
	var out [4]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	main, pre, hasPre := strings.Cut(v, "-")
	ps := strings.Split(main, ".")
	if len(ps) < 1 || len(ps) > 3 || len(v) > 64 {
		return out, false
	}
	for i, p := range ps {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	out[3] = 1
	if hasPre && pre != "" {
		out[3] = 0
	}
	return out, true
}

// Newer reports whether latest is a newer version than current (unparsable versions never are).
func Newer(latest, current string) bool {
	l, ok1 := parse(latest)
	c, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}
