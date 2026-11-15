package email

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Offline tests: nothing here opens a connection to a real mail provider.

func baseEnv(provider string, extra map[string]string) map[string]string {
	env := map[string]string{
		"EMAIL_PROVIDER": provider, "EMAIL_FROM": "Mail <sender@example.com>",
		"SMTP_HOST": "smtp.example.com", "RESEND_API_KEY": "private-resend-key",
		"AWS_SES_REGION": "us-east-1", "ALIYUN_ACCESS_KEY_ID": "private-access-id",
		"ALIYUN_ACCESS_KEY_SECRET": "private-access-secret",
	}
	for key, value := range extra {
		env[key] = value
	}
	return env
}

func mustNew(t *testing.T, env map[string]string) service {
	t.Helper()
	sender, err := New(env)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc, ok := sender.(service)
	if !ok {
		t.Fatalf("New returned %T", sender)
	}
	return svc
}

// stubTransport records round trips and answers from respond (call number from 1).
type stubTransport struct {
	mu      sync.Mutex
	calls   int
	reqs    []*http.Request
	bodies  []string
	respond func(call int, r *http.Request) (*http.Response, error)
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.reqs = append(s.reqs, r)
	s.bodies = append(s.bodies, string(body))
	s.mu.Unlock()
	return s.respond(call, r)
}

func (s *stubTransport) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func reply(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status),
		Header: http.Header{"Content-Type": {contentType}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

func jsonReply(status int, body string) func(int, *http.Request) (*http.Response, error) {
	return func(int, *http.Request) (*http.Response, error) { return reply(status, "application/json", body), nil }
}

func assertDelivery(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrDelivery) || err.Error() != "email delivery failed" {
		t.Fatalf("want exactly ErrDelivery, got %v", err)
	}
}

type recordingTransport struct {
	calls int
	err   error
}

func (r *recordingTransport) deliver(context.Context, message) error {
	r.calls++
	return r.err
}

func TestDisabledAndLegacySMTPSelection(t *testing.T) {
	if s, err := New(nil); s != nil || err != nil {
		t.Fatalf("unconfigured must be (nil, nil), got %v, %v", s, err)
	}
	if s, err := New(map[string]string{"EMAIL_PROVIDER": "  ", "EMAIL_FROM": "a@example.com"}); s != nil || err != nil {
		t.Fatalf("blank provider without SMTP_HOST must be disabled, got %v, %v", s, err)
	}
	legacy := map[string]string{"SMTP_HOST": "smtp.example.com", "SMTP_FROM": "Original <sender@example.com>"}
	svc := mustNew(t, legacy)
	smtpT, ok := svc.t.(*smtpTransport)
	if !ok || smtpT.id.raw != "Original <sender@example.com>" || smtpT.security != "starttls" || smtpT.port != 587 {
		t.Fatalf("legacy SMTP fallback not selected: %#v", svc.t)
	}
	legacy["EMAIL_FROM"] = "New <new@example.com>"
	if got := mustNew(t, legacy).t.(*smtpTransport).id.raw; got != "New <new@example.com>" {
		t.Fatalf("EMAIL_FROM must win over SMTP_FROM, got %q", got)
	}
	// SMTP_FROM only applies to SMTP.
	if _, err := New(map[string]string{"EMAIL_PROVIDER": "resend", "SMTP_FROM": "a@example.com", "RESEND_API_KEY": "k"}); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("SMTP_FROM must not serve resend, got %v", err)
	}
	// Explicit none disables even with old credentials around.
	legacy["EMAIL_PROVIDER"] = "none"
	if s, err := New(legacy); s != nil || err != nil {
		t.Fatalf("EMAIL_PROVIDER=none must disable, got %v, %v", s, err)
	}
	legacy["EMAIL_PROVIDER"] = " NONE "
	if s, err := New(legacy); s != nil || err != nil {
		t.Fatalf("provider name is trimmed and case-insensitive, got %v, %v", s, err)
	}
}

