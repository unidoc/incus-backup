package notify_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/server/notify"
)

func smtpConfig(port int, tlsMode, caPEM, user string) json.RawMessage {
	c := map[string]any{"host": "127.0.0.1", "port": port, "tls": tlsMode, "from": "Backup Server <backup@example.com>",
		"to": []string{"ops@example.com", "Ann Ó <ann@example.com>"}}
	if caPEM != "" {
		c["tls_ca_pem"] = caPEM
	}
	if user != "" {
		c["username"] = user
	}
	b, _ := json.Marshal(c)
	return b
}

// parseMail reads a received message the way a mail client does: headers (RFC 2047 decoded) and
// the multipart/alternative parts (quoted-printable decoded by mime/multipart).
func parseMail(t *testing.T, raw []byte) (subject string, h mail.Header, parts map[string]string) {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a mail message: %v\n%s", err, raw)
	}
	dec := new(mime.WordDecoder)
	subject, err = dec.DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q: %v", m.Header.Get("Content-Type"), err)
	}
	parts = map[string]string{}
	mr := multipart.NewReader(m.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		b, _ := io.ReadAll(p)
		parts[ct] = string(b)
	}
	return subject, m.Header, parts
}

func TestSMTPStartTLSAuthPlainAndLogin(t *testing.T) {
	e := newEnv(t)
	for _, mech := range []string{"", "login"} {
		srv, ca := newFakeSMTP(t, false, true, "mailer", "s3cr3t-Pa55")
		cfg := smtpConfig(srv.port(), "starttls", ca, "mailer")
		if mech != "" {
			var m map[string]any
			_ = json.Unmarshal(cfg, &m)
			m["auth"] = mech
			cfg, _ = json.Marshal(m)
		}
		ch, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "mail-" + mech + "x", Kind: notify.KindSMTP, Config: cfg,
			Secret: &notify.ChannelSecret{Password: "s3cr3t-Pa55"}})
		if err != nil {
			t.Fatal(err)
		}
		d, err := e.eng.TestChannel(e.ctx, ch.ID)
		if err != nil {
			t.Fatalf("%s: %v", mech, err)
		}
		if d.State != "sent" || d.Kind != "test" {
			t.Fatalf("test delivery: %+v", d)
		}
		ms := srv.got()
		if len(ms) != 1 || !ms[0].TLS || ms[0].From != "backup@example.com" || len(ms[0].To) != 2 || ms[0].To[1] != "ann@example.com" {
			t.Fatalf("mail: %+v", ms)
		}
		want := "PLAIN"
		if mech == "login" {
			want = "LOGIN"
		}
		if ms[0].Authed != want {
			t.Fatalf("auth mechanism %q, want %q", ms[0].Authed, want)
		}
		subj, h, parts := parseMail(t, ms[0].Data)
		if subj != "[TEST] incus-backup notification test" || !strings.Contains(parts["text/plain"], "If you can read it") || !strings.Contains(parts["text/html"], "<p>") {
			t.Fatalf("parsed: %q %v", subj, parts)
		}
		if to, _ := h.AddressList("To"); len(to) != 2 || to[1].Name != "Ann Ó" {
			t.Fatalf("To header: %v", h.Get("To"))
		}
		// nothing secret went over the wire before STARTTLS
		for _, l := range srv.clearLog {
			if strings.Contains(strings.ToUpper(l), "AUTH") {
				t.Fatalf("AUTH before TLS: %q", l)
			}
		}
	}
}

