package email

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a small in-process SMTP server speaking TLS with a throwaway
// certificate for 127.0.0.1.
type fakeSMTPConfig struct {
	offerSTARTTLS bool
	authMechs     string    // advertised after TLS, e.g. "PLAIN LOGIN"
	authReply     string    // reply to a finished AUTH, default "235 ok"
	dataReply     string    // reply after the message, default "250 queued"
	quit          string    // "221 bye" (default), "421 closing" or "drop"
	hang          bool      // accept the connection and never speak
	loginPrompts  [2]string // AUTH LOGIN challenges, default "Username:" and "Password:"
}

type fakeSMTP struct {
	t        *testing.T
	ln       net.Listener
	cert     tls.Certificate
	pool     *x509.CertPool
	implicit bool

	fakeSMTPConfig

	mu        sync.Mutex
	conns     int
	commands  []string
	tlsAtAuth []bool
	tlsAtMail []bool
	creds     []string
	data      string
	dataCount int
}

func newFakeSMTP(t *testing.T, implicit bool) *fakeSMTP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtp test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{
		t: t, ln: ln, pool: pool, implicit: implicit,
		fakeSMTPConfig: fakeSMTPConfig{offerSTARTTLS: true, authMechs: "PLAIN LOGIN"},
		cert:           tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
	}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns++
		f.mu.Unlock()
		go f.handle(conn)
	}
}

func (f *fakeSMTP) handle(raw net.Conn) {
	defer raw.Close()
	cfg := f.config()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	if cfg.hang {
		time.Sleep(5 * time.Second)
		return
	}
	var conn net.Conn = raw
	isTLS := false
	if f.implicit {
		conn = tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{f.cert}})
		if err := conn.(*tls.Conn).Handshake(); err != nil {
			return
		}
		isTLS = true
	}
	r := textproto.NewReader(bufio.NewReader(conn))
	say := func(lines ...string) {
		for _, line := range lines {
			_, _ = io.WriteString(conn, line+"\r\n")
		}
	}
	say("220 fake ESMTP")
	for {
		line, err := r.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		f.mu.Lock()
		f.commands = append(f.commands, verb)
		f.mu.Unlock()
		switch verb {
		case "EHLO":
			lines := []string{"250-fake"}
			if isTLS && cfg.authMechs != "" {
				lines = append(lines, "250-AUTH "+cfg.authMechs)
			}
			if !isTLS && cfg.offerSTARTTLS && !f.implicit {
				lines = append(lines, "250-STARTTLS")
			}
			lines = append(lines, "250 8BITMIME")
			say(lines...)
		case "STARTTLS":
			say("220 go ahead")
			conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			if err := conn.(*tls.Conn).Handshake(); err != nil {
				return
			}
			isTLS = true
			r = textproto.NewReader(bufio.NewReader(conn))
		case "AUTH":
			f.mu.Lock()
			f.tlsAtAuth = append(f.tlsAtAuth, isTLS)
			f.mu.Unlock()
			fields := strings.Fields(line)
			var user, pass string
			switch strings.ToUpper(fields[1]) {
			case "PLAIN":
				decoded, _ := base64.StdEncoding.DecodeString(fields[2])
				parts := strings.Split(string(decoded), "\x00")
				user, pass = parts[1], parts[2]
			case "LOGIN":
				prompts := cfg.loginPrompts
				if prompts == ([2]string{}) {
					prompts = [2]string{"Username:", "Password:"}
				}
				say("334 " + base64.StdEncoding.EncodeToString([]byte(prompts[0])))
				u, _ := r.ReadLine()
				say("334 " + base64.StdEncoding.EncodeToString([]byte(prompts[1])))
				p, _ := r.ReadLine()
				ub, _ := base64.StdEncoding.DecodeString(u)
				pb, _ := base64.StdEncoding.DecodeString(p)
				user, pass = string(ub), string(pb)
			}
			f.mu.Lock()
			f.creds = append(f.creds, fields[1]+":"+user+":"+pass)
			f.mu.Unlock()
			if cfg.authReply != "" {
				say(cfg.authReply)
			} else {
				say("235 ok")
			}
		case "MAIL":
			f.mu.Lock()
			f.tlsAtMail = append(f.tlsAtMail, isTLS)
			f.mu.Unlock()
			say("250 ok")
		case "RCPT":
			say("250 ok")
		case "DATA":
			say("354 go")
			body, err := io.ReadAll(r.DotReader())
			if err != nil {
				return
			}
			f.mu.Lock()
			f.data = string(body)
			f.dataCount++
			f.mu.Unlock()
			if cfg.dataReply != "" {
				say(cfg.dataReply)
			} else {
				say("250 queued")
			}
		case "QUIT":
			switch cfg.quit {
			case "drop":
			case "":
				say("221 bye")
			default:
				say(cfg.quit)
			}
			return
		default:
			say("502 unsupported")
		}
	}
}

