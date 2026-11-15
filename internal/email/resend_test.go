package email

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newResendStub(t *testing.T, respond func(int, *http.Request) (*http.Response, error)) (service, *stubTransport) {
	t.Helper()
	svc := mustNew(t, baseEnv("resend", nil))
	stub := &stubTransport{respond: respond}
	svc.t.(*resendTransport).client = newHTTPClient(stub)
	return svc, stub
}

func TestResendWirePayload(t *testing.T) {
	svc, stub := newResendStub(t, jsonReply(200, `{"id":"accepted"}`))
	if err := svc.Send(context.Background(), "user@example.com", "Subject <&>", "Text", "<p>HTML & more</p>"); err != nil {
		t.Fatal(err)
	}
	req := stub.reqs[0]
	if req.URL.String() != "https://api.resend.com/emails" || req.Method != http.MethodPost {
		t.Fatalf("request %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer private-resend-key" {
		t.Fatalf("Authorization %q", got)
	}
	if req.Header.Get("Content-Type") != "application/json" || req.Header.Get("User-Agent") != "Subtitle-Translator" {
		t.Fatalf("headers %v", req.Header)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stub.bodies[0]), &payload); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"from": "Mail <sender@example.com>", "to": []any{"user@example.com"},
		"subject": "Subject <&>", "text": "Text", "html": "<p>HTML & more</p>",
	}
	if !reflect.DeepEqual(payload, want) {
		t.Fatalf("payload %v", payload)
	}
	// No HTML key without an HTML body.
	if err := svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stub.bodies[1], `"html"`) {
		t.Fatalf("unexpected html key: %s", stub.bodies[1])
	}
}

func TestResendRejectionsAndMalformedResponsesAreSanitized(t *testing.T) {
	big := `{"id":"ok","pad":"` + strings.Repeat("x", maxResponseBytes) + `"}`
	cases := map[string]string{
		"error with id":   `{"id":"ok","error":"secret"}`,
		"status code":     `{"id":"ok","statusCode":403}`,
		"missing id":      `{"message":"secret"}`,
		"empty id":        `{"id":""}`,
		"array":           `[]`,
		"null":            `null`,
		"invalid json":    `not json: private`,
		"trailing data":   `{"id":"ok"} private`,
		"oversized":       big,
		"empty body":      ``,
		"scalar":          `"private"`,
		"error no id":     `{"statusCode":422,"name":"validation_error","message":"private"}`,
		"truncated":       `{"id":"ok"`,
		"id false":        `{"id":false}`,
		"id null":         `{"id":null}`,
		"error-false-id0": `{"id":0}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			svc, stub := newResendStub(t, jsonReply(200, body))
			assertDelivery(t, svc.Send(context.Background(), "user@example.com", "Subject", "private code", ""))
			if stub.count() != 1 {
				t.Fatalf("calls = %d, want exactly one", stub.count())
			}
		})
	}
}

func TestResendStatusAndNetworkErrorsAreSanitizedWithoutRetry(t *testing.T) {
	for _, status := range []int{301, 302, 307, 308, 400, 401, 403, 422, 429, 500, 503} {
		svc, stub := newResendStub(t, jsonReply(status, `{"id":"looks-ok","message":"private key"}`))
		assertDelivery(t, svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""))
		if stub.count() != 1 {
			t.Fatalf("status %d: calls = %d", status, stub.count())
		}
	}
	svc, stub := newResendStub(t, func(int, *http.Request) (*http.Response, error) {
		return nil, &timeoutError{"dial tcp private-host: i/o timeout, Authorization: Bearer private-resend-key"}
	})
	err := svc.Send(context.Background(), "user@example.com", "Subject", "Text", "")
	assertDelivery(t, err)
	if stub.count() != 1 {
		t.Fatalf("calls = %d", stub.count())
	}
}

type timeoutError struct{ msg string }

func (e *timeoutError) Error() string   { return e.msg }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }

func TestResendNeverFollowsRedirects(t *testing.T) {
	var target atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.Add(1)
		_, _ = w.Write([]byte(`{"id":"accepted"}`))
	}))
	defer other.Close()
	var first atomic.Int32
	var forwarded atomic.Value
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.Add(1)
		forwarded.Store(r.Header.Get("Authorization"))
		w.Header().Set("Location", other.URL+"/emails")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	svc := mustNew(t, baseEnv("resend", nil))
	tr := svc.t.(*resendTransport)
	// Trust the test servers' certificates, as the real client trusts system roots.
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	pool.AddCert(other.Certificate())
	tr.client = newHTTPClient(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
	tr.endpoint = origin.URL + "/emails"
	assertDelivery(t, svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""))
	if first.Load() != 1 || target.Load() != 0 {
		t.Fatalf("origin hits %d, redirect target hits %d", first.Load(), target.Load())
	}
	if forwarded.Load() != "Bearer private-resend-key" {
		t.Fatalf("origin saw %v", forwarded.Load())
	}
}

func TestHTTPClientPolicy(t *testing.T) {
	client := newHTTPClient(nil)
	if client.Timeout != 10*time.Second {
		t.Fatalf("timeout %v", client.Timeout)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/", nil)
	for range 5 {
		if err := client.CheckRedirect(req, nil); err != http.ErrUseLastResponse {
			t.Fatalf("redirects must not be followed, got %v", err)
		}
	}
	cfg := client.Transport.(*http.Transport).TLSClientConfig
	if cfg == nil || cfg.MinVersion < tls.VersionTLS12 || cfg.InsecureSkipVerify {
		t.Fatalf("unsafe TLS config %+v", cfg)
	}
	if got := mustNew(t, baseEnv("resend", nil)).t.(*resendTransport).endpoint; got != "https://api.resend.com/emails" {
		t.Fatalf("endpoint %q", got)
	}
}