func TestSMTPAlertMailContent(t *testing.T) {
	e := newEnv(t)
	_ = e.eng.SetBaseURL(e.ctx, "https://console.example")
	srv, ca := newFakeSMTP(t, true, false, "", "")
	ch, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "mail", Kind: notify.KindSMTP, Config: smtpConfig(srv.port(), "tls", ca, "")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.CreateRule(e.ctx, notify.DefaultRule("all", ch.ID)); err != nil {
		t.Fatal(err)
	}
	// a host-supplied name with a header injection attempt and non-ASCII
	e.policy("daily", "03:00")
	hid := e.host("node1", ptr(e.clock()))
	g := e.object(hid, "node1", "default", "Bösch\r\nBcc: evil@example.com", "daily", e.clock().Add(-30*time.Hour))
	_ = g
	e.advance(8 * time.Hour)
	e.exec(`UPDATE hosts SET last_seen_at=$1`, e.clock())
	e.tick()
	ms := srv.got()
	if len(ms) != 1 || !ms[0].TLS {
		t.Fatalf("mails: %+v", ms)
	}
	subj, h, parts := parseMail(t, ms[0].Data)
	if subj != "[WARNING] Bösch  Bcc: evil@example.com: never backed up (known for 38 h)" {
		t.Fatalf("subject %q", subj)
	}
	if h.Get("Bcc") != "" || len(ms[0].To) != 2 {
		t.Fatalf("header injection: %v / %v", h, ms[0].To)
	}
	if !strings.Contains(parts["text/plain"], "Open in the console: https://console.example/alerts/") || !strings.Contains(parts["text/plain"], "What to do:") {
		t.Fatalf("text part: %s", parts["text/plain"])
	}
	if !strings.Contains(parts["text/html"], `<a href="https://console.example/alerts/`) || strings.Contains(parts["text/html"], "<script") {
		t.Fatalf("html part: %s", parts["text/html"])
	}
	if h.Get("Auto-Submitted") != "auto-generated" || h.Get("X-Incus-Backup-Severity") != "warning" {
		t.Fatalf("headers: %v", h)
	}
}

func TestSMTPRefusals(t *testing.T) {
	e := newEnv(t)
	// a password over a clear connection is refused at configuration time
	if _, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "clear", Kind: notify.KindSMTP, Config: smtpConfig(25, "none", "", "u"),
		Secret: &notify.ChannelSecret{Password: "p"}}); !errors.Is(err, notify.ErrBadRequest) {
		t.Fatalf("clear-text auth accepted: %v", err)
	}
	// starttls required but not offered: refuses to continue in clear
	srv, _ := newFakeSMTP(t, false, false, "", "")
	ch, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "notls", Kind: notify.KindSMTP, Config: smtpConfig(srv.port(), "starttls", "", "")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.TestChannel(e.ctx, ch.ID); err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Fatalf("missing STARTTLS: %v", err)
	}
	// an untrusted certificate is refused (no tls_ca_pem)
	srv2, _ := newFakeSMTP(t, false, true, "", "")
	ch2, _ := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "untrusted", Kind: notify.KindSMTP, Config: smtpConfig(srv2.port(), "starttls", "", "")})
	if _, err := e.eng.TestChannel(e.ctx, ch2.ID); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted cert: %v", err)
	}
	// wrong password: the server's answer reaches the UI, the password does not
	srv3, ca3 := newFakeSMTP(t, false, true, "mailer", "right-password")
	ch3, _ := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "wrongpw", Kind: notify.KindSMTP, Config: smtpConfig(srv3.port(), "starttls", ca3, "mailer"),
		Secret: &notify.ChannelSecret{Password: "wrong-password-123"}})
	_, err = e.eng.TestChannel(e.ctx, ch3.ID)
	if err == nil || !strings.Contains(err.Error(), "535 5.7.8 authentication credentials invalid") || strings.Contains(err.Error(), "wrong-password-123") {
		t.Fatalf("wrong password: %v", err)
	}
	// transient RCPT failure: error text carries the SMTP reply
	srv4, ca4 := newFakeSMTP(t, false, true, "", "")
	srv4.mu.Lock()
	srv4.rcptFail = true
	srv4.mu.Unlock()
	ch4, _ := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "rcpt", Kind: notify.KindSMTP, Config: smtpConfig(srv4.port(), "starttls", ca4, "")})
	if _, err := e.eng.TestChannel(e.ctx, ch4.ID); err == nil || !strings.Contains(err.Error(), "451 4.3.0") {
		t.Fatalf("rcpt failure: %v", err)
	}
	// unreachable server and a timeout
	ch5, _ := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "down", Kind: notify.KindSMTP, Config: smtpConfig(1, "starttls", "", "")})
	if _, err := e.eng.TestChannel(e.ctx, ch5.ID); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("down: %v", err)
	}
	// a typo putting the password into config is refused (unknown field), so it is never stored in clear
	if _, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "typo", Kind: notify.KindSMTP,
		Config: json.RawMessage(`{"host":"h","from":"a@b.c","to":["c@d.e"],"password":"oops"}`)}); !errors.Is(err, notify.ErrBadRequest) {
		t.Fatalf("password in config accepted: %v", err)
	}
}

