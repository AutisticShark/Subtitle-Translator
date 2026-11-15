package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestSecretsAreWriteOnlyAndEncrypted(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	result := admin.send(http.MethodPut, "/api/settings", map[string]any{"anthropic_api_key": " sk-ant-123 "})
	expectStatus(t, result, http.StatusOK)
	if strings.Contains(string(result.body), "sk-ant-123") {
		t.Fatal("secret echoed back")
	}
	if result.json()["configured"].(map[string]any)["anthropic"] != true {
		t.Fatal("configured flag missing")
	}
	stored := env.text("SELECT value FROM settings WHERE name = 'anthropic_api_key'")
	if !strings.HasPrefix(stored, secretPrefix) || strings.Contains(stored, "sk-ant") {
		t.Fatalf("stored %q", stored)
	}
	if plain, err := env.server.decryptSecret(stored); err != nil || plain != "sk-ant-123" {
		t.Fatalf("decrypt %q %v", plain, err)
	}
	// A blank secret keeps the saved value; DELETE removes it.
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{"anthropic_api_key": ""}), http.StatusOK)
	if env.text("SELECT value FROM settings WHERE name = 'anthropic_api_key'") != stored {
		t.Fatal("blank secret replaced the saved one")
	}
	result = admin.do(http.MethodDelete, "/api/settings/keys/anthropic", nil, "")
	expectStatus(t, result, http.StatusOK)
	if result.json()["configured"].(map[string]any)["anthropic"] != false {
		t.Fatal("key not removed")
	}
	expectStatus(t, admin.do(http.MethodDelete, "/api/settings/keys/unknown", nil, ""), http.StatusNotFound)
	member := env.createUser(admin, "member", "user")
	view := member.get("/api/settings")
	expectStatus(t, view, http.StatusOK)
	for _, key := range secretKeys {
		if _, ok := view.json()[key]; ok {
			t.Fatalf("settings expose %s", key)
		}
	}
}

func TestPlaintextSecretsAreEncryptedAtStartupAndUnstableKeysCannotSave(t *testing.T) {
	env := newEnv(t)
	env.setupAdmin()
	env.exec("INSERT INTO settings (name, value, updated_at) VALUES ('deepl_api_key', 'plain-key', 'x')")
	env.restart()
	stored := env.text("SELECT value FROM settings WHERE name = 'deepl_api_key'")
	if !strings.HasPrefix(stored, secretPrefix) {
		t.Fatalf("legacy plaintext kept: %q", stored)
	}
	unstable := newEnv(t, func(c *Config) { c.JWTSecret = "" })
	adminUnstable := unstable.setupAdmin()
	expectStatus(t, adminUnstable.send(http.MethodPut, "/api/settings", map[string]any{"google_api_key": "x"}),
		http.StatusServiceUnavailable)
	expectStatus(t, adminUnstable.send(http.MethodPut, "/api/settings", map[string]any{"rpm": "5"}), http.StatusOK)
}

func TestSettingsValidation(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	bad := []map[string]any{
		{"nope": "1"},
		{"batch_size": "4.5"},
		{"batch_size": "0"},
		{"workers": true},
		{"rpm": nil},
		{"width": []any{1}},
		{"width": "3"},
		{"rpm": "nan"},
		{"registration_enabled": "yes"},
		{"captcha_provider": "other"},
		{"captcha_hostname": "https://example.com"},
		{"default_provider": "bogus"},
		{"user_daily_job_limit": "100001"},
	}
	for _, payload := range bad {
		result := admin.send(http.MethodPut, "/api/settings", payload)
		if result.status != http.StatusBadRequest {
			t.Errorf("%v accepted: %d %s", payload, result.status, result.body)
		}
	}
	result := admin.send(http.MethodPut, "/api/settings", map[string]any{
		"batch_size": json.Number("12"), "width": json.Number("16.0"), "rpm": " 30 ",
	})
	expectStatus(t, result, http.StatusOK)
	values := result.json()
	if values["batch_size"] != "12" || values["width"] != "16.0" || values["rpm"] != "30" {
		t.Fatalf("stored text %v %v %v", values["batch_size"], values["width"], values["rpm"])
	}
}