// config returns a copy of the behavior switches; tests change them through set.
func (f *fakeSMTP) config() fakeSMTPConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fakeSMTPConfig
}

func (f *fakeSMTP) set(change func(c *fakeSMTPConfig)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.fakeSMTPConfig)
}

// recorded returns copies of what the server observed about authentication.
func (f *fakeSMTP) recorded() (creds []string, tlsAtAuth, tlsAtMail []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.creds...), append([]bool(nil), f.tlsAtAuth...), append([]bool(nil), f.tlsAtMail...)
}

func (f *fakeSMTP) snapshot() (conns int, commands []string, data string, dataCount int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns, append([]string(nil), f.commands...), f.data, f.dataCount
}

func (f *fakeSMTP) service(t *testing.T, extra map[string]string) service {
	t.Helper()
	security := "starttls"
	if f.implicit {
		security = "ssl"
	}
	env := baseEnv("smtp", map[string]string{
		"SMTP_HOST": "127.0.0.1", "SMTP_PORT": strconv.Itoa(f.port()), "SMTP_SECURITY": security,
	})
	for key, value := range extra {
		env[key] = value
	}
	svc := mustNew(t, env)
	svc.t.(*smtpTransport).rootCAs = f.pool
	return svc
}

func send(svc service, subject, text, html string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return svc.Send(ctx, "user@example.com", subject, text, html)
}

func TestSMTPTLSAuthenticationAndMultipart(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		name := map[bool]string{false: "starttls", true: "ssl"}[implicit]
		t.Run(name, func(t *testing.T) {
			f := newFakeSMTP(t, implicit)
			svc := f.service(t, map[string]string{"SMTP_USERNAME": "mailer", "SMTP_PASSWORD": "secret"})
			text := "plain text\n.leading dot line\nsecond line with 驗證 and a very long tail " + strings.Repeat("abcdefghij ", 20)
			if err := send(svc, "Verification 驗證", text, "<p>html 驗證</p>"); err != nil {
				t.Fatal(err)
			}
			conns, commands, data, count := f.snapshot()
			wantCommands := "EHLO STARTTLS EHLO AUTH MAIL RCPT DATA QUIT"
			if implicit {
				wantCommands = "EHLO AUTH MAIL RCPT DATA QUIT"
			}
			if conns != 1 || count != 1 || strings.Join(commands, " ") != wantCommands {
				t.Fatalf("conns %d, data %d, commands %v", conns, count, commands)
			}
			creds, tlsAtAuth, tlsAtMail := f.recorded()
			if len(tlsAtAuth) != 1 || !tlsAtAuth[0] || len(tlsAtMail) != 1 || !tlsAtMail[0] {
				t.Fatalf("credentials or mail outside TLS: auth %v mail %v", tlsAtAuth, tlsAtMail)
			}
			if len(creds) != 1 || creds[0] != "PLAIN:mailer:secret" {
				t.Fatalf("credentials %v", creds)
			}
			msg, err := mail.ReadMessage(strings.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if msg.Header.Get("From") != "Mail <sender@example.com>" || msg.Header.Get("To") != "user@example.com" {
				t.Fatalf("addresses %q -> %q", msg.Header.Get("From"), msg.Header.Get("To"))
			}
			subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
			if err != nil || subject != "Verification 驗證" {
				t.Fatalf("subject %q (%v)", subject, err)
			}
			if _, err := msg.Header.Date(); err != nil {
				t.Fatalf("Date: %v", err)
			}
			if id := msg.Header.Get("Message-Id"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
				t.Fatalf("Message-ID %q", id)
			}
			mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
			if err != nil || mediaType != "multipart/alternative" {
				t.Fatalf("content type %q (%v)", mediaType, err)
			}
			mr := multipart.NewReader(msg.Body, params["boundary"])
			var got []string
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				// multipart.Reader already decodes quoted-printable parts.
				body, err := io.ReadAll(part)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, part.Header.Get("Content-Type")+"|"+strings.ReplaceAll(string(body), "\r\n", "\n"))
			}
			want := []string{
				`text/plain; charset="utf-8"|` + text + "\n",
				`text/html; charset="utf-8"|<p>html 驗證</p>` + "\n",
			}
			if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("parts:\n got %q\nwant %q", got, want)
			}
		})
	}
}