func TestWebhookSignatureFormatsAndHeaders(t *testing.T) {
	e := newEnv(t)
	rcv := newReceiver(t)
	ch, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "hook", Kind: notify.KindWebhook,
		Config: json.RawMessage(`{"format":"json","headers":{"X-Team":"ops"}}`),
		Secret: &notify.ChannelSecret{URL: rcv.URL + "/in/T0KEN-path", HMACSecret: "sign-me", Headers: map[string]string{"Authorization": "Bearer hdr-secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.TestChannel(e.ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	q := rcv.requests()[0]
	ts := q.Header.Get(notify.TimestampHeader)
	mac := hmac.New(sha256.New, []byte("sign-me"))
	mac.Write([]byte(ts + "."))
	mac.Write(q.Body)
	if got := q.Header.Get(notify.DefaultSignatureHeader); got != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature %q", got)
	}
	if ts != fmt.Sprint(e.clock().Unix()) || q.Header.Get("Authorization") != "Bearer hdr-secret" || q.Header.Get("X-Team") != "ops" || q.Path != "/in/T0KEN-path" {
		t.Fatalf("headers: %v %s", q.Header, q.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(q.Body, &body); err != nil || body["kind"] != "test" || body["version"].(float64) != 1 {
		t.Fatalf("body: %s", q.Body)
	}
	// a tampered body fails verification (the receiver's check is real)
	mac2 := hmac.New(sha256.New, []byte("sign-me"))
	mac2.Write([]byte(ts + "."))
	mac2.Write(append(q.Body, ' '))
	if hmac.Equal(mac2.Sum(nil), mac.Sum(nil)) {
		t.Fatal("signature does not cover the body")
	}

	// slack and discord payloads; untrusted text cannot mention @channel/@everyone
	for _, f := range []string{"slack", "discord"} {
		r2 := newReceiver(t)
		c2, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: f, Kind: notify.KindWebhook, Config: json.RawMessage(`{"format":"` + f + `"}`),
			Secret: &notify.ChannelSecret{URL: r2.URL + "/services/X"}})
		if err != nil {
			t.Fatal(err)
		}
		rule := notify.DefaultRule("r-"+f, c2.ID)
		rule.HostGlob = f
		if _, err := e.eng.CreateRule(e.ctx, rule); err != nil {
			t.Fatal(err)
		}
		if err := e.eng.Emit(e.ctx, notify.Event{Type: "job_failed", Key: f, Host: f, Title: "job <!channel> @everyone failed"}); err != nil {
			t.Fatal(err)
		}
		e.eng.Deliver(e.ctx)
		var got map[string]any
		reqs := r2.requests()
		if len(reqs) != 1 {
			t.Fatalf("%s: %d requests", f, len(reqs))
		}
		_ = json.Unmarshal(reqs[0].Body, &got)
		if f == "slack" {
			txt, _ := got["text"].(string)
			if strings.Contains(txt, "<!channel>") || !strings.Contains(txt, "&lt;!channel&gt;") {
				t.Fatalf("slack text not escaped: %q", txt)
			}
		} else {
			am, _ := got["allowed_mentions"].(map[string]any)
			if content, _ := got["content"].(string); content == "" || am == nil || len(am["parse"].([]any)) != 0 {
				t.Fatalf("discord payload: %s", reqs[0].Body)
			}
		}
		if _, ok := got["kind"]; ok {
			t.Fatalf("%s got the JSON format", f)
		}
	}
	// redirects are not followed (secret headers would follow them)
	target := newReceiver(t)
	redir := httptest.NewServer(http.RedirectHandler(target.URL+"/stolen", http.StatusFound))
	defer redir.Close()
	c3, _ := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "redir", Kind: notify.KindWebhook, Config: json.RawMessage(`{}`),
		Secret: &notify.ChannelSecret{URL: redir.URL, Headers: map[string]string{"Authorization": "Bearer x-123456"}}})
	if _, err := e.eng.TestChannel(e.ctx, c3.ID); err == nil || !strings.Contains(err.Error(), "302") || len(target.requests()) != 0 {
		t.Fatalf("redirect followed: %v", err)
	}
}

