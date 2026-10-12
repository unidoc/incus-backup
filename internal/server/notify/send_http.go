package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultSignatureHeader carries the webhook signature: "sha256=" + hex(HMAC-SHA256(secret, timestamp + "." + body)),
// with the timestamp (unix seconds) in TimestampHeader.
const (
	DefaultSignatureHeader = "X-Incus-Backup-Signature"
	TimestampHeader        = "X-Incus-Backup-Timestamp"
)

func (e *Engine) httpClient(caPEM string, timeout time.Duration) (*http.Client, error) {
	var rt http.RoundTripper
	if e.HTTPTransport != nil {
		rt = e.HTTPTransport
	} else {
		t := http.DefaultTransport.(*http.Transport).Clone()
		if caPEM != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(caPEM)) {
				return nil, errors.New("tls_ca_pem holds no certificate")
			}
			t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		}
		rt = t
	}
	return &http.Client{Transport: rt, Timeout: timeout,
		// never follow a redirect: secret headers would travel to wherever it points
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

// httpErr rebuilds a client error without the URL (paths and queries of webhook URLs carry tokens).
func httpErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		inner := ue.Err
		if errors.Is(inner, context.DeadlineExceeded) || ue.Timeout() {
			return fmt.Errorf("%s %s: timed out", ue.Op, redactURL(ue.URL))
		}
		return fmt.Errorf("%s %s: %v", ue.Op, redactURL(ue.URL), inner)
	}
	return err
}

func (e *Engine) post(ctx context.Context, client *http.Client, u string, body []byte, hdr http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid request URL")
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "incus-backup-server")
	resp, err := client.Do(req)
	if err != nil {
		return httpErr(err)
	}
	defer resp.Body.Close()
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := fmt.Sprintf("HTTP %s", resp.Status)
		if t := clean(string(excerpt), 200); t != "" {
			msg += ": " + t
		}
		return errors.New(msg)
	}
	return nil
}

func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (e *Engine) sendWebhook(ctx context.Context, cfgRaw json.RawMessage, sec *ChannelSecret, m Message, timeout time.Duration, now time.Time) error {
	var c WebhookConfig
	if err := json.Unmarshal(cfgRaw, &c); err != nil {
		return errors.New("webhook: bad config")
	}
	if sec == nil || sec.URL == "" {
		return errors.New("webhook: no URL stored")
	}
	var body []byte
	switch c.Format {
	case "slack":
		body, _ = json.Marshal(map[string]any{"text": slackEscape(m.Subject + "\n" + m.Text)})
	case "discord":
		body, _ = json.Marshal(map[string]any{"content": truncateRunes(m.Subject+"\n"+m.Text, 2000),
			"allowed_mentions": map[string]any{"parse": []string{}}})
	default:
		p := map[string]any{"version": 1, "kind": m.Kind, "severity": m.Severity, "subject": m.Subject, "text": m.Text, "sent_at": now.UTC()}
		if m.Link != "" {
			p["link"] = m.Link
		}
		for k, v := range m.Payload {
			p[k] = v
		}
		body, _ = json.Marshal(p)
	}
	hdr := http.Header{}
	for k, v := range c.Headers {
		hdr.Set(k, v)
	}
	for k, v := range sec.Headers {
		hdr.Set(k, v)
	}
	if sec.HMACSecret != "" {
		ts := strconv.FormatInt(now.Unix(), 10)
		mac := hmac.New(sha256.New, []byte(sec.HMACSecret))
		mac.Write([]byte(ts + "."))
		mac.Write(body)
		h := c.SignatureHeader
		if h == "" {
			h = DefaultSignatureHeader
		}
		hdr.Set(h, "sha256="+hex.EncodeToString(mac.Sum(nil)))
		hdr.Set(TimestampHeader, ts)
	}
	client, err := e.httpClient(c.TLSCAPEM, timeout)
	if err != nil {
		return err
	}
	return e.post(ctx, client, sec.URL, body, hdr)
}

func priority(m Message, conf map[string]int, def map[string]int) int {
	k := m.Severity
	if m.Kind == "resolved" {
		k = "resolved"
	}
	if p, ok := conf[k]; ok {
		return p
	}
	return def[k]
}

var (
	ntfyDefaults   = map[string]int{SeverityInfo: 3, SeverityWarning: 4, SeverityCritical: 5, "resolved": 2}
	gotifyDefaults = map[string]int{SeverityInfo: 3, SeverityWarning: 6, SeverityCritical: 9, "resolved": 2}
)

func (e *Engine) sendNtfy(ctx context.Context, cfgRaw json.RawMessage, sec *ChannelSecret, m Message, timeout time.Duration) error {
	var c NtfyConfig
	if err := json.Unmarshal(cfgRaw, &c); err != nil {
		return errors.New("ntfy: bad config")
	}
	tag := m.Severity
	switch {
	case m.Kind == "resolved":
		tag = "white_check_mark"
	case m.Severity == SeverityCritical:
		tag = "rotating_light"
	case m.Severity == SeverityWarning:
		tag = "warning"
	}
	p := map[string]any{"topic": c.Topic, "title": m.Subject, "message": m.Text, "priority": priority(m, c.Priorities, ntfyDefaults), "tags": []string{tag}}
	if m.Link != "" {
		p["click"] = m.Link
	}
	body, _ := json.Marshal(p)
	hdr := http.Header{}
	switch {
	case c.Username != "" && sec != nil:
		req := http.Request{Header: http.Header{}}
		req.SetBasicAuth(c.Username, sec.Password)
		hdr.Set("Authorization", req.Header.Get("Authorization"))
	case sec != nil && sec.Token != "":
		hdr.Set("Authorization", "Bearer "+sec.Token)
	}
	client, err := e.httpClient(c.TLSCAPEM, timeout)
	if err != nil {
		return err
	}
	// JSON publishing (POST to the server root) carries non-ASCII titles, unlike the Title header
	return e.post(ctx, client, strings.TrimRight(c.Server, "/")+"/", body, hdr)
}

func (e *Engine) sendGotify(ctx context.Context, cfgRaw json.RawMessage, sec *ChannelSecret, m Message, timeout time.Duration) error {
	var c GotifyConfig
	if err := json.Unmarshal(cfgRaw, &c); err != nil {
		return errors.New("gotify: bad config")
	}
	if sec == nil || sec.Token == "" {
		return errors.New("gotify: no token stored")
	}
	extras := map[string]any{"client::display": map[string]any{"contentType": "text/plain"}}
	if m.Link != "" {
		extras["client::notification"] = map[string]any{"click": map[string]any{"url": m.Link}}
	}
	body, _ := json.Marshal(map[string]any{"title": m.Subject, "message": m.Text, "priority": priority(m, c.Priorities, gotifyDefaults), "extras": extras})
	hdr := http.Header{}
	hdr.Set("X-Gotify-Key", sec.Token) // a header, never ?token= (URLs end up in logs)
	client, err := e.httpClient(c.TLSCAPEM, timeout)
	if err != nil {
		return err
	}
	return e.post(ctx, client, strings.TrimRight(c.URL, "/")+"/message", body, hdr)
}