func TestInvalidConfigurationFailsClosedWithoutFallback(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown provider":    {"EMAIL_PROVIDER": "typo"},
		"module path":         {"EMAIL_PROVIDER": "module.Class"},
		"no sender":           {"EMAIL_FROM": ""},
		"sender list":         {"EMAIL_FROM": "a@example.com,b@example.com"},
		"sender injection":    {"EMAIL_FROM": "sender@example.com\r\nBcc: extra@example.com"},
		"sender no tld":       {"EMAIL_FROM": "sender@localhost"},
		"smtp security":       {"EMAIL_PROVIDER": "smtp", "SMTP_SECURITY": "none"},
		"smtp security blank": {"EMAIL_PROVIDER": "smtp", "SMTP_SECURITY": "plain"},
		"smtp port text":      {"EMAIL_PROVIDER": "smtp", "SMTP_PORT": "invalid"},
		"smtp port zero":      {"EMAIL_PROVIDER": "smtp", "SMTP_PORT": "0"},
		"smtp port high":      {"EMAIL_PROVIDER": "smtp", "SMTP_PORT": "65536"},
		"smtp no host":        {"EMAIL_PROVIDER": "smtp", "SMTP_HOST": ""},
		"smtp password only":  {"EMAIL_PROVIDER": "smtp", "SMTP_PASSWORD": "secret", "SMTP_USERNAME": ""},
		"ses no region":       {"EMAIL_PROVIDER": "ses", "AWS_SES_REGION": ""},
		"ses url region":      {"EMAIL_PROVIDER": "ses", "AWS_SES_REGION": "https://evil.example"},
		"ses half keys":       {"EMAIL_PROVIDER": "ses", "AWS_ACCESS_KEY_ID": "incomplete"},
		"ses token only":      {"EMAIL_PROVIDER": "ses", "AWS_SESSION_TOKEN": "incomplete"},
		"aliyun region":       {"EMAIL_PROVIDER": "aliyun", "ALIYUN_DM_REGION": "evil.example"},
		"aliyun bad sender":   {"EMAIL_PROVIDER": "aliyun", "EMAIL_FROM": "SixteenCharAlias <sender@example.com>!"},
		"aliyun long alias":   {"EMAIL_PROVIDER": "aliyun", "EMAIL_FROM": "Subtitle Translator <sender@example.com>"},
		"aliyun no secret":    {"EMAIL_PROVIDER": "aliyun", "ALIYUN_ACCESS_KEY_SECRET": ""},
		"aliyun no key id":    {"EMAIL_PROVIDER": "aliyun", "ALIYUN_ACCESS_KEY_ID": " "},
		"resend no key":       {"RESEND_API_KEY": "", "EMAIL_PROVIDER": "resend"},
	}
	for name, changes := range cases {
		t.Run(name, func(t *testing.T) {
			provider := changes["EMAIL_PROVIDER"]
			if provider == "" {
				provider = "resend"
			}
			s, err := New(baseEnv(provider, changes))
			if s != nil || !errors.Is(err, ErrConfiguration) {
				t.Fatalf("want (nil, ErrConfiguration), got (%v, %v)", s, err)
			}
			// Configuration errors name settings but never echo their values.
			for _, secret := range []string{"private", "evil.example", "Bcc", "incomplete"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("configuration error leaks %q: %v", secret, err)
				}
			}
		})
	}
}

func TestValidConfigurationsAndSESRegionFallback(t *testing.T) {
	for _, provider := range []string{"smtp", "ses", "aliyun", "resend"} {
		mustNew(t, baseEnv(provider, nil))
	}
	// AWS_SES_REGION, then AWS_REGION, then AWS_DEFAULT_REGION.
	env := map[string]string{"EMAIL_PROVIDER": "ses", "EMAIL_FROM": "a@example.com",
		"AWS_REGION": "eu-west-2", "AWS_DEFAULT_REGION": "ap-south-1"}
	if got := mustNew(t, env).t.(*sesTransport).region; got != "eu-west-2" {
		t.Fatalf("region %q", got)
	}
	delete(env, "AWS_REGION")
	if got := mustNew(t, env).t.(*sesTransport).region; got != "ap-south-1" {
		t.Fatalf("region %q", got)
	}
	// A bare address needs no display name, and aliyun then sends no FromAlias.
	if a := mustNew(t, baseEnv("aliyun", map[string]string{"EMAIL_FROM": "sender@example.com"})).t.(*aliyunTransport); a.id.name != "" {
		t.Fatalf("unexpected alias %q", a.id.name)
	}
	// Exactly 15 characters is allowed, counted in characters, not bytes.
	mustNew(t, baseEnv("aliyun", map[string]string{"EMAIL_FROM": "訂閱字幕翻譯服務通知中心測試 <sender@example.com>"}))
}

func TestFromEnv(t *testing.T) {
	for _, name := range []string{"EMAIL_PROVIDER", "EMAIL_FROM", "SMTP_HOST", "SMTP_FROM", "RESEND_API_KEY"} {
		t.Setenv(name, "")
	}
	if s, err := FromEnv(); s != nil || err != nil {
		t.Fatalf("empty environment must disable mail, got %v, %v", s, err)
	}
	t.Setenv("EMAIL_PROVIDER", "resend")
	t.Setenv("EMAIL_FROM", "Mail <sender@example.com>")
	if _, err := FromEnv(); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("missing key must be a configuration error, got %v", err)
	}
	t.Setenv("RESEND_API_KEY", "private-key")
	s, err := FromEnv()
	if err != nil || s == nil {
		t.Fatalf("FromEnv: %v, %v", s, err)
	}
	if _, ok := s.(service).t.(*resendTransport); !ok {
		t.Fatalf("wrong transport %T", s.(service).t)
	}
}