func TestNtfyAndGotify(t *testing.T) {
	e := newEnv(t)
	_ = e.eng.SetBaseURL(e.ctx, "https://console.example")
	ntfy := newReceiver(t)
	got := newReceiver(t)
	cn, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "ntfy", Kind: notify.KindNtfy,
		Config: json.RawMessage(fmt.Sprintf(`{"server":%q,"topic":"backups","priorities":{"warning":3}}`, ntfy.URL)),
		Secret: &notify.ChannelSecret{Token: "tk_ntfy_secret"}})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "ntfy-basic", Kind: notify.KindNtfy,
		Config: json.RawMessage(fmt.Sprintf(`{"server":%q,"topic":"b2","username":"phone"}`, ntfy.URL)),
		Secret: &notify.ChannelSecret{Password: "basic-pw-1"}})
	if err != nil {
		t.Fatal(err)
	}
	cg, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "gotify", Kind: notify.KindGotify,
		Config: json.RawMessage(fmt.Sprintf(`{"url":%q}`, got.URL+"/gotify/")), Secret: &notify.ChannelSecret{Token: "AppT0ken"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.CreateRule(e.ctx, notify.DefaultRule("push", cn.ID, cb.ID, cg.ID)); err != nil {
		t.Fatal(err)
	}
	e.host("node1", ptr(e.clock().Add(-25*time.Hour))) // critical
	e.host("node2", ptr(e.clock().Add(-3*time.Hour)))  // warning
	e.tick()
	// ntfy: JSON publish to the root, bearer token / basic auth, mapped priority, click link
	reqs := ntfy.requests()
	if len(reqs) != 4 {
		t.Fatalf("ntfy requests: %d", len(reqs))
	}
	byTopic := map[string][]map[string]any{}
	for _, q := range reqs {
		var m map[string]any
		if err := json.Unmarshal(q.Body, &m); err != nil {
			t.Fatal(err)
		}
		if q.Path != "/" {
			t.Fatalf("ntfy path %q", q.Path)
		}
		topic := m["topic"].(string)
		switch topic {
		case "backups":
			if q.Header.Get("Authorization") != "Bearer tk_ntfy_secret" {
				t.Fatalf("ntfy token: %v", q.Header)
			}
		case "b2":
			if q.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("phone:basic-pw-1")) {
				t.Fatalf("ntfy basic: %v", q.Header)
			}
		}
		byTopic[topic] = append(byTopic[topic], m)
	}
	for _, m := range byTopic["backups"] {
		title := m["title"].(string)
		p := m["priority"].(float64)
		if strings.HasPrefix(title, "[CRITICAL]") && p != 5 || strings.HasPrefix(title, "[WARNING]") && p != 3 {
			t.Fatalf("ntfy priority: %v", m)
		}
		if !strings.HasPrefix(m["click"].(string), "https://console.example/alerts/") {
			t.Fatalf("ntfy click: %v", m)
		}
	}
	// gotify: X-Gotify-Key header (never ?token=), /message, priority mapping
	greqs := got.requests()
	if len(greqs) != 2 {
		t.Fatalf("gotify: %d", len(greqs))
	}
	for _, q := range greqs {
		if q.Path != "/gotify/message" || q.Header.Get("X-Gotify-Key") != "AppT0ken" || strings.Contains(q.Path, "token") {
			t.Fatalf("gotify request: %s %v", q.Path, q.Header)
		}
		var m map[string]any
		_ = json.Unmarshal(q.Body, &m)
		p := m["priority"].(float64)
		if !(strings.HasPrefix(m["title"].(string), "[CRITICAL]") && p == 9 || strings.HasPrefix(m["title"].(string), "[WARNING]") && p == 6) {
			t.Fatalf("gotify priority: %v", m)
		}
	}
	// HTTP errors reach the UI with the status (and no secret)
	got.setStatus(`{"error":"Unauthorized","errorDescription":"you need to provide a valid access token AppT0ken"}`, 401)
	_, err = e.eng.TestChannel(e.ctx, cg.ID)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401 Unauthorized") || strings.Contains(err.Error(), "AppT0ken") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("gotify 401: %v", err)
	}
}

