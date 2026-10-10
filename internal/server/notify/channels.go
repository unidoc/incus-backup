package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unidoc/incus-backup/internal/server/service"
)

// Channel kinds.
const (
	KindSMTP    = "smtp"
	KindWebhook = "webhook"
	KindNtfy    = "ntfy"
	KindGotify  = "gotify"
)

// Channel is what the API shows about a channel: never a secret.
type Channel struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	Config         json.RawMessage `json:"config"` // non-secret settings (one of the *Config types)
	HasSecret      bool            `json:"has_secret"`
	Target         string          `json:"target"` // where messages go, without credentials (e.g. "smtp://mail.example.com:587", "https://hooks.slack.com/…")
	Enabled        bool            `json:"enabled"`
	RatePerHour    int             `json:"rate_per_hour"`
	TimeoutSeconds int             `json:"timeout_seconds"`
	Created        time.Time       `json:"created"`
}

// ChannelSecret is the sealed part of a channel. It is written, never read back through the API.
type ChannelSecret struct {
	Password   string            `json:"password,omitempty"`    // smtp; ntfy basic auth
	Token      string            `json:"token,omitempty"`       // ntfy access token; gotify application token
	URL        string            `json:"url,omitempty"`         // webhook: the URL (Slack/Discord URLs carry their token)
	HMACSecret string            `json:"hmac_secret,omitempty"` // webhook: signing secret
	Headers    map[string]string `json:"headers,omitempty"`     // webhook: secret headers (Authorization ...)
}

func (s *ChannelSecret) empty() bool {
	return s == nil || (s.Password == "" && s.Token == "" && s.URL == "" && s.HMACSecret == "" && len(s.Headers) == 0)
}

// values lists every secret string (for redaction of error texts).
func (s *ChannelSecret) values() []string {
	if s == nil {
		return nil
	}
	v := []string{s.Password, s.Token, s.URL, s.HMACSecret}
	for _, h := range s.Headers {
		v = append(v, h)
	}
	if u, err := url.Parse(s.URL); err == nil && s.URL != "" {
		v = append(v, u.Path, u.RawQuery)
	}
	return v
}

// ChannelInput creates or updates a channel. On update, nil fields keep their value; Secret nil keeps
// the stored secret, a non-nil empty Secret removes it.
type ChannelInput struct {
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	Config         json.RawMessage `json:"config"`
	Secret         *ChannelSecret  `json:"secret,omitempty"`
	Enabled        *bool           `json:"enabled,omitempty"`
	RatePerHour    *int            `json:"rate_per_hour,omitempty"`
	TimeoutSeconds *int            `json:"timeout_seconds,omitempty"`
}

// SMTPConfig is the non-secret configuration of an e-mail channel.
type SMTPConfig struct {
	Host          string   `json:"host"`
	Port          int      `json:"port"`
	TLS           string   `json:"tls"`  // starttls (default), tls (implicit, port 465), none
	Auth          string   `json:"auth"` // "" (pick from the server's list), plain, login; used when username is set
	Username      string   `json:"username"`
	From          string   `json:"from"`
	To            []string `json:"to"`
	HELO          string   `json:"helo,omitempty"`
	TLSCAPEM      string   `json:"tls_ca_pem,omitempty"`      // trust this CA instead of the system roots
	TLSServerName string   `json:"tls_server_name,omitempty"` // verify this name instead of Host
}

// WebhookConfig is the non-secret configuration of a webhook channel (the URL itself is secret).
type WebhookConfig struct {
	Format          string            `json:"format"`                     // json (default), slack, discord
	Headers         map[string]string `json:"headers,omitempty"`          // non-secret extra headers
	SignatureHeader string            `json:"signature_header,omitempty"` // default X-Incus-Backup-Signature
	TLSCAPEM        string            `json:"tls_ca_pem,omitempty"`
}

// NtfyConfig is the non-secret configuration of an ntfy channel.
type NtfyConfig struct {
	Server     string         `json:"server"` // default https://ntfy.sh
	Topic      string         `json:"topic"`
	Username   string         `json:"username,omitempty"` // basic auth with secret.password; else secret.token as bearer
	Priorities map[string]int `json:"priorities,omitempty"`
	TLSCAPEM   string         `json:"tls_ca_pem,omitempty"`
}

// GotifyConfig is the non-secret configuration of a Gotify channel (the application token is secret).
type GotifyConfig struct {
	URL        string         `json:"url"`
	Priorities map[string]int `json:"priorities,omitempty"`
	TLSCAPEM   string         `json:"tls_ca_pem,omitempty"`
}

func decodeStrict(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields() // a password typed into config by mistake must not be stored in clear
	if err := d.Decode(v); err != nil {
		return bad("config: %v", err)
	}
	return nil
}

func validHTTPURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an http(s) URL")
	}
	if u.User != nil {
		return errors.New("must not contain credentials (use the secret)")
	}
	return nil
}

func validHeaderMap(h map[string]string) error {
	for k, v := range h {
		if k == "" || strings.ContainsAny(k, " \r\n:") || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("invalid header %q", k)
		}
	}
	return nil
}

// validate checks a channel's config together with its secret and returns the normalised config.
func validateChannel(kind string, raw json.RawMessage, sec *ChannelSecret) (json.RawMessage, error) {
	var v any
	switch kind {
	case KindSMTP:
		var c SMTPConfig
		if err := decodeStrict(raw, &c); err != nil {
			return nil, err
		}
		if c.TLS == "" {
			c.TLS = "starttls"
		}
		if c.Port == 0 {
			c.Port = map[string]int{"starttls": 587, "tls": 465, "none": 25}[c.TLS]
		}
		switch {
		case c.Host == "" || strings.ContainsAny(c.Host, " /\r\n"):
			return nil, bad("smtp: host required")
		case c.Port < 1 || c.Port > 65535:
			return nil, bad("smtp: invalid port")
		case c.TLS != "starttls" && c.TLS != "tls" && c.TLS != "none":
			return nil, bad("smtp: tls must be starttls, tls or none")
		case c.Auth != "" && c.Auth != "plain" && c.Auth != "login":
			return nil, bad("smtp: auth must be plain or login")
		case c.Username != "" && c.TLS == "none":
			return nil, bad("smtp: refusing to send a password over an unencrypted connection (use starttls or tls)")
		case c.Username != "" && (sec == nil || sec.Password == ""):
			return nil, bad("smtp: a username needs secret.password")
		case len(c.To) == 0:
			return nil, bad("smtp: at least one recipient (to) required")
		}
		if _, err := mail.ParseAddress(c.From); err != nil || strings.ContainsAny(c.From, "\r\n") {
			return nil, bad("smtp: invalid from address")
		}
		for _, t := range c.To {
			if _, err := mail.ParseAddress(t); err != nil || strings.ContainsAny(t, "\r\n") {
				return nil, bad("smtp: invalid recipient %q", t)
			}
		}
		if strings.ContainsAny(c.HELO, " \r\n") {
			return nil, bad("smtp: invalid helo name")
		}
		v = c
	case KindWebhook:
		var c WebhookConfig
		if err := decodeStrict(raw, &c); err != nil {
			return nil, err
		}
		if c.Format == "" {
			c.Format = "json"
		}
		if c.Format != "json" && c.Format != "slack" && c.Format != "discord" {
			return nil, bad("webhook: format must be json, slack or discord")
		}
		if sec == nil || sec.URL == "" {
			return nil, bad("webhook: secret.url required")
		}
		if err := validHTTPURL(sec.URL); err != nil {
			return nil, bad("webhook: url %v", err)
		}
		if err := validHeaderMap(c.Headers); err != nil {
			return nil, bad("webhook: %v", err)
		}
		if err := validHeaderMap(sec.Headers); err != nil {
			return nil, bad("webhook: secret %v", err)
		}
		if c.SignatureHeader != "" && strings.ContainsAny(c.SignatureHeader, " \r\n:") {
			return nil, bad("webhook: invalid signature header")
		}
		v = c
	case KindNtfy:
		var c NtfyConfig
		if err := decodeStrict(raw, &c); err != nil {
			return nil, err
		}
		if c.Server == "" {
			c.Server = "https://ntfy.sh"
		}
		if err := validHTTPURL(c.Server); err != nil {
			return nil, bad("ntfy: server %v", err)
		}
		if c.Topic == "" || strings.ContainsAny(c.Topic, "/ \r\n?#") {
			return nil, bad("ntfy: invalid topic")
		}
		if c.Username != "" && (sec == nil || sec.Password == "") {
			return nil, bad("ntfy: a username needs secret.password")
		}
		if err := validPriorities(c.Priorities, 1, 5); err != nil {
			return nil, bad("ntfy: %v", err)
		}
		v = c
	case KindGotify:
		var c GotifyConfig
		if err := decodeStrict(raw, &c); err != nil {
			return nil, err
		}
		if err := validHTTPURL(c.URL); err != nil {
			return nil, bad("gotify: url %v", err)
		}
		if sec == nil || sec.Token == "" {
			return nil, bad("gotify: secret.token required")
		}
		if err := validPriorities(c.Priorities, 0, 10); err != nil {
			return nil, bad("gotify: %v", err)
		}
		v = c
	default:
		return nil, bad("unknown channel kind %q", kind)
	}
	out, _ := json.Marshal(v)
	return out, nil
}