func TestValidAddress(t *testing.T) {
	cases := []struct {
		address string
		valid   bool
	}{
		{"user@example.com", true}, {"first.last+tag@sub.example.org", true},
		{"User <user@example.com>", false}, {"user@example", false}, {"", false},
		{strings.Repeat("a", 65) + "@example.com", false},
		{strings.Repeat("a", 64) + "@example.com", true},
		{"user@" + strings.Repeat("a", 250) + ".com", false},
		{"user@example.com\n", false}, {"a@example.com,b@example.com", false},
		{" user@example.com", false}, {"user@exa mple.com", false},
		{"user@-example.com", false}, {"user@example.c", false},
		{"üser@example.com", false}, {"user@例え.jp", false},
		{"user@example.com (comment)", false}, {"@example.com", false},
	}
	for _, c := range cases {
		if got := ValidAddress(c.address); got != c.valid {
			t.Errorf("ValidAddress(%q) = %v, want %v", c.address, got, c.valid)
		}
	}
}

func TestInvalidMessagesNeverReachTheTransport(t *testing.T) {
	cases := []struct{ name, to, subject, text, html string }{
		{"list", "a@example.com,b@example.com", "Subject", "Text", ""},
		{"header injection", "a@example.com\r\nBcc: b@example.com", "Subject", "Text", ""},
		{"display name", "A <a@example.com>", "Subject", "Text", ""},
		{"empty recipient", "", "Subject", "Text", ""},
		{"subject newline", "user@example.com", "Subject\nBcc: a@example.com", "Text", ""},
		{"subject DEL", "user@example.com", "Sub\x7fject", "Text", ""},
		{"empty subject", "user@example.com", "", "Text", ""},
		{"blank subject", "user@example.com", "  ", "Text", ""},
		{"long subject", "user@example.com", strings.Repeat("x", 257), "Text", ""},
		{"long CJK subject", "user@example.com", strings.Repeat("字", 257), "Text", ""},
		{"empty text", "user@example.com", "Subject", "", ""},
		{"blank text", "user@example.com", "Subject", " \n", ""},
		{"bad utf-8 subject", "user@example.com", "Sub\xffject", "Text", ""},
		{"bad utf-8 text", "user@example.com", "Subject", "Te\xffxt", ""},
		{"bad utf-8 html", "user@example.com", "Subject", "Text", "<p>\xff</p>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recordingTransport{}
			err := service{t: rec}.Send(context.Background(), c.to, c.subject, c.text, c.html)
			if !errors.Is(err, ErrInvalidMessage) || errors.Is(err, ErrDelivery) {
				t.Fatalf("want ErrInvalidMessage, got %v", err)
			}
			if rec.calls != 0 {
				t.Fatal("transport was reached")
			}
		})
	}
	// Boundaries that are accepted.
	rec := &recordingTransport{}
	if err := (service{t: rec}).Send(context.Background(), "user@example.com", strings.Repeat("字", 256), "Text", "<p>x</p>"); err != nil || rec.calls != 1 {
		t.Fatalf("256 characters must be accepted: %v (calls %d)", err, rec.calls)
	}
}

// Send must accept exactly the recipients ValidAddress accepts: callers validate
// with ValidAddress, and net/mail rejected some of them (dot placement) that the
// Python port delivered.
func TestSendAcceptsEveryValidAddress(t *testing.T) {
	for _, to := range []string{
		"user@example.com", "a..b@example.com", ".a@example.com", "a.@example.com",
		"a@b..example.com", "a{b}|c`d@example.com", "o'neil+tag@example.com",
	} {
		if !ValidAddress(to) {
			t.Fatalf("precondition: %q should be valid", to)
		}
		rec := &recordingTransport{}
		if err := (service{t: rec}).Send(context.Background(), to, "Subject", "Text", ""); err != nil || rec.calls != 1 {
			t.Errorf("Send(%q) = %v, calls %d", to, err, rec.calls)
		}
	}
	for _, to := range []string{"A <a@example.com>", "a@example.com ", "<a@example.com>", "a@example.com (c)"} {
		rec := &recordingTransport{}
		if err := (service{t: rec}).Send(context.Background(), to, "Subject", "Text", ""); !errors.Is(err, ErrInvalidMessage) || rec.calls != 0 {
			t.Errorf("Send(%q) = %v, calls %d", to, err, rec.calls)
		}
	}
}

func TestTransportErrorsAreHiddenAndCanceledContextSkipsTheTransport(t *testing.T) {
	rec := &recordingTransport{err: errors.New("private smtp password and code 123456")}
	err := service{t: rec}.Send(context.Background(), "user@example.com", "Subject", "private code", "")
	assertDelivery(t, err)
	if errors.Unwrap(err) != nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("cause leaked: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("calls = %d", rec.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec = &recordingTransport{}
	assertDelivery(t, service{t: rec}.Send(ctx, "user@example.com", "Subject", "Text", ""))
	if rec.calls != 0 {
		t.Fatal("canceled context reached the transport")
	}
}
