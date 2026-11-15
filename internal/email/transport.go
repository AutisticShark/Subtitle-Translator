package email

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	// timeout bounds each connection attempt and each HTTP exchange, like the
	// 10 second socket timeouts of the Python adapters.
	timeout = 10 * time.Second
	// overallTimeout bounds one whole Send, including SMTP's several round trips
	// and credential lookups, whatever the caller's context allows.
	overallTimeout = 30 * time.Second
	// maxResponseBytes caps provider responses that are read into memory.
	maxResponseBytes = 64 * 1024
)

var errRejected = errors.New("provider rejected the message")

// newHTTPClient returns a client that never follows redirects, so credentials and
// verification codes are never forwarded to another endpoint. A nil transport
// selects a TLS 1.2+ clone of the default transport.
func newHTTPClient(rt http.RoundTripper) *http.Client {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		rt = t
	}
	return &http.Client{
		Transport: rt,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// postJSON posts body once and decodes a small JSON object response. Every
// non-2xx status (redirects included, as they are not followed), oversized,
// malformed or non-object response is an error.
func postJSON(ctx context.Context, client *http.Client, url string, body []byte, headers map[string]string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Subtitle-Translator")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, errRejected
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponseBytes {
		return nil, errRejected
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil || result == nil {
		return nil, errRejected
	}
	return result, nil
}

// truthy mirrors Python truthiness for decoded JSON values, which the provider
// success checks were written against.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("email: no randomness available: " + err.Error())
	}
	return hex.EncodeToString(b)
}