func validPriorities(m map[string]int, lo, hi int) error {
	for k, p := range m {
		switch k {
		case SeverityInfo, SeverityWarning, SeverityCritical, "resolved":
		default:
			return fmt.Errorf("priorities: unknown key %q (info, warning, critical, resolved)", k)
		}
		if p < lo || p > hi {
			return fmt.Errorf("priorities: %s must be %d..%d", k, lo, hi)
		}
	}
	return nil
}

func secretKind(kind string) string {
	switch kind {
	case KindSMTP:
		return service.SecretSMTP
	case KindWebhook:
		return service.SecretWebhook
	}
	return service.SecretToken
}

// target describes where a channel sends, without any credential.
func target(kind string, cfg json.RawMessage, sec *ChannelSecret) string {
	switch kind {
	case KindSMTP:
		var c SMTPConfig
		_ = json.Unmarshal(cfg, &c)
		return fmt.Sprintf("smtp://%s:%d (%s) -> %s", c.Host, c.Port, c.TLS, strings.Join(c.To, ", "))
	case KindWebhook:
		if sec == nil {
			return "webhook"
		}
		return redactURL(sec.URL)
	case KindNtfy:
		var c NtfyConfig
		_ = json.Unmarshal(cfg, &c)
		return strings.TrimRight(c.Server, "/") + "/" + c.Topic
	case KindGotify:
		var c GotifyConfig
		_ = json.Unmarshal(cfg, &c)
		return c.URL
	}
	return ""
}

// redactURL keeps scheme and host only: paths and queries of webhook URLs often carry tokens.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "(invalid url)"
	}
	r := u.Scheme + "://" + u.Host
	if u.Path != "" && u.Path != "/" {
		r += "/…"
	}
	return r
}

var nameOK = func(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

const channelCols = `id::text, name, kind, config, COALESCE(secret_id::text,''), enabled, rate_per_hour, timeout_seconds, created`

type channelRow struct {
	Channel
	secretID string
}

func scanChannel(r pgx.Row) (*channelRow, error) {
	var c channelRow
	err := r.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.secretID, &c.Enabled, &c.RatePerHour, &c.TimeoutSeconds, &c.Created)
	c.HasSecret = c.secretID != ""
	return &c, err
}

func (e *Engine) loadSecret(ctx context.Context, id string) (*ChannelSecret, error) {
	if id == "" {
		return &ChannelSecret{}, nil
	}
	raw, err := e.Svc.ReadSecret(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading the channel secret: %w", err)
	}
	var s ChannelSecret
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errors.New("channel secret is malformed")
	}
	return &s, nil
}

func (e *Engine) publicChannel(ctx context.Context, c *channelRow) Channel {
	out := c.Channel
	var sec *ChannelSecret
	if c.Kind == KindWebhook && c.secretID != "" {
		sec, _ = e.loadSecret(ctx, c.secretID) // only to show scheme://host of the URL
	}
	out.Target = target(c.Kind, c.Config, sec)
	return out
}

// ListChannels returns every channel (without secrets).
func (e *Engine) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := e.Svc.DB.Pool().Query(ctx, `SELECT `+channelCols+` FROM notify_channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var crs []*channelRow
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		crs = append(crs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []Channel{}
	for _, c := range crs {
		out = append(out, e.publicChannel(ctx, c))
	}
	return out, nil
}

func (e *Engine) channelRow(ctx context.Context, id string) (*channelRow, error) {
	c, err := scanChannel(e.Svc.DB.Pool().QueryRow(ctx, `SELECT `+channelCols+` FROM notify_channels WHERE id::text=$1 OR name=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: channel %q", ErrNotFound, id)
	}
	return c, err
}

// GetChannel returns one channel by id or name.
func (e *Engine) GetChannel(ctx context.Context, id string) (*Channel, error) {
	c, err := e.channelRow(ctx, id)
	if err != nil {
		return nil, err
	}
	pc := e.publicChannel(ctx, c)
	return &pc, nil
}

func newUUID(ctx context.Context, e *Engine) (string, error) {
	var id string
	err := e.Svc.DB.Pool().QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id)
	return id, err
}

