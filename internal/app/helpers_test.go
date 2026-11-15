package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

const (
	testPassword  = "correct horse battery"
	testJWTSecret = "test-jwt-secret"
	testSubtitle  = "1\n00:00:01,000 --> 00:00:02,000\nHello there\n\n2\n00:00:03,000 --> 00:00:04,000\nGeneral Kenobi\n"
)

var testIndex = []byte(`<!doctype html>
<html lang="en" data-theme="system">
  <head>
    <meta charset="UTF-8" />
    <title>Subtitle Translator</title>
    <script type="module" crossorigin src="/assets/index-abc.js"></script>
  </head>
  <body>
    <div id="app"></div>
  </body>
</html>
`)

func testFrontend() fstest.MapFS {
	return fstest.MapFS{
		"index.html":          {Data: testIndex},
		"assets/index-abc.js": {Data: []byte("console.log('ok')")},
		"favicon.svg":         {Data: []byte("<svg/>")},
	}
}

func testLocales() fstest.MapFS {
	files := fstest.MapFS{}
	for _, name := range []string{"en.json", "zh-TW.json", "zh-CN.json"} {
		data, err := os.ReadFile("../../locales/" + name)
		if err != nil {
			panic(err)
		}
		files[name] = &fstest.MapFile{Data: data}
	}
	return files
}

// testClock is an adjustable clock shared by a server and its test.
type testClock struct {
	mu     sync.Mutex
	offset time.Duration
	fixed  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fixed.IsZero() {
		return c.fixed.Add(c.offset)
	}
	return time.Now().Add(c.offset)
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

type testEnv struct {
	t      *testing.T
	server *Server
	http   *httptest.Server
	clock  *testClock
	config Config
}

type option func(*Config)

// testDatabaseURL selects an external PostgreSQL/MySQL/MariaDB database for the
// whole suite. Each server starts from an empty schema there.
func testDatabaseURL() string { return os.Getenv("TEST_DATABASE_URL") }

func newEnv(t *testing.T, options ...option) *testEnv {
	t.Helper()
	clock := &testClock{}
	config := Config{
		DataDir:     t.TempDir(),
		DatabaseURL: testDatabaseURL(),
		JWTSecret:   testJWTSecret,
		Debug:       true,
		Env:         map[string]string{},
		Logger:      testLogger(),
		Now:         clock.now,
		JobWorkers:  2,
	}
	for _, apply := range options {
		apply(&config)
	}
	if config.DatabaseURL != "" {
		resetExternalDatabase(t, config.DatabaseURL)
	}
	return startEnv(t, config, clock)
}

func startEnv(t *testing.T, config Config, clock *testClock) *testEnv {
	t.Helper()
	server, err := New(config, testFrontend(), testLocales())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env := &testEnv{t: t, server: server, clock: clock, config: config}
	env.http = httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		env.http.Close()
		_ = server.Close()
	})
	return env
}

// restart closes the server and starts a new one on the same data.
func (e *testEnv) restart(options ...option) {
	e.t.Helper()
	e.http.Close()
	if err := e.server.Close(); err != nil {
		e.t.Fatalf("Close: %v", err)
	}
	config := e.config
	for _, apply := range options {
		apply(&config)
	}
	fresh := startEnv(e.t, config, e.clock)
	*e = *fresh
}

func resetExternalDatabase(t *testing.T, rawURL string) {
	t.Helper()
	db, err := openDatabase(rawURL, t.TempDir())
	if err != nil {
		t.Fatalf("open TEST_DATABASE_URL: %v", err)
	}
	defer db.Close()
	for _, table := range []string{"mfa_challenges", "mfa_accounts", "jobs", "rate_limit_buckets", "revoked_tokens",
		"cache_revisions", "settings", "users"} {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
}

type client struct {
	t      *testing.T
	env    *testEnv
	http   *http.Client
	bearer string
	header map[string]string
}

func (e *testEnv) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: e.t, env: e, http: &http.Client{Jar: jar}, header: map[string]string{}}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json() map[string]any {
	var value map[string]any
	_ = json.Unmarshal(r.body, &value)
	return value
}

func (r response) str(key string) string {
	value, _ := r.json()[key].(string)
	return value
}