func TestSMTPPlainTextOnlyAndNoAuth(t *testing.T) {
	f := newFakeSMTP(t, false)
	svc := f.service(t, nil) // no credentials: a TLS relay without authentication
	if err := send(svc, "Subject", "just text", ""); err != nil {
		t.Fatal(err)
	}
	_, commands, data, _ := f.snapshot()
	if strings.Contains(strings.Join(commands, " "), "AUTH") {
		t.Fatalf("authenticated without credentials: %v", commands)
	}
	msg, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	mediaType, _, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if mediaType != "text/plain" || strings.TrimSpace(string(body)) != "just text" {
		t.Fatalf("%s %q", mediaType, body)
	}
}

func TestSMTPLoginMechanismFallback(t *testing.T) {
	f := newFakeSMTP(t, false)
	f.set(func(c *fakeSMTPConfig) { c.authMechs = "LOGIN" })
	svc := f.service(t, map[string]string{"SMTP_USERNAME": "mailer", "SMTP_PASSWORD": "secret"})
	if err := send(svc, "Subject", "text", ""); err != nil {
		t.Fatal(err)
	}
	if creds, _, _ := f.recorded(); len(creds) != 1 || creds[0] != "LOGIN:mailer:secret" {
		t.Fatalf("credentials %v", creds)
	}
}

// Servers word the AUTH LOGIN prompts differently; like Python's smtplib the
// client answers by position, never by prompt text.
func TestSMTPLoginIgnoresThePromptText(t *testing.T) {
	for _, prompts := range [][2]string{{"User Name\x00", "Pass Word\x00"}, {"", ""}, {"Password:", "Username:"}} {
		f := newFakeSMTP(t, false)
		f.set(func(c *fakeSMTPConfig) { c.authMechs, c.loginPrompts = "LOGIN", prompts })
		svc := f.service(t, map[string]string{"SMTP_USERNAME": "mailer", "SMTP_PASSWORD": "secret"})
		if err := send(svc, "Subject", "text", ""); err != nil {
			t.Fatalf("prompts %q: %v", prompts, err)
		}
		if creds, _, _ := f.recorded(); len(creds) != 1 || creds[0] != "LOGIN:mailer:secret" {
			t.Fatalf("prompts %q: credentials %v", prompts, creds)
		}
	}
}

