// Package email sends single transactional messages through a deployment
// configured provider: SMTP, Amazon SES, Aliyun DirectMail or Resend.
//
// It is a port of the Python email_delivery module and keeps its contract:
//
//   - Every message has exactly one recipient, a subject, a plain text body and an
//     optional HTML body.
//   - A message is submitted once. Success means the provider accepted it, not that
//     it reached an inbox. Nothing here retries or switches provider, because a
//     timeout can happen after acceptance. Callers own authorization, send limits
//     and any explicit resend.
//   - Endpoints are fixed in code (SES uses the AWS SDK's own endpoint rules and
//     ignores configured endpoint URLs). Configuration never names a URL.
//   - Errors returned by Send never include provider responses, credentials,
//     addresses or message content. Delivery failures are always ErrDelivery.
package email

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Sender submits one message. Send blocks until the provider accepts or rejects it.
type Sender interface {
	Send(ctx context.Context, to, subject, text, html string) error
}

var (
	// ErrDelivery is returned for every provider, network or ambiguous failure.
	// Its text is the whole error: no cause is attached on purpose.
	ErrDelivery = errors.New("email delivery failed")
	// ErrConfiguration matches errors returned by New for missing or invalid settings.
	ErrConfiguration = errors.New("email configuration is invalid")
	// ErrInvalidMessage matches errors returned by Send for a message that was
	// rejected before any transport was contacted.
	ErrInvalidMessage = errors.New("email message is invalid")
)

// safeError carries a fixed, human-readable message that never embeds values read
// from the environment or the message, and matches one of the sentinel errors.
type safeError struct {
	kind error
	msg  string
}

func (e *safeError) Error() string        { return e.msg }
func (e *safeError) Is(target error) bool { return target == e.kind }

func configError(format string, args ...any) error {
	return &safeError{kind: ErrConfiguration, msg: fmt.Sprintf(format, args...)}
}

func messageError(msg string) error {
	return &safeError{kind: ErrInvalidMessage, msg: msg}
}

const maxSubjectRunes = 256

// message is a validated outbound message.
type message struct {
	to, subject, text, html string
}

// transport performs one submission. Its errors are discarded by service.Send, so
// they may carry detail useful in a debugger but are never shown to callers.
type transport interface {
	deliver(ctx context.Context, m message) error
}

// service validates messages and hides transport errors behind ErrDelivery.
type service struct{ t transport }

func (s service) Send(ctx context.Context, to, subject, text, html string) error {
	if err := validateMessage(to, subject, text, html); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return ErrDelivery
	}
	ctx, cancel := context.WithTimeout(ctx, overallTimeout)
	defer cancel()
	if err := s.t.deliver(ctx, message{to: to, subject: subject, text: text, html: html}); err != nil {
		return ErrDelivery
	}
	return nil
}

func validateMessage(to, subject, text, html string) error {
	// ValidAddress alone decides: it is the check callers use when accepting an
	// address, and net/mail rejects some addresses it (and Python) accept, such as
	// "a..b@example.com". Its ASCII-only pattern excludes controls and separators.
	if !ValidAddress(to) {
		return messageError("Enter a valid single email address")
	}
	if strings.TrimSpace(subject) == "" || utf8.RuneCountInString(subject) > maxSubjectRunes ||
		!utf8.ValidString(subject) || hasControl(subject) {
		return messageError("Email subject must be 1 to 256 characters without control characters")
	}
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || !utf8.ValidString(html) {
		return messageError("Email requires a valid UTF-8 plain text body and optional HTML body")
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 32 || r == 127 {
			return true
		}
	}
	return false
}

// addressPattern mirrors the Python validator: one bare address with an ASCII
// local part and a domain that has a 2 to 63 letter top level label.
var addressPattern = regexp.MustCompile(
	`^[a-zA-Z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?\.[a-zA-Z]{2,63}$`)

// ValidAddress reports whether value is one bare address (no display name, no
// list, no comment) that every supported provider accepts.
func ValidAddress(value string) bool {
	local, _, _ := strings.Cut(value, "@")
	return len(value) <= 254 && len(local) <= 64 && addressPattern.MatchString(value)
}

// parseMailbox parses the configured sender: one address with an optional display
// name. It rejects lists and anything that could inject headers before a provider
// sees the value.
func parseMailbox(value string) (name, address string, err error) {
	bad := configError("Enter a valid single email address")
	if value == "" || !utf8.ValidString(value) || hasControl(value) {
		return "", "", bad
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || !ValidAddress(parsed.Address) {
		return "", "", bad
	}
	return parsed.Name, parsed.Address, nil
}

// identity is the configured From mailbox.
type identity struct {
	raw     string // as configured, used verbatim where the provider takes UTF-8
	name    string
	address string
}

// FromEnv builds a Sender from the process environment. See New.
func FromEnv() (Sender, error) {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok && key != "" {
			env[key] = value
		}
	}
	return New(env)
}

// New validates the configuration in env and returns the selected Sender. It
// neither sends mail nor probes provider credentials.
//
// It returns (nil, nil) when mail is disabled: EMAIL_PROVIDER=none (even with old
// SMTP settings left behind), or no EMAIL_PROVIDER and no SMTP_HOST. A blank
// EMAIL_PROVIDER with SMTP_HOST set selects SMTP, which keeps SMTP-only
// deployments working. An unknown provider or incomplete settings return an error
// that matches ErrConfiguration; there is never a fallback to another provider.
func New(env map[string]string) (Sender, error) {
	name := strings.ToLower(strings.TrimSpace(env["EMAIL_PROVIDER"]))
	if name == "" {
		name = "none"
		if env["SMTP_HOST"] != "" {
			name = "smtp"
		}
	}
	switch name {
	case "none":
		return nil, nil
	case "smtp", "ses", "aliyun", "resend":
	default:
		return nil, configError("Unknown EMAIL_PROVIDER")
	}
	from := strings.TrimSpace(env["EMAIL_FROM"])
	if from == "" && name == "smtp" {
		from = strings.TrimSpace(env["SMTP_FROM"])
	}
	if from == "" {
		return nil, configError("EMAIL_FROM is required (SMTP also accepts SMTP_FROM)")
	}
	displayName, address, err := parseMailbox(from)
	if err != nil {
		return nil, err
	}
	id := identity{raw: from, name: displayName, address: address}

	var t transport
	switch name {
	case "smtp":
		t, err = newSMTP(env, id)
	case "ses":
		t, err = newSES(env, id)
	case "aliyun":
		t, err = newAliyun(env, id)
	default:
		t, err = newResend(env, id)
	}
	if err != nil {
		return nil, err
	}
	return service{t: t}, nil
}

func required(env map[string]string, name string) (string, error) {
	value := strings.TrimSpace(env[name])
	if value == "" {
		return "", configError("%s is required", name)
	}
	return value, nil
}