func (c *client) cookie(name string) string {
	base, _ := url.Parse(c.env.http.URL)
	for _, cookie := range c.http.Jar.Cookies(base) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

func (c *client) setCookie(name, value string) {
	base, _ := url.Parse(c.env.http.URL)
	c.http.Jar.SetCookies(base, []*http.Cookie{{Name: name, Value: value, Path: "/"}})
}

func (c *client) do(method, path string, body io.Reader, contentType string) response {
	c.t.Helper()
	request, err := http.NewRequest(method, c.env.http.URL+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if csrf := c.cookie(csrfCookieName); csrf != "" && method != http.MethodGet {
		request.Header.Set(csrfHeaderName, csrf)
	}
	if c.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for key, value := range c.header {
		request.Header.Set(key, value)
	}
	result, err := c.http.Do(request)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer result.Body.Close()
	data, _ := io.ReadAll(result.Body)
	return response{status: result.StatusCode, header: result.Header, body: data}
}

func (c *client) get(path string) response { return c.do(http.MethodGet, path, nil, "") }

func (c *client) send(method, path string, payload any) response {
	c.t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(method, path, bytes.NewReader(data), "application/json")
}

func (c *client) post(path string, payload any) response {
	return c.send(http.MethodPost, path, payload)
}

// upload posts files (name -> content) with form fields.
func (c *client) upload(fields map[string]string, files map[string]string) response {
	c.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		_ = writer.WriteField(key, value)
	}
	for name, content := range files {
		part, _ := writer.CreateFormFile("files", name)
		_, _ = part.Write([]byte(content))
	}
	_ = writer.Close()
	return c.do(http.MethodPost, "/api/jobs", &body, writer.FormDataContentType())
}

func expectStatus(t *testing.T, got response, want int) {
	t.Helper()
	if got.status != want {
		t.Fatalf("status %d, want %d: %s", got.status, want, got.body)
	}
}

// setupAdmin completes first-run setup and returns the signed-in admin client.
func (e *testEnv) setupAdmin() *client {
	e.t.Helper()
	admin := e.client()
	expectStatus(e.t, admin.post("/api/auth/setup", map[string]any{"username": "admin", "password": testPassword}),
		http.StatusCreated)
	return admin
}

// createUser makes an account through the admin API and signs it in.
func (e *testEnv) createUser(admin *client, username, role string) *client {
	e.t.Helper()
	expectStatus(e.t, admin.post("/api/users", map[string]any{
		"username": username, "password": testPassword, "role": role,
	}), http.StatusCreated)
	return e.login(username)
}

func (e *testEnv) login(username string) *client {
	e.t.Helper()
	user := e.client()
	expectStatus(e.t, user.post("/api/auth/login", map[string]any{"username": username, "password": testPassword}),
		http.StatusOK)
	return user
}

func (e *testEnv) waitForJob(c *client, jobID string) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job := c.get("/api/jobs/" + jobID).json()
		if status, _ := job["status"].(string); terminalStatuses[status] {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("job %s did not finish", jobID)
	return nil
}

func (e *testEnv) scalar(query string, args ...any) int64 {
	e.t.Helper()
	value, err := e.server.db.conn().scalarInt(context.Background(), query, args...)
	if err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return value
}

func (e *testEnv) text(query string, args ...any) string {
	e.t.Helper()
	var value sql.NullString
	if err := e.server.db.conn().queryRow(context.Background(), query, args...).Scan(&value); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return value.String
}

func (e *testEnv) exec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.server.db.conn().exec(context.Background(), query, args...); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
}

func jobIDs(t *testing.T, r response) []string {
	t.Helper()
	var body struct {
		Jobs []string `json:"jobs"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil || len(body.Jobs) == 0 {
		t.Fatalf("no jobs in %s", r.body)
	}
	return body.Jobs
}

func contentOf(t *testing.T, value any) string {
	t.Helper()
	return fmt.Sprint(value)
}

// testLogger discards logs unless TEST_LOG=1.
func testLogger() *slog.Logger {
	if os.Getenv("TEST_LOG") == "1" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testNow() time.Time { return time.Now() }

func containsText(haystack, needle string) bool { return strings.Contains(haystack, needle) }