func TestSMTPTLSFailuresNeverAuthenticateOrSend(t *testing.T) {
	cases := map[string]func(f *fakeSMTP, svc service){
		"untrusted certificate": func(f *fakeSMTP, svc service) { svc.t.(*smtpTransport).rootCAs = nil },
		"no STARTTLS offered":   func(f *fakeSMTP, svc service) { f.set(func(c *fakeSMTPConfig) { c.offerSTARTTLS = false }) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeSMTP(t, false)
			svc := f.service(t, map[string]string{"SMTP_USERNAME": "mailer", "SMTP_PASSWORD": "private-password"})
			mutate(f, svc)
			assertDelivery(t, send(svc, "Subject", "private code", ""))
			conns, commands, _, count := f.snapshot()
			joined := strings.Join(commands, " ")
			if conns != 1 || count != 0 || strings.Contains(joined, "AUTH") || strings.Contains(joined, "MAIL") {
				t.Fatalf("conns %d, data %d, commands %v", conns, count, commands)
			}
		})
	}
	t.Run("untrusted certificate with implicit TLS", func(t *testing.T) {
		f := newFakeSMTP(t, true)
		svc := f.service(t, map[string]string{"SMTP_USERNAME": "mailer", "SMTP_PASSWORD": "private-password"})
		svc.t.(*smtpTransport).rootCAs = nil
		assertDelivery(t, send(svc, "Subject", "private code", ""))
		if _, commands, _, _ := f.snapshot(); len(commands) != 0 {
			t.Fatalf("commands after failed handshake: %v", commands)
		}
	})
	t.Run("wrong host name", func(t *testing.T) {
		f := newFakeSMTP(t, false)
		svc := f.service(t, map[string]string{"SMTP_HOST": "localhost"})
		if err := send(svc, "Subject", "Text", ""); err != nil {
			t.Fatalf("certificate covers localhost: %v", err) // control: the name is checked, not ignored
		}
		svc = f.service(t, nil)
		svc.t.(*smtpTransport).host = "127.0.0.2" // not in the certificate; may not be reachable either way
		assertDelivery(t, send(svc, "Subject", "Text", ""))
	})
}

func TestSMTPTLSConfigIsStrict(t *testing.T) {
	cfg := mustNew(t, baseEnv("smtp", map[string]string{"SMTP_HOST": "mail.example.com"})).t.(*smtpTransport).tlsConfig()
	if cfg.InsecureSkipVerify || cfg.ServerName != "mail.example.com" || cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("weak TLS config: %+v", cfg)
	}
}

func TestSMTPAuthenticationFailureStopsBeforeMail(t *testing.T) {
	f := newFakeSMTP(t, false)
	f.set(func(c *fakeSMTPConfig) { c.authReply = "535 bad credentials private" })
	svc := f.service(t, map[string]string{"SMTP_USERNAME": "mailer", "SMTP_PASSWORD": "private-password"})
	assertDelivery(t, send(svc, "Subject", "Text", ""))
	conns, commands, _, count := f.snapshot()
	if conns != 1 || count != 0 || strings.Contains(strings.Join(commands, " "), "MAIL") {
		t.Fatalf("conns %d, data %d, commands %v", conns, count, commands)
	}
}

func TestSMTPQuitFailureAfterAcceptanceIsSuccess(t *testing.T) {
	for _, quit := range []string{"421 closing", "drop", "500 what"} {
		t.Run(quit, func(t *testing.T) {
			f := newFakeSMTP(t, false)
			f.set(func(c *fakeSMTPConfig) { c.quit = quit })
			svc := f.service(t, map[string]string{"SMTP_USERNAME": "mailer", "SMTP_PASSWORD": "secret"})
			if err := send(svc, "Subject", "Body", ""); err != nil {
				t.Fatalf("accepted message reported as failure: %v", err)
			}
			conns, _, _, count := f.snapshot()
			if conns != 1 || count != 1 {
				t.Fatalf("conns %d, data %d", conns, count)
			}
		})
	}
}

