package notify_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCert is a self-signed certificate for 127.0.0.1 (also its own CA).
func testCert(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "fake mail"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, string(pemCert)
}

type smtpMail struct {
	From   string
	To     []string
	Data   []byte
	TLS    bool
	Authed string // mechanism that succeeded
}

// fakeSMTP is a small RFC 5321 server: STARTTLS (or implicit TLS), AUTH PLAIN/LOGIN offered only
// once the session is encrypted, MAIL/RCPT/DATA recorded.
type fakeSMTP struct {
	t        *testing.T
	ln       net.Listener
	tlsCfg   *tls.Config
	implicit bool
	user     string
	pass     string
	offerTLS bool

	mu       sync.Mutex
	mails    []smtpMail
	authLog  []string // "PLAIN ok", "LOGIN bad" ...
	clearLog []string // commands received before TLS
	rcptFail bool
}

func newFakeSMTP(t *testing.T, implicit, offerTLS bool, user, pass string) (*fakeSMTP, string) {
	t.Helper()
	cert, caPEM := testCert(t)
	f := &fakeSMTP{t: t, tlsCfg: &tls.Config{Certificates: []tls.Certificate{cert}}, implicit: implicit, user: user, pass: pass, offerTLS: offerTLS}
	var err error
	if implicit {
		f.ln, err = tls.Listen("tcp", "127.0.0.1:0", f.tlsCfg)
	} else {
		f.ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := f.ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { f.ln.Close() })
	return f, caPEM
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) got() []smtpMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smtpMail(nil), f.mails...)
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	tp := textproto.NewConn(c)
	secure := f.implicit
	authed := ""
	var cur smtpMail
	_ = tp.PrintfLine("220 fake.test ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		if !secure {
			f.mu.Lock()
			f.clearLog = append(f.clearLog, line)
			f.mu.Unlock()
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			ext := []string{"fake.test"}
			if !secure && f.offerTLS {
				ext = append(ext, "STARTTLS")
			}
			if secure {
				ext = append(ext, "AUTH PLAIN LOGIN")
			}
			ext = append(ext, "8BITMIME")
			for i, e := range ext {
				sep := "-"
				if i == len(ext)-1 {
					sep = " "
				}
				_ = tp.PrintfLine("250%s%s", sep, e)
			}
		case "STARTTLS":
			_ = tp.PrintfLine("220 2.0.0 ready")
			tc := tls.Server(c, f.tlsCfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			c = tc
			tp = textproto.NewConn(tc)
			secure = true
			authed = ""
		case "AUTH":
			if !secure {
				_ = tp.PrintfLine("538 5.7.11 encryption required")
				continue
			}
			mech, initial, _ := strings.Cut(arg, " ")
			var user, pass string
			switch strings.ToUpper(mech) {
			case "PLAIN":
				if initial == "" {
					_ = tp.PrintfLine("334 ")
					initial, _ = tp.ReadLine()
				}
				raw, _ := base64.StdEncoding.DecodeString(initial)
				parts := strings.Split(string(raw), "\x00")
				if len(parts) == 3 {
					user, pass = parts[1], parts[2]
				}
			case "LOGIN":
				_ = tp.PrintfLine("334 VXNlcm5hbWU6")
				u, _ := tp.ReadLine()
				_ = tp.PrintfLine("334 UGFzc3dvcmQ6")
				p, _ := tp.ReadLine()
				ub, _ := base64.StdEncoding.DecodeString(u)
				pb, _ := base64.StdEncoding.DecodeString(p)
				user, pass = string(ub), string(pb)
			default:
				_ = tp.PrintfLine("504 5.5.4 mechanism not supported")
				continue
			}
			ok := user == f.user && pass == f.pass
			f.mu.Lock()
			f.authLog = append(f.authLog, strings.ToUpper(mech)+map[bool]string{true: " ok", false: " bad"}[ok])
			f.mu.Unlock()
			if !ok {
				_ = tp.PrintfLine("535 5.7.8 authentication credentials invalid")
				continue
			}
			authed = strings.ToUpper(mech)
			_ = tp.PrintfLine("235 2.7.0 authentication successful")
		case "MAIL":
			if f.user != "" && authed == "" {
				_ = tp.PrintfLine("530 5.7.0 authentication required")
				continue
			}
			addr, _, _ := strings.Cut(arg[5:], " ") // "FROM:<a@b> BODY=8BITMIME"
			cur = smtpMail{From: strings.Trim(addr, "<>"), TLS: secure, Authed: authed}
			_ = tp.PrintfLine("250 2.1.0 ok")
		case "RCPT":
			f.mu.Lock()
			fail := f.rcptFail
			f.mu.Unlock()
			if fail {
				_ = tp.PrintfLine("451 4.3.0 mailbox temporarily unavailable")
				continue
			}
			cur.To = append(cur.To, strings.Trim(arg[3:], "<>"))
			_ = tp.PrintfLine("250 2.1.5 ok")
		case "DATA":
			_ = tp.PrintfLine("354 go ahead")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			cur.Data = data
			f.mu.Lock()
			f.mails = append(f.mails, cur)
			f.mu.Unlock()
			_ = tp.PrintfLine("250 2.0.0 queued")
		case "RSET", "NOOP":
			_ = tp.PrintfLine("250 ok")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("502 5.5.2 unknown command")
		}
	}
}
