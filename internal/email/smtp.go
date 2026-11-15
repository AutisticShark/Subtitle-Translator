package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// smtpTransport submits one message per connection over TLS only: STARTTLS (port
// 587 by default) or implicit TLS (port 465). Plaintext SMTP does not exist, and
// certificates and host names are always verified. There is no retry.
type smtpTransport struct {
	id       identity
	host     string
	port     int
	security string // "starttls" or "ssl"
	username string
	password string

	rootCAs *x509.CertPool // nil uses the system roots; tests inject a private CA
	now     func() time.Time
}

func newSMTP(env map[string]string, id identity) (transport, error) {
	host, err := required(env, "SMTP_HOST")
	if err != nil {
		return nil, err
	}
	security := strings.ToLower(env["SMTP_SECURITY"])
	if security == "" {
		security = "starttls"
	}
	if security != "starttls" && security != "ssl" {
		return nil, configError("SMTP_SECURITY must be starttls or ssl")
	}
	port := 587
	if security == "ssl" {
		port = 465
	}
	if raw := env["SMTP_PORT"]; raw != "" {
		port, err = strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, configError("SMTP_PORT must be between 1 and 65535")
		}
	}
	if port < 1 || port > 65535 {
		return nil, configError("SMTP_PORT must be between 1 and 65535")
	}
	username, password := env["SMTP_USERNAME"], env["SMTP_PASSWORD"]
	if password != "" && username == "" {
		return nil, configError("SMTP_USERNAME is required with SMTP_PASSWORD")
	}
	return &smtpTransport{
		id: id, host: host, port: port, security: security,
		username: username, password: password, now: time.Now,
	}, nil
}

func (s *smtpTransport) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12, RootCAs: s.rootCAs}
}

func (s *smtpTransport) deliver(ctx context.Context, m message) error {
	raw, err := s.buildMessage(m)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	if s.security == "ssl" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: s.tlsConfig()}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	// Close the socket when the context ends so blocked reads and writes return.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer client.Close()

	if s.security == "starttls" {
		// Never authenticate or send in the clear: a server that does not offer
		// STARTTLS, or whose certificate fails verification, ends the attempt.
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("server does not offer STARTTLS")
		}
		if err := client.StartTLS(s.tlsConfig()); err != nil {
			return err
		}
	}
	if s.username != "" {
		auth, err := s.authenticator(client)
		if err != nil {
			return err
		}
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(s.id.address); err != nil {
		return err
	}
	if err := client.Rcpt(m.to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	// The server has accepted the message. A failed QUIT from here on (a 421
	// reply or a dropped connection) must not be reported as a failure, which
	// would invite a duplicate send. The deferred Close always releases the socket.
	_ = client.Quit()
	return nil
}

func (s *smtpTransport) authenticator(client *smtp.Client) (smtp.Auth, error) {
	_, advertised := client.Extension("AUTH")
	mechanisms := " " + strings.ToUpper(advertised) + " "
	switch {
	case strings.Contains(mechanisms, " PLAIN "):
		return smtp.PlainAuth("", s.username, s.password, s.host), nil
	case strings.Contains(mechanisms, " LOGIN "):
		return &loginAuth{username: s.username, password: s.password}, nil
	case strings.Contains(mechanisms, " CRAM-MD5 "):
		return smtp.CRAMMD5Auth(s.username, s.password), nil
	}
	return nil, errors.New("server offers no supported authentication mechanism")
}

// loginAuth implements the widely deployed AUTH LOGIN mechanism. It is only used
// after TLS is established. Like Python's smtplib it ignores the prompt text,
// which servers word differently ("Username:", "User Name", empty): the first
// challenge gets the username and every later one the password.
type loginAuth struct {
	username, password string
	challenges         int
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("refusing to send credentials without TLS")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.challenges++
	if a.challenges == 1 {
		return []byte(a.username), nil
	}
	return []byte(a.password), nil
}

// buildMessage renders an RFC 5322 message: UTF-8 quoted-printable text, or a
// multipart/alternative with an HTML part, with RFC 2047 encoded subject and
// display name when they are not plain ASCII.
func (s *smtpTransport) buildMessage(m message) ([]byte, error) {
	var buf bytes.Buffer
	header := func(name, value string) { fmt.Fprintf(&buf, "%s: %s\r\n", name, value) }
	header("From", s.id.headerValue())
	header("To", m.to)
	header("Subject", encodeHeader(m.subject))
	header("Date", s.now().UTC().Format(time.RFC1123Z))
	domain := s.id.address[strings.LastIndexByte(s.id.address, '@')+1:]
	header("Message-ID", "<"+randomHex(12)+"@"+domain+">")
	header("MIME-Version", "1.0")

	if m.html == "" {
		header("Content-Type", `text/plain; charset="utf-8"`)
		header("Content-Transfer-Encoding", "quoted-printable")
		buf.WriteString("\r\n")
		if err := writeQuotedPrintable(&buf, m.text); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	mw := multipart.NewWriter(&buf)
	header("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	buf.WriteString("\r\n")
	for _, part := range []struct{ kind, body string }{{"plain", m.text}, {"html", m.html}} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {`text/` + part.kind + `; charset="utf-8"`},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeQuotedPrintable(pw, part.body); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeQuotedPrintable(w interface{ Write([]byte) (int, error) }, text string) error {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
	if !strings.HasSuffix(text, "\r\n") {
		text += "\r\n"
	}
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(text)); err != nil {
		return err
	}
	return qp.Close()
}

// headerValue is the From header value: the configured text when it is ASCII,
// otherwise the display name as folded RFC 2047 encoded words.
func (id identity) headerValue() string { return id.encoded(headerFold) }

// apiValue is the same mailbox for APIs that take it as a single-line field (SES
// FromEmailAddress): adjacent encoded words are separated by a space, never by a
// line break.
func (id identity) apiValue() string { return id.encoded(" ") }

func (id identity) encoded(sep string) string {
	if isASCII(id.raw) {
		return id.raw
	}
	if id.name == "" {
		return id.address
	}
	return encodeWords(id.name, sep) + " <" + id.address + ">"
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// encodeHeader returns s unchanged when it is ASCII, otherwise a run of RFC 2047
// base64 encoded words, each within the 75 character limit and folded onto
// continuation lines, so long non-ASCII subjects never exceed the line limit.
func encodeHeader(s string) string { return encodeWords(s, headerFold) }

// headerFold separates encoded words in a header: a folding line break.
const headerFold = "\r\n "

func encodeWords(s, sep string) string {
	if isASCII(s) {
		return s
	}
	const maxChunk = 45 // 60 base64 characters + 12 of "=?utf-8?b?" and "?="
	var words []string
	start, size := 0, 0
	for i, r := range s {
		n := utf8.RuneLen(r)
		if size+n > maxChunk {
			words = append(words, s[start:i])
			start, size = i, 0
		}
		size += n
	}
	words = append(words, s[start:])
	for i, w := range words {
		words[i] = "=?utf-8?b?" + base64.StdEncoding.EncodeToString([]byte(w)) + "?="
	}
	return strings.Join(words, sep)
}