// CreateChannel adds a channel; its secret is sealed in the secret store.
func (e *Engine) CreateChannel(ctx context.Context, in ChannelInput) (*Channel, error) {
	if !nameOK(in.Name) {
		return nil, bad("invalid channel name (letters, digits, '-', '_', '.')")
	}
	cfg, err := validateChannel(in.Kind, in.Config, in.Secret)
	if err != nil {
		return nil, err
	}
	id, err := newUUID(ctx, e)
	if err != nil {
		return nil, err
	}
	var secretID *string
	if !in.Secret.empty() {
		raw, _ := json.Marshal(in.Secret)
		sid, err := e.Svc.CreateSecret(ctx, "notify-"+id, secretKind(in.Kind), raw)
		if err != nil {
			return nil, err
		}
		secretID = &sid
	}
	enabled, rate, timeout := true, 30, 15
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.RatePerHour != nil {
		rate = *in.RatePerHour
	}
	if in.TimeoutSeconds != nil {
		timeout = *in.TimeoutSeconds
	}
	_, err = e.Svc.DB.Pool().Exec(ctx, `INSERT INTO notify_channels (id, name, kind, config, secret_id, enabled, rate_per_hour, timeout_seconds, created)
		VALUES ($1::uuid,$2,$3,$4,$5::uuid,$6,$7,$8,$9)`, id, in.Name, in.Kind, cfg, secretID, enabled, rate, timeout, e.now())
	if err != nil {
		if secretID != nil {
			_ = e.Svc.DeleteSecret(ctx, *secretID)
		}
		if strings.Contains(err.Error(), "notify_channels_name_key") {
			return nil, fmt.Errorf("%w: a channel named %q exists", ErrConflict, in.Name)
		}
		return nil, bad("channel: %s", dbMsg(err))
	}
	return e.GetChannel(ctx, id)
}

// dbMsg reduces a database error to its message (never echoes values).
func dbMsg(err error) string {
	s := err.Error()
	if i := strings.Index(s, " (SQLSTATE"); i > 0 {
		s = s[:i]
	}
	return s
}

// UpdateChannel changes a channel. The kind cannot change.
func (e *Engine) UpdateChannel(ctx context.Context, id string, in ChannelInput) (*Channel, error) {
	cur, err := e.channelRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Kind != "" && in.Kind != cur.Kind {
		return nil, bad("the kind of a channel cannot change (create a new one)")
	}
	name := cur.Name
	if in.Name != "" {
		if !nameOK(in.Name) {
			return nil, bad("invalid channel name")
		}
		name = in.Name
	}
	sec, err := e.loadSecret(ctx, cur.secretID)
	if err != nil {
		return nil, err
	}
	if in.Secret != nil {
		sec = in.Secret
	}
	cfgRaw := cur.Config
	if len(in.Config) > 0 {
		cfgRaw = in.Config
	}
	cfg, err := validateChannel(cur.Kind, cfgRaw, sec)
	if err != nil {
		return nil, err
	}
	enabled, rate, timeout := cur.Enabled, cur.RatePerHour, cur.TimeoutSeconds
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.RatePerHour != nil {
		rate = *in.RatePerHour
	}
	if in.TimeoutSeconds != nil {
		timeout = *in.TimeoutSeconds
	}
	secretID := cur.secretID
	var dropSecret string
	if in.Secret != nil {
		raw, _ := json.Marshal(in.Secret)
		switch {
		case in.Secret.empty():
			dropSecret, secretID = cur.secretID, ""
		case cur.secretID != "":
			if err := e.Svc.RotateSecret(ctx, cur.secretID, raw); err != nil {
				return nil, err
			}
		default:
			if secretID, err = e.Svc.CreateSecret(ctx, "notify-"+cur.ID, secretKind(cur.Kind), raw); err != nil {
				return nil, err
			}
		}
	}
	_, err = e.Svc.DB.Pool().Exec(ctx, `UPDATE notify_channels SET name=$2, config=$3, secret_id=NULLIF($4,'')::uuid, enabled=$5, rate_per_hour=$6, timeout_seconds=$7
		WHERE id=$1::uuid`, cur.ID, name, cfg, secretID, enabled, rate, timeout)
	if err != nil {
		if strings.Contains(err.Error(), "notify_channels_name_key") {
			return nil, fmt.Errorf("%w: a channel named %q exists", ErrConflict, name)
		}
		return nil, bad("channel: %s", dbMsg(err))
	}
	if dropSecret != "" {
		_ = e.Svc.DeleteSecret(ctx, dropSecret)
	}
	return e.GetChannel(ctx, cur.ID)
}

// DeleteChannel removes a channel, its secret and its queued deliveries, and drops it from rules.
func (e *Engine) DeleteChannel(ctx context.Context, id string) error {
	cur, err := e.channelRow(ctx, id)
	if err != nil {
		return err
	}
	tx, err := e.Svc.DB.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE notify_rules SET channel_ids=array_remove(channel_ids, $1::uuid) WHERE $1::uuid = ANY(channel_ids)`, cur.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM notify_channels WHERE id=$1::uuid`, cur.ID); err != nil {
		return err
	}
	if cur.secretID != "" {
		if _, err := tx.Exec(ctx, `DELETE FROM credentials WHERE id=$1::uuid`, cur.secretID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
