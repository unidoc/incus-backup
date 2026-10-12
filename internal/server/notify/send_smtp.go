package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// tlsAuth wraps an smtp.Auth mechanism and refuses to run without TLS, whatever the host
// (net/smtp's PlainAuth allows localhost in clear, which would hide a misconfiguration in tests).
type plainAuth struct{ user, pass string }

func (a plainAuth) Start(info *smtp.ServerInfo) (string, []byte, error) {
	if !info.TLS {
		return "", nil, errors.New("refusing to authenticate over an unencrypted connection")
	}
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

type loginAuth struct {
	user, pass string
	step       int
}

func (a *loginAuth) Start(info *smtp.ServerInfo) (string, []byte, error) {
	if !info.TLS {
		return "", nil, errors.New("refusing to authenticate over an unencrypted connection")
	}
	a.step = 0
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	q := strings.ToLower(strings.TrimSpace(string(fromServer)))
	switch {
	case strings.HasPrefix(q, "user") || a.step == 1 && !strings.HasPrefix(q, "pass"):
		return []byte(a.user), nil
	case strings.HasPrefix(q, "pass") || a.step == 2:
		return []byte(a.pass), nil
	}
	return nil, errors.New("unexpected LOGIN challenge")
}

func smtpTLSConfig(c SMTPConfig) (*tls.Config, error) {
	cfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	if c.TLSServerName != "" {
		cfg.ServerName = c.TLSServerName
	}
	if c.TLSCAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(c.TLSCAPEM)) {
			return nil, errors.New("tls_ca_pem holds no certificate")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func encodeHeader(s string) string { return mime.QEncoding.Encode("utf-8", clean(s, 400)) }

// buildMail renders a multipart/alternative (text + HTML) message.
func buildMail(c SMTPConfig, m Message, now time.Time) ([]byte, error) {
	var b bytes.Buffer
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, part := range []struct{ ctype, content string }{{"text/plain; charset=utf-8", m.Text}, {"text/html; charset=utf-8", m.HTML}} {
		if part.content == "" {
			continue
		}
		pw, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {part.ctype}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(pw)
		if _, err := qp.Write([]byte(part.content)); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	from, _ := mail.ParseAddress(c.From)
	var to []string
	for _, t := range c.To {
		if a, err := mail.ParseAddress(t); err == nil {
			to = append(to, a.String())
		}
	}
	var id [12]byte
	_, _ = rand.Read(id[:])
	domain := "incus-backup.local"
	if i := strings.LastIndex(from.Address, "@"); i >= 0 {
		domain = from.Address[i+1:]
	}
	hdr := [][2]string{
		{"From", from.String()},
		{"To", strings.Join(to, ", ")},
		{"Subject", encodeHeader(m.Subject)},
		{"Date", now.Format(time.RFC1123Z)},
		{"Message-ID", "<" + hex.EncodeToString(id[:]) + "@" + domain + ">"},
		{"MIME-Version", "1.0"},
		{"Content-Type", "multipart/alternative; boundary=" + strconv.Quote(mw.Boundary())},
		{"Auto-Submitted", "auto-generated"},
		{"X-Incus-Backup-Kind", m.Kind},
		{"X-Incus-Backup-Severity", m.Severity},
	}
	for _, h := range hdr {
		b.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	b.WriteString("\r\n")
	b.Write(body.Bytes())
	return b.Bytes(), nil
}

func (e *Engine) sendSMTP(ctx context.Context, cfgRaw json.RawMessage, sec *ChannelSecret, m Message, timeout time.Duration, now time.Time) (err error) {
	var c SMTPConfig
	if err := json.Unmarshal(cfgRaw, &c); err != nil {
		return errors.New("smtp: bad config")
	}
	tcfg, err := smtpTLSConfig(c)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp: connecting to %s: %v", addr, unwrapNet(err))
	}
	deadline := time.Now().Add(timeout) // network deadline: wall clock, not the service clock
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if c.TLS == "tls" {
		tc := tls.Client(conn, tcfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return fmt.Errorf("smtp: TLS handshake with %s: %v", addr, err)
		}
		conn = tc
	}
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: %s: %v", addr, unwrapNet(err))
	}
	defer cl.Close()
	helo := c.HELO
	if helo == "" {
		helo = "localhost"
	}
	if err := cl.Hello(helo); err != nil {
		return fmt.Errorf("smtp: EHLO: %v", smtpReply(err))
	}
	if c.TLS == "starttls" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("smtp: the server does not offer STARTTLS (refusing to continue in clear)")
		}
		if err := cl.StartTLS(tcfg); err != nil {
			return fmt.Errorf("smtp: STARTTLS: %v", smtpReply(err))
		}
	}
	if c.Username != "" {
		if sec == nil || sec.Password == "" {
			return errors.New("smtp: no password stored")
		}
		mech := c.Auth
		if mech == "" {
			_, list := cl.Extension("AUTH")
			l := " " + strings.ToUpper(list) + " "
			switch {
			case strings.Contains(l, " PLAIN "):
				mech = "plain"
			case strings.Contains(l, " LOGIN "):
				mech = "login"
			default:
				return fmt.Errorf("smtp: the server offers no usable AUTH mechanism (%q)", list)
			}
		}
		var a smtp.Auth = plainAuth{c.Username, sec.Password}
		if mech == "login" {
			a = &loginAuth{user: c.Username, pass: sec.Password}
		}
		if err := cl.Auth(a); err != nil {
			return fmt.Errorf("smtp: authentication failed: %v", smtpReply(err))
		}
	}
	raw, err := buildMail(c, m, now)
	if err != nil {
		return err
	}
	from, _ := mail.ParseAddress(c.From)
	if err := cl.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp: MAIL FROM: %v", smtpReply(err))
	}
	for _, t := range c.To {
		a, _ := mail.ParseAddress(t)
		if err := cl.Rcpt(a.Address); err != nil {
			return fmt.Errorf("smtp: RCPT TO %s: %v", a.Address, smtpReply(err))
		}
	}
	w, err := cl.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %v", smtpReply(err))
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("smtp: writing the message: %v", unwrapNet(err))
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: message rejected: %v", smtpReply(err))
	}
	_ = cl.Quit()
	return nil
}

// smtpReply shows a server reply as "535 5.7.8 text" (the reply the UI shows the operator).
func smtpReply(err error) string {
	var te *textproto.Error
	if errors.As(err, &te) {
		return fmt.Sprintf("%03d %s", te.Code, te.Msg)
	}
	return unwrapNet(err).Error()
}

func unwrapNet(err error) error {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		if oe.Timeout() {
			return errors.New("timed out")
		}
		return oe.Err
	}
	return err
}