func TestChangingTheWindowResetsWindowBucketsButKeepsCalendarUsage(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{"panel_job_limit": "50",
		"panel_daily_job_limit": "50"}), http.StatusOK)
	id := jobIDs(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}))[0]
	env.waitForJob(admin, id)
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{"rate_limit_window_minutes": "30"}),
		http.StatusOK)
	if env.scalar("SELECT COUNT(*) FROM rate_limit_buckets WHERE scope = 'panel'") != 0 {
		t.Fatal("window bucket kept")
	}
	if env.scalar("SELECT used FROM rate_limit_buckets WHERE scope = 'panel:daily'") != 1 {
		t.Fatal("calendar usage lost")
	}
}

// captchaTransport answers Siteverify requests with a scripted body.
type captchaTransport struct {
	mu       sync.Mutex
	body     string
	status   int
	err      error
	requests []url.Values
	hosts    []string
}

func (c *captchaTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, _ := io.ReadAll(request.Body)
	form, _ := url.ParseQuery(string(data))
	c.requests = append(c.requests, form)
	c.hosts = append(c.hosts, request.URL.String())
	if c.err != nil {
		return nil, c.err
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(c.body)),
		Header: http.Header{}, Request: request}, nil
}

func TestCaptchaIsVerifiedServerSideAndFailsClosed(t *testing.T) {
	transport := &captchaTransport{body: `{"success":true,"hostname":"subs.example","action":"login"}`}
	env := newEnv(t, func(c *Config) { c.CaptchaClient = &http.Client{Transport: transport} })
	admin := env.setupAdmin()
	// Enabling a provider needs its keys and an expected hostname.
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{"captcha_provider": "turnstile"}),
		http.StatusBadRequest)
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{
		"captcha_provider": "turnstile", "turnstile_site_key": "site", "turnstile_secret_key": "secret",
	}), http.StatusBadRequest)
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{
		"captcha_provider": "turnstile", "turnstile_site_key": "site", "turnstile_secret_key": "secret",
		"captcha_hostname": "Subs.Example.", "captcha_on_upload": "0",
	}), http.StatusOK)
	status := env.client().get("/api/auth/setup-status").json()["captcha"].(map[string]any)
	if status["provider"] != "turnstile" || status["site_key"] != "site" ||
		len(status["protected_actions"].([]any)) != 2 {
		t.Fatalf("public captcha config %v", status)
	}
	expectStatus(t, admin.do(http.MethodDelete, "/api/settings/keys/captcha-turnstile", nil, ""), http.StatusConflict)

	login := func(token any, headers map[string]string) response {
		c := env.client()
		for key, value := range headers {
			c.header[key] = value
		}
		return c.post("/api/auth/login", map[string]any{"username": "admin", "password": testPassword,
			"captcha_token": token})
	}
	expectStatus(t, login(nil, nil), http.StatusBadRequest)
	expectStatus(t, login("token", map[string]string{"Host": "evil.example", "X-Forwarded-Host": "evil.example"}),
		http.StatusOK)
	transport.mu.Lock()
	last := transport.requests[len(transport.requests)-1]
	host := transport.hosts[len(transport.hosts)-1]
	transport.mu.Unlock()
	if last.Get("secret") != "secret" || last.Get("response") != "token" || last.Get("remoteip") == "" ||
		host != captchaVerifyURLs["turnstile"] {
		t.Fatalf("verification request %v to %s", last, host)
	}
	for _, body := range []string{
		`{"success":true,"hostname":"evil.example","action":"login"}`,
		`{"success":true,"hostname":"subs.example","action":"register"}`,
		`{"success":false}`, `{"success":"true","hostname":"subs.example","action":"login"}`,
	} {
		transport.mu.Lock()
		transport.body = body
		transport.mu.Unlock()
		expectStatus(t, login("token", nil), http.StatusBadRequest)
	}
	for _, item := range []struct {
		body   string
		status int
		err    error
	}{{"not json", 200, nil}, {`{"success":true}`, 500, nil}, {"", 0, errors.New("network down")}} {
		transport.mu.Lock()
		transport.body, transport.status, transport.err = item.body, item.status, item.err
		transport.mu.Unlock()
		result := login("token", nil)
		expectStatus(t, result, http.StatusServiceUnavailable)
	}
	// Uploads are not protected in this configuration.
	transport.mu.Lock()
	transport.err = errors.New("down")
	transport.mu.Unlock()
	expectStatus(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}), http.StatusAccepted)
}
