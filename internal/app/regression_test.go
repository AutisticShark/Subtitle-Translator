package app

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AutisticShark/Subtitle-Translator/internal/engine"
)

// serveWithContext runs one request through the full handler with ctx, the way
// net/http cancels a request context when the client disconnects.
func (e *testEnv) serveWithContext(ctx context.Context, method, path, body string,
	header map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	for key, value := range header {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// A client that disconnects while its failing password is hashed must still
// lock the account. Otherwise the reserved attempt is committed but the lock
// is not, the counter stays at the limit, and no later reservation (not even
// with the right password) can succeed until an administrator unlocks it.
func TestLoginLockoutSurvivesClientDisconnect(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	env.createUser(admin, "target", "user")
	env.exec("UPDATE users SET failed_login_count = ? WHERE username = 'target'", loginFailureLimit-1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Disconnect as soon as the final attempt is reserved, while it is hashed.
		for ctx.Err() == nil {
			var count int64
			err := env.server.db.conn().queryRow(context.Background(),
				"SELECT failed_login_count FROM users WHERE username = 'target'").Scan(&count)
			if err == nil && count >= loginFailureLimit {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	env.serveWithContext(ctx, http.MethodPost, "/api/auth/login",
		`{"username": "target", "password": "wrong password!!"}`, nil)
	cancel()

	if env.text("SELECT locked_until FROM users WHERE username = 'target'") == "" {
		t.Fatalf("account not locked; failed_login_count=%d",
			env.scalar("SELECT failed_login_count FROM users WHERE username = 'target'"))
	}
	env.clock.advance(16 * time.Minute)
	env.login("target")
}

// A pre-canceled request is the deterministic form of the same disconnect.
func TestLoginBookkeepingIgnoresRequestCancellation(t *testing.T) {
	env := newEnv(t)
	env.setupAdmin()
	env.exec("UPDATE users SET failed_login_count = ? WHERE username = 'admin'", loginFailureLimit-1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env.serveWithContext(ctx, http.MethodPost, "/api/auth/login",
		`{"username": "admin", "password": "wrong password!!"}`, nil)
	if env.text("SELECT locked_until FROM users WHERE username = 'admin'") == "" ||
		env.scalar("SELECT failed_login_count FROM users WHERE username = 'admin'") != 0 {
		t.Fatal("the final failed attempt did not lock the account")
	}
}

// Logout must revoke both the token and its session even if the client goes
// away between the two writes; otherwise earlier refreshed tokens survive.
func TestLogoutRevokesTheSessionAfterClientDisconnect(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	request := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	request.AddCookie(&http.Cookie{Name: accessCookieName, Value: admin.cookie(accessCookieName)})
	auth, failure := env.server.authenticate(request)
	if failure != nil {
		t.Fatalf("authenticate: %v", failure)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logout := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil).WithContext(ctx)
	env.server.handleLogout(httptest.NewRecorder(), logout, auth)
	if env.scalar("SELECT COUNT(*) FROM revoked_tokens WHERE jti = ?", auth.claims.jti) != 1 ||
		env.scalar("SELECT COUNT(*) FROM revoked_tokens WHERE jti = ?",
			revokedSessionPrefix+auth.claims.sessionID()) != 1 {
		t.Fatal("logout did not record both revocation keys")
	}
}

// forgetUnsentCode runs after the request may have ended; it must not reuse the
// request context, or an undelivered code stays stored (rule 29).
func TestUndeliveredEmailCodeIsForgottenAfterClientDisconnect(t *testing.T) {
	env := newEnv(t)
	env.setupAdmin()
	userID := env.text("SELECT id FROM users WHERE username = 'admin'")
	digest := env.server.digestCode("challenge-1", "123456")
	env.exec("INSERT INTO mfa_challenges (id, user_id, version, purpose, method, secret, email, code_hash, expires, "+
		"transport) VALUES ('challenge-1', ?, 0, 'login', 'email', '', 'a@example.com', ?, ?, 'cookies')",
		userID, digest, env.server.mfaNow()+300)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env.server.forgetUnsentCode(ctx, "challenge-1", "123456")
	if env.text("SELECT code_hash FROM mfa_challenges WHERE id = 'challenge-1'") != "" {
		t.Fatal("undelivered code kept after the request context ended")
	}
}

// cancelingEmail ends the request (like a client disconnect) during the send and
// fails the way a transport does when its context is canceled.
type cancelingEmail struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	sent   int
}

func (c *cancelingEmail) Send(ctx context.Context, _, _, _, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.sent++
	return nil
}

// A stored code hash must mean the send was attempted and not known to have
// failed (rule 29), whatever the client does meanwhile.
func TestEmailCodeStateMatchesDeliveryWhenTheClientDisconnects(t *testing.T) {
	sender := &cancelingEmail{}
	env := newEnv(t, func(c *Config) { c.Email = sender })
	env.setupAdmin()
	userID := env.text("SELECT id FROM users WHERE username = 'admin'")
	env.exec("INSERT INTO mfa_accounts ("+mfaAccountColumns+") VALUES (?, 'email', '', 'admin@example.com', -1, "+
		"'[]', 0, 0, 0, 0, 0, 0, 0)", userID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender.mu.Lock()
	sender.cancel = cancel
	sender.mu.Unlock()
	env.serveWithContext(ctx, http.MethodPost, "/api/auth/login",
		`{"username": "admin", "password": "`+testPassword+`"}`, nil)
	stored := env.text("SELECT code_hash FROM mfa_challenges WHERE purpose = 'login'")
	sender.mu.Lock()
	sent := sender.sent
	sender.mu.Unlock()
	if (stored != "") != (sent == 1) {
		t.Fatalf("code hash stored=%v but email sent=%d", stored != "", sent)
	}
}

// panickingTranslator panics for one target language.
type panickingTranslator struct{ blockingTranslator }

func (p *panickingTranslator) Translate(_ context.Context, _ []byte, _, target string, _ engine.Config,
	_ func(done, total int)) ([]byte, engine.Stats, error) {
	if target == "ja" {
		panic("provider-secret-cannot-appear-in-logs")
	}
	return []byte("translated " + target), engine.Stats{Cues: 2, Segments: 2}, nil
}

// Python recorded any exception as a failed job. A panic in a worker goroutine
// would instead end the whole server, so it must be recorded the same way.
type lockedLogBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func TestTranslatorPanicFailsTheJobAndKeepsTheServerRunning(t *testing.T) {
	var logs lockedLogBuffer
	env := newEnv(t, withTranslator(&panickingTranslator{}), func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	admin := env.setupAdmin()
	upload := admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle})
	expectStatus(t, upload, http.StatusAccepted)
	job := env.waitForJob(admin, jobIDs(t, upload)[0])
	if job["status"] != "failed" || job["stage"] != "Translation failed" {
		t.Fatalf("job %v", job)
	}
	if strings.Contains(logs.String(), "provider-secret-cannot-appear-in-logs") {
		t.Fatal("panic contents leaked into operational logs")
	}
	next := admin.upload(map[string]string{"provider": "echo", "target_languages": "fr"},
		map[string]string{"b.srt": testSubtitle})
	expectStatus(t, next, http.StatusAccepted)
	if job := env.waitForJob(admin, jobIDs(t, next)[0]); job["status"] != "completed" {
		t.Fatalf("job after the panic %v", job)
	}
}

// MySQL/MariaDB compare job ids case-insensitively, so a differently cased id
// in the path can match the row. Every filesystem and runner action must then
// use the stored id: deleting through such a path used to drop the row and
// leave the job folder behind, and the ZIP download looked in a folder that
// does not exist. Case-sensitive backends simply report the job as not found.
func TestDifferentlyCasedJobIDsUseTheStoredID(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	upload := admin.upload(map[string]string{"provider": "echo", "target_languages": "ja,fr"},
		map[string]string{"a.srt": testSubtitle})
	expectStatus(t, upload, http.StatusAccepted)
	jobID := jobIDs(t, upload)[0]
	env.waitForJob(admin, jobID)
	upper := strings.ToUpper(jobID)
	folder := filepath.Join(env.server.jobsDir, jobID)

	matched := admin.get("/api/jobs/" + upper)
	switch matched.status {
	case http.StatusNotFound:
		// Case-sensitive backend: every other route must agree.
		expectStatus(t, admin.get("/api/jobs/"+upper+"/download"), http.StatusNotFound)
		expectStatus(t, admin.do(http.MethodDelete, "/api/jobs/"+upper, nil, ""), http.StatusNotFound)
		if _, err := os.Stat(folder); err != nil {
			t.Fatal("folder removed by a request that matched no job")
		}
		return
	case http.StatusOK:
		if matched.str("id") != jobID {
			t.Fatalf("matched job %s", matched.body)
		}
	default:
		t.Fatalf("status %d: %s", matched.status, matched.body)
	}
	archive := admin.get("/api/jobs/" + upper + "/download")
	expectStatus(t, archive, http.StatusOK)
	if _, err := zip.NewReader(bytes.NewReader(archive.body), int64(len(archive.body))); err != nil {
		t.Fatalf("archive: %v", err)
	}
	deleted := admin.do(http.MethodDelete, "/api/jobs/"+upper, nil, "")
	expectStatus(t, deleted, http.StatusOK)
	if deleted.str("deleted") != jobID {
		t.Fatalf("deleted %s", deleted.body)
	}
	if _, err := os.Stat(folder); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("job folder orphaned by a differently cased id")
	}
	expectStatus(t, admin.get("/api/jobs/"+jobID), http.StatusNotFound)
}

// Conditional updates are checked by row count. SQLAlchemy's MySQL dialects
// report matched rows (CLIENT_FOUND_ROWS); without it MySQL and MariaDB count
// only changed rows, so an update that rewrites identical values reads as
// "not applied" (for example a successful login whose reset raced another one).
func TestUpdateRowCountCountsMatchedRows(t *testing.T) {
	env := newEnv(t)
	env.setupAdmin()
	changed, err := env.server.db.conn().exec(context.Background(),
		"UPDATE users SET failed_login_count = failed_login_count WHERE username = 'admin'")
	if err != nil || changed != 1 {
		t.Fatalf("rows affected %d, %v", changed, err)
	}
}