func TestSMTPFailureBeforeAcceptanceIsReportedOnceWithoutRetry(t *testing.T) {
	f := newFakeSMTP(t, false)
	f.set(func(c *fakeSMTPConfig) { c.dataReply, c.quit = "451 try later private", "421 closing" })
	svc := f.service(t, map[string]string{"SMTP_USERNAME": "mailer", "SMTP_PASSWORD": "secret"})
	err := send(svc, "Subject", "Body", "")
	assertDelivery(t, err)
	if conns, _, _, count := f.snapshot(); conns != 1 || count != 1 {
		t.Fatalf("conns %d, data %d: the send must not be retried", conns, count)
	}
}

func TestSMTPConnectionAndContextFailures(t *testing.T) {
	// Nothing listening.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	svc := mustNew(t, baseEnv("smtp", map[string]string{"SMTP_HOST": "127.0.0.1", "SMTP_PORT": strconv.Itoa(port)}))
	assertDelivery(t, send(svc, "Subject", "Text", ""))

	// A server that never answers ends with the context, promptly.
	f := newFakeSMTP(t, false)
	f.set(func(c *fakeSMTPConfig) { c.hang = true })
	svc = f.service(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	assertDelivery(t, svc.Send(ctx, "user@example.com", "Subject", "Text", ""))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("blocked for %v", elapsed)
	}
}

func TestEncodeHeaderFoldsAndRoundTrips(t *testing.T) {
	if got := encodeHeader("Plain ASCII subject"); got != "Plain ASCII subject" {
		t.Fatalf("ASCII must pass through, got %q", got)
	}
	for _, subject := range []string{"驗證碼", strings.Repeat("驗證碼 ", 40), strings.Repeat("字", 256), "mixed é ASCII " + strings.Repeat("x", 100) + " 驗"} {
		encoded := encodeHeader(subject)
		for _, line := range strings.Split(encoded, "\r\n") {
			if len(line) > 76 {
				t.Fatalf("line of %d characters: %q", len(line), line)
			}
		}
		if strings.ContainsAny(strings.ReplaceAll(encoded, "\r\n ", ""), "\r\n") {
			t.Fatalf("bare line break in %q", encoded)
		}
		decoded, err := new(mime.WordDecoder).DecodeHeader(encoded)
		if err != nil || decoded != subject {
			t.Fatalf("round trip failed: %q -> %q (%v)", subject, decoded, err)
		}
	}
}

func TestFromHeaderEncodesNonASCIIDisplayName(t *testing.T) {
	id := identity{raw: "字幕翻譯 <sender@example.com>", name: "字幕翻譯", address: "sender@example.com"}
	value := id.headerValue()
	if !isASCII(value) || !strings.HasSuffix(value, " <sender@example.com>") {
		t.Fatalf("From header %q", value)
	}
	parsed, err := (&mail.AddressParser{WordDecoder: new(mime.WordDecoder)}).Parse(value)
	if err != nil || parsed.Name != "字幕翻譯" || parsed.Address != "sender@example.com" {
		t.Fatalf("parsed %+v (%v)", parsed, err)
	}
	if got := (identity{raw: "Mail <a@example.com>", name: "Mail", address: "a@example.com"}).headerValue(); got != "Mail <a@example.com>" {
		t.Fatalf("ASCII sender must be kept verbatim, got %q", got)
	}
}

func TestSMTPPortDefaults(t *testing.T) {
	for security, port := range map[string]int{"": 587, "starttls": 587, "ssl": 465, "SSL": 465} {
		got := mustNew(t, baseEnv("smtp", map[string]string{"SMTP_SECURITY": security})).t.(*smtpTransport)
		if got.port != port {
			t.Errorf("security %q: port %d, want %d", security, got.port, port)
		}
	}
	if got := mustNew(t, baseEnv("smtp", map[string]string{"SMTP_PORT": " 2525 "})).t.(*smtpTransport).port; got != 2525 {
		t.Errorf("explicit port %d", got)
	}
}