// Secrets never appear in any listing, delivery record, error text or log line.
func TestSecretsNeverLeak(t *testing.T) {
	e := newEnv(t)
	secrets := []string{"smtp-Pa55word-XYZ", "ntfy-tok-ABCDEF", "gotify-tok-GHIJKL", "hmac-sec-MNOPQR", "Bearer hdr-sec-STUVWX", "slack-path-T0K3N"}
	srv, ca := newFakeSMTP(t, false, true, "mailer", secrets[0])
	rcv := newReceiver(t)
	rcv.setStatus("denied", 403)
	rcv.mu.Lock()
	rcv.echo = true // a verbose receiver echoing the credentials it got
	rcv.mu.Unlock()
	chs := []notify.ChannelInput{
		{Name: "mail", Kind: notify.KindSMTP, Config: smtpConfig(srv.port(), "starttls", ca, "mailer"), Secret: &notify.ChannelSecret{Password: secrets[0]}},
		{Name: "wrongmail", Kind: notify.KindSMTP, Config: smtpConfig(srv.port(), "starttls", ca, "mailer"), Secret: &notify.ChannelSecret{Password: secrets[0] + "-bad"}},
		{Name: "ntfy", Kind: notify.KindNtfy, Config: json.RawMessage(fmt.Sprintf(`{"server":%q,"topic":"t"}`, rcv.URL)), Secret: &notify.ChannelSecret{Token: secrets[1]}},
		{Name: "gotify", Kind: notify.KindGotify, Config: json.RawMessage(fmt.Sprintf(`{"url":%q}`, rcv.URL)), Secret: &notify.ChannelSecret{Token: secrets[2]}},
		{Name: "hook", Kind: notify.KindWebhook, Config: json.RawMessage(`{"format":"slack"}`),
			Secret: &notify.ChannelSecret{URL: rcv.URL + "/services/" + secrets[5], HMACSecret: secrets[3], Headers: map[string]string{"Authorization": secrets[4]}}},
		{Name: "deadhook", Kind: notify.KindWebhook, Config: json.RawMessage(`{}`), Secret: &notify.ChannelSecret{URL: "http://127.0.0.1:1/services/" + secrets[5]}},
	}
	var ids []string
	var errs []string
	for _, in := range chs {
		c, err := e.eng.CreateChannel(e.ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
		if _, err := e.eng.TestChannel(e.ctx, c.ID); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) != 5 { // only "mail" succeeds
		t.Fatalf("errors: %v", errs)
	}
	if _, err := e.eng.CreateRule(e.ctx, notify.DefaultRule("all", ids...)); err != nil {
		t.Fatal(err)
	}
	e.host("node1", ptr(e.clock().Add(-3*time.Hour)))
	e.tick()
	// update a channel (rotate) and fail again
	if _, err := e.eng.UpdateChannel(e.ctx, "ntfy", notify.ChannelInput{Secret: &notify.ChannelSecret{Token: secrets[1] + "2"}}); err != nil {
		t.Fatal(err)
	}
	e.advance(10 * time.Minute)
	e.eng.Deliver(e.ctx)

	var blobs []string
	add := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		blobs = append(blobs, string(b))
	}
	cl, _ := e.eng.ListChannels(e.ctx)
	add(cl)
	for _, id := range ids {
		c, _ := e.eng.GetChannel(e.ctx, id)
		add(c)
	}
	add(e.deliveries(notify.DeliveryFilter{Limit: 1000}))
	for _, a := range e.alerts(notify.AlertFilter{Limit: 100}) {
		d, _ := e.eng.GetAlert(e.ctx, a.ID)
		add(d)
	}
	rl, _ := e.eng.ListRules(e.ctx)
	add(rl)
	creds, err := e.svc.Credentials(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	add(creds)
	blobs = append(blobs, errs...)
	blobs = append(blobs, e.logs.String())
	// the database itself: everything except the sealed column
	var dump string
	if err := e.d.Pool().QueryRow(e.ctx, `SELECT concat_ws('|', (SELECT string_agg(row_to_json(c)::text, ',') FROM notify_channels c),
		(SELECT string_agg(row_to_json(d)::text, ',') FROM notify_deliveries d), (SELECT string_agg(row_to_json(a)::text, ',') FROM notify_alerts a),
		(SELECT string_agg(encode(sealed, 'escape'), ',') FROM credentials))`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	blobs = append(blobs, dump)
	all := strings.Join(blobs, "\n")
	if len(e.deliveries(notify.DeliveryFilter{Limit: 1000})) < 10 || !strings.Contains(all, "[redacted]") {
		t.Fatalf("the test did not exercise failing deliveries")
	}
	for _, s := range append(secrets, secrets[1]+"2") {
		for _, form := range []string{s, base64.StdEncoding.EncodeToString([]byte(s)), base64.StdEncoding.EncodeToString([]byte("\x00mailer\x00" + s))} {
			if strings.Contains(all, form) {
				i := strings.Index(all, form)
				lo := i - 200
				if lo < 0 {
					lo = 0
				}
				t.Fatalf("secret %q leaked (as %q) near: %s", s, form, all[lo:i+len(form)])
			}
		}
	}
	// the target shown for a webhook is scheme://host only
	for _, c := range cl {
		if c.Kind == notify.KindWebhook && strings.Contains(c.Target, "services") {
			t.Fatalf("webhook target shows the path: %s", c.Target)
		}
		if !c.HasSecret {
			t.Fatalf("has_secret false for %s", c.Name)
		}
	}
	// deleting a channel removes its sealed secret too
	if err := e.eng.DeleteChannel(e.ctx, "ntfy"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = e.d.Pool().QueryRow(e.ctx, `SELECT count(*) FROM credentials`).Scan(&n)
	if n != len(chs)-1 {
		t.Fatalf("%d credentials after deleting one of %d channels", n, len(chs))
	}
}

func TestChannelValidationAndCRUD(t *testing.T) {
	e := newEnv(t)
	bad := []notify.ChannelInput{
		{Name: "x y", Kind: notify.KindWebhook, Secret: &notify.ChannelSecret{URL: "https://h/"}},
		{Name: "w", Kind: notify.KindWebhook},                                                                                                    // no URL
		{Name: "w", Kind: notify.KindWebhook, Secret: &notify.ChannelSecret{URL: "ftp://h/"}},                                                    // scheme
		{Name: "w", Kind: notify.KindWebhook, Secret: &notify.ChannelSecret{URL: "https://u:p@h/"}},                                              // userinfo
		{Name: "w", Kind: notify.KindWebhook, Config: json.RawMessage(`{"url":"https://h/"}`), Secret: &notify.ChannelSecret{URL: "https://h/"}}, // url in config
		{Name: "w", Kind: notify.KindWebhook, Config: json.RawMessage(`{"headers":{"X":"a\r\nb"}}`), Secret: &notify.ChannelSecret{URL: "https://h/"}},
		{Name: "g", Kind: notify.KindGotify, Config: json.RawMessage(`{"url":"https://g"}`)}, // no token
		{Name: "n", Kind: notify.KindNtfy, Config: json.RawMessage(`{"topic":"a/b"}`)},
		{Name: "n", Kind: notify.KindNtfy, Config: json.RawMessage(`{"topic":"a","priorities":{"critical":9}}`)},
		{Name: "m", Kind: notify.KindSMTP, Config: json.RawMessage(`{"host":"h","from":"not an address","to":["a@b.c"]}`)},
		{Name: "m", Kind: notify.KindSMTP, Config: json.RawMessage(`{"host":"h","from":"a@b.c","to":[]}`)},
		{Name: "z", Kind: "pager"},
	}
	for i, in := range bad {
		if _, err := e.eng.CreateChannel(e.ctx, in); !errors.Is(err, notify.ErrBadRequest) {
			t.Fatalf("case %d accepted: %v", i, err)
		}
	}
	c, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "n", Kind: notify.KindNtfy, Config: json.RawMessage(`{"topic":"a"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if c.HasSecret || c.Target != "https://ntfy.sh/a" || c.RatePerHour != 30 || c.TimeoutSeconds != 15 {
		t.Fatalf("defaults: %+v", c)
	}
	if _, err := e.eng.CreateChannel(e.ctx, notify.ChannelInput{Name: "n", Kind: notify.KindNtfy, Config: json.RawMessage(`{"topic":"b"}`)}); !errors.Is(err, notify.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	// add then remove a secret
	c, err = e.eng.UpdateChannel(e.ctx, "n", notify.ChannelInput{Secret: &notify.ChannelSecret{Token: "t1234"}, Enabled: ptr(false)})
	if err != nil || !c.HasSecret || c.Enabled {
		t.Fatalf("update: %+v %v", c, err)
	}
	c, err = e.eng.UpdateChannel(e.ctx, "n", notify.ChannelInput{Secret: &notify.ChannelSecret{}, Name: "n2"})
	if err != nil || c.HasSecret || c.Name != "n2" {
		t.Fatalf("remove secret: %+v %v", c, err)
	}
	var n int
	_ = e.d.Pool().QueryRow(e.ctx, `SELECT count(*) FROM credentials`).Scan(&n)
	if n != 0 {
		t.Fatalf("secret left behind: %d", n)
	}
	if _, err := e.eng.UpdateChannel(e.ctx, "n2", notify.ChannelInput{Kind: notify.KindGotify}); !errors.Is(err, notify.ErrBadRequest) {
		t.Fatalf("kind change: %v", err)
	}
	// rules: validation and channel removal on delete
	if _, err := e.eng.CreateRule(e.ctx, notify.Rule{Name: "r"}); !errors.Is(err, notify.ErrBadRequest) {
		t.Fatalf("rule without channel: %v", err)
	}
	r := notify.DefaultRule("r", "n2")
	r.QuietStart = "25:00"
	r.QuietEnd = "07:00"
	if _, err := e.eng.CreateRule(e.ctx, r); !errors.Is(err, notify.ErrBadRequest) {
		t.Fatalf("bad quiet hours: %v", err)
	}
	r.QuietStart, r.Timezone = "22:00", "Mars/Olympus"
	if _, err := e.eng.CreateRule(e.ctx, r); !errors.Is(err, notify.ErrBadRequest) {
		t.Fatalf("bad zone: %v", err)
	}
	r.Timezone = "Europe/Berlin"
	rr, err := e.eng.CreateRule(e.ctx, r)
	if err != nil || rr.ChannelIDs[0] != c.ID {
		t.Fatalf("rule: %+v %v", rr, err)
	}
	rr.RepeatMinutes = 60
	if rr, err = e.eng.UpdateRule(e.ctx, rr.ID, *rr); err != nil || rr.RepeatMinutes != 60 {
		t.Fatalf("update rule: %+v %v", rr, err)
	}
	if err := e.eng.DeleteChannel(e.ctx, "n2"); err != nil {
		t.Fatal(err)
	}
	if rr, _ = e.eng.GetRule(e.ctx, "r"); len(rr.ChannelIDs) != 0 {
		t.Fatalf("deleted channel still in rule: %v", rr.ChannelIDs)
	}
	if err := e.eng.DeleteRule(e.ctx, "r"); err != nil {
		t.Fatal(err)
	}
}
