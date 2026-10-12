package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/jobspec"
)

// NextJob long-polls for the next job of this host (nil when none arrived within wait). types
// restricts what the host can take right now (empty = any).
func (c *Client) NextJob(ctx context.Context, wait time.Duration, types []string) (*jobspec.Job, error) {
	q := url.Values{}
	q.Set("wait", strconv.Itoa(int(wait/time.Second)))
	if len(types) > 0 {
		q.Set("types", strings.Join(types, ","))
	}
	var raw []byte
	if err := c.doRaw(ctx, "GET", "/v1/jobs/next?"+q.Encode(), nil, "", nil, nil, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 { // 204
		return nil, nil
	}
	var j jobspec.Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// JobProgress reports progress (and renews the job's lease); the answer says whether to stop.
func (c *Client) JobProgress(ctx context.Context, id string, p jobspec.Progress) (*jobspec.ProgressResponse, error) {
	var out jobspec.ProgressResponse
	err := retry(ctx, func() error { return c.do(ctx, "POST", "/v1/jobs/"+url.PathEscape(id)+"/progress", p, &out, nil) })
	return &out, err
}

// FinishJob ends a job.
func (c *Client) FinishJob(ctx context.Context, id string, f jobspec.Finish) error {
	return retry(ctx, func() error { return c.do(ctx, "POST", "/v1/jobs/"+url.PathEscape(id)+"/finish", f, nil, nil) })
}

// JobCancelRequested asks whether a job should stop.
func (c *Client) JobCancelRequested(ctx context.Context, id string) (*jobspec.CancelState, error) {
	var out jobspec.CancelState
	err := retry(ctx, func() error {
		return c.do(ctx, "GET", "/v1/jobs/"+url.PathEscape(id)+"/cancel-requested", nil, &out, nil)
	})
	return &out, err
}

// DeliveryReceipt is the server's view of an uploaded delivery.
type DeliveryReceipt struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// PutDelivery streams an already sealed file to the server. The body is read once, so it is not
// retried; a body that ends with an error (CloseWithError on a pipe) aborts the request and the
// server never records a delivery. Only ctx bounds the upload (no per-request timeout: a 4 GiB file
// over a slow link legitimately takes long).
func (c *Client) PutDelivery(ctx context.Context, id string, attempt int, body io.Reader) (*DeliveryReceipt, error) {
	req, err := http.NewRequestWithContext(ctx, "PUT", c.Base+"/v1/jobs/"+url.PathEscape(id)+"/delivery", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(jobspec.HeaderAttempt, strconv.Itoa(attempt))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e api.Error
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, &APIError{Status: resp.StatusCode, Msg: msg}
	}
	var out DeliveryReceipt
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("delivery receipt: %w", err)
	}
	return &out, nil
}

// ReportRestoreTest records a restore test the host ran on its own schedule.
func (c *Client) ReportRestoreTest(ctx context.Context, rep jobspec.RestoreTestReport) error {
	return retry(ctx, func() error { return c.do(ctx, "POST", "/v1/restore-tests", rep, nil, nil) })
}
