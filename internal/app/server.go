package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AutisticShark/Subtitle-Translator/internal/engine"
)

// Server is the HTTP application. Create it with New and release it with Close.
type Server struct {
	cfg           Config
	db            *database
	cipher        *fernet
	cache         *readCache
	catalogs      *catalogs
	frontend      fs.FS
	indexHTML     []byte
	defaults      map[string]string
	jwtSecret     []byte
	mfaKey        []byte
	stableKeys    bool
	dummyHash     string
	jobsDir       string
	logger        *slog.Logger
	translator    Translator
	languages     map[string]Language
	loginLimit    *attemptLimiter
	registerLimit *attemptLimiter
	runner        *jobRunner
	handler       http.Handler
	accessTTL     time.Duration
	sessionMax    time.Duration
	maxBody       int64

	mfaPurgeMu   sync.Mutex
	nextMFAPurge int64
	warnOnce     sync.Map

	closeOnce sync.Once
	closeErr  error
}

const (
	loginFailureLimit  = 5
	loginLockMinutes   = 15
	loginRateLimit     = 30
	registerRateLimit  = 10
	authBodyLimit      = 8192
	passwordMinLength  = 12
	maxTargetLanguages = 20
	jobListMaxOffset   = 1_000_000
	// adminRosterLockSetting is a seeded row that is never deleted; writing it
	// serializes changes that could remove the final active administrator.
	adminRosterLockSetting = "registration_enabled"
	revokedSessionPrefix   = "sid:"
)

// New opens the database, creates or migrates the schema, recovers jobs that a
// previous process left active, bootstraps the first administrator and starts
// the job workers. frontend is rooted at the built UI (index.html, assets/);
// locales holds en.json, zh-TW.json and zh-CN.json.
func New(config Config, frontend fs.FS, locales fs.FS) (*Server, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:        config,
		frontend:   frontend,
		logger:     config.Logger,
		accessTTL:  time.Duration(config.JWTAccessMinutes) * time.Minute,
		sessionMax: time.Duration(config.SessionMaxHours) * time.Hour,
		maxBody:    int64(config.MaxUploadMB) * 1024 * 1024,
	}
	if s.catalogs, err = loadCatalogs(locales); err != nil {
		return nil, err
	}
	if s.defaults, err = settingDefaults(config.Env); err != nil {
		return nil, err
	}
	if frontend != nil {
		if s.indexHTML, err = fs.ReadFile(frontend, "index.html"); err != nil {
			return nil, fmt.Errorf("frontend index.html: %w", err)
		}
	}
	s.translator = config.Translator
	if s.translator == nil {
		s.translator = engineTranslator{}
	}
	s.languages = engine.Languages

	dataDir, err := filepath.Abs(config.DataDir)
	if err != nil {
		return nil, err
	}
	s.jobsDir = filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(s.jobsDir, 0o755); err != nil {
		return nil, err
	}

	secret := config.JWTSecret
	if secret == "" {
		secret = randomURLToken(64)
		s.logger.Warn("JWT_SECRET_KEY is not configured; generated tokens will become invalid after restart " +
			"and secrets cannot be saved")
	}
	s.jwtSecret = []byte(secret)
	s.stableKeys = config.JWTSecret != "" || config.EncryptionKey != ""
	mac := hmac.New(sha256.New, s.jwtSecret)
	mac.Write([]byte("subtitle-mfa-v1"))
	s.mfaKey = mac.Sum(nil)
	key, err := encryptionKey(config.EncryptionKey, secret)
	if err != nil {
		return nil, err
	}
	if s.cipher, err = newFernet(key); err != nil {
		return nil, err
	}

	if s.db, err = openDatabase(config.DatabaseURL, dataDir); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = s.db.Close()
		}
	}()
	if s.cache, err = newReadCache(config.RedisURL, config.RedisTTL, config.RedisKeyPrefix, s.db.identity,
		s.cipher, s.logger, s.now); err != nil {
		return nil, err
	}
	ctx := context.Background()
	if err := s.db.initialize(ctx, s.defaults, isoSeconds(s.now()), !config.SkipRecovery); err != nil {
		return nil, fmt.Errorf("database initialization failed: %w", err)
	}
	if err := retryDatabaseRace(ctx, func() error { return s.bootstrapAdmin(ctx) }); err != nil {
		return nil, err
	}
	if err := retryDatabaseRace(ctx, func() error { return s.encryptExistingSecrets(ctx) }); err != nil {
		return nil, err
	}
	s.dummyHash = hashPassword(randomURLToken(32))
	s.loginLimit = newAttemptLimiter(loginRateLimit, loginLockMinutes*time.Minute, s.now)
	s.registerLimit = newAttemptLimiter(registerRateLimit, loginLockMinutes*time.Minute, s.now)
	s.runner = newJobRunner(s, config.JobWorkers)
	s.handler = s.routes()
	ok = true
	return s, nil
}

// Handler returns the application's HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Close stops the job workers (finalizing interrupted jobs) and closes the
// database and cache connections. It is safe to call more than once.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.runner.close()
		s.closeErr = errors.Join(s.cache.close(), s.db.Close())
	})
	return s.closeErr
}

func (s *Server) now() time.Time { return s.cfg.Now().UTC() }

func (s *Server) tr(locale, message string, args ...any) string {
	return s.catalogs.translate(locale, message, args...)
}

// bootstrapAdmin creates the first administrator from ADMIN_PASSWORD (or the
// deprecated APP_PASSWORD) when no account exists yet.
func (s *Server) bootstrapAdmin(ctx context.Context) error {
	password := s.cfg.AdminPassword
	if password == "" {
		return nil
	}
	username := normalizeUsername(s.cfg.AdminUsername)
	count, err := s.db.conn().scalarInt(ctx, "SELECT COUNT(*) FROM users")
	if err != nil {
		return err
	}
	if count > 0 {
		// Already bootstrapped: never reject a previously accepted account name.
		return nil
	}
	if message := s.validateUsername(defaultLocale, username); message != "" {
		return errors.New(message)
	}
	if message := s.validatePassword(defaultLocale, password); message != "" {
		return errors.New(message)
	}
	timestamp := isoSeconds(s.now())
	err = s.db.tx(ctx, func(c conn) error {
		count, err := c.scalarInt(ctx, "SELECT COUNT(*) FROM users")
		if err != nil || count > 0 {
			return err
		}
		return s.createFirstAdmin(ctx, c, uuidHex(), username, password, timestamp)
	})
	if err != nil && isIntegrityError(err) {
		count, countErr := s.db.conn().scalarInt(ctx, "SELECT COUNT(*) FROM users")
		if countErr == nil && count > 0 {
			err = nil
		}
	}
	if err == nil && s.cfg.LegacyAppPassword {
		s.logger.Warn("APP_PASSWORD is deprecated; it was used to bootstrap the admin account")
	}
	return err
}

// createFirstAdmin inserts the one-time setup marker (whose primary key makes
// concurrent first-admin setups collide), the administrator, and claims jobs
// from before authentication existed.
func (s *Server) createFirstAdmin(ctx context.Context, c conn, userID, username, password, timestamp string) error {
	if _, err := c.exec(ctx, "INSERT INTO settings (name, value, updated_at) VALUES ('_auth_setup_complete', '1', ?)",
		timestamp); err != nil {
		return err
	}
	if err := insertUser(ctx, c, userID, username, hashPassword(password), "admin", timestamp); err != nil {
		return err
	}
	if _, err := c.exec(ctx, "UPDATE jobs SET user_id = ? WHERE user_id IS NULL", userID); err != nil {
		return err
	}
	return bumpCacheRevision(ctx, c, "jobs")
}

// encryptExistingSecrets upgrades plaintext secrets written by old versions.
func (s *Server) encryptExistingSecrets(ctx context.Context) error {
	if !s.stableKeys {
		return nil
	}
	return s.db.tx(ctx, func(c conn) error {
		rows, err := c.query(ctx, "SELECT name, value FROM settings WHERE name IN ("+
			placeholders(len(secretKeys))+")", stringArgs(secretKeys)...)
		if err != nil {
			return err
		}
		var stored []settingRow
		for rows.Next() {
			var row settingRow
			if err := rows.Scan(&row.Name, &row.Value); err != nil {
				rows.Close()
				return err
			}
			stored = append(stored, row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, row := range stored {
			switch {
			case strings.HasPrefix(row.Value, secretPrefix):
				if _, err := s.decryptSecret(row.Value); err != nil {
					return err
				}
			case row.Value != "":
				if _, err := c.exec(ctx, "UPDATE settings SET value = ?, updated_at = ? WHERE name = ?",
					s.encryptSecret(row.Value), isoSeconds(s.now()), row.Name); err != nil {
					return err
				}
			}
		}
		// Unconditional: a restored database can repeat revisions Redis still holds.
		return bumpCacheRevision(ctx, c, "settings")
	})
}

// --- request state and middleware -------------------------------------------

type ctxKey struct{}

// requestState is per-request data shared by middleware and handlers.
type requestState struct {
	locale    string
	noRefresh bool
	remote    string
	authBody  bool
}

func state(r *http.Request) *requestState {
	if value, ok := r.Context().Value(ctxKey{}).(*requestState); ok {
		return value
	}
	return &requestState{locale: defaultLocale}
}

func localeOf(r *http.Request) string { return state(r).locale }

const contentSecurityPolicy = "default-src 'self'; script-src 'self' https://challenges.cloudflare.com " +
	"https://www.google.com https://www.gstatic.com https://js.hcaptcha.com; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; " +
	"connect-src 'self' https://challenges.cloudflare.com https://www.google.com " +
	"https://www.gstatic.com https://*.hcaptcha.com; frame-src " +
	"https://challenges.cloudflare.com https://www.google.com https://recaptcha.google.com " +
	"https://*.hcaptcha.com; object-src 'none'; base-uri 'self'; frame-ancestors 'none'"

// middleware sets the locale, client address, body limit and security headers,
// and refreshes a nearly expired cookie session (security_headers in Python).
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := &requestState{locale: s.catalogs.requestLocale(r), remote: s.remoteAddr(r)}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, st))
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBody)
		writer := &headerWriter{ResponseWriter: w, before: func(h http.Header) {
			setDefault(h, "X-Content-Type-Options", "nosniff")
			setDefault(h, "X-Frame-Options", "DENY")
			setDefault(h, "Referrer-Policy", "no-referrer")
			setDefault(h, "Content-Security-Policy", contentSecurityPolicy)
			setDefault(h, "Cache-Control", "no-store")
			setDefault(h, "Content-Language", st.locale)
			h.Add("Vary", "Accept-Language")
			h.Add("Vary", "Cookie")
			if !st.noRefresh {
				s.refreshSessionCookie(r, h)
			}
		}}
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				s.logger.Error("request handler panicked", "path", r.URL.Path, "panic", fmt.Sprint(recovered))
				if !writer.wrote {
					s.internalError(writer, r, nil)
				}
			}
		}()
		next.ServeHTTP(writer, r)
	})
}

func setDefault(h http.Header, name, value string) {
	if h.Get(name) == "" {
		h.Set(name, value)
	}
}

// headerWriter runs before() once, just before the status line is written.
type headerWriter struct {
	http.ResponseWriter
	before func(http.Header)
	wrote  bool
}

func (w *headerWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		w.before(w.Header())
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *headerWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *headerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// remoteAddr is request.remote_addr: the peer address, or with
// TRUSTED_PROXY_COUNT=n the n-th X-Forwarded-For entry from the right (ProxyFix).
func (s *Server) remoteAddr(r *http.Request) string {
	address := r.RemoteAddr
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	if count := s.cfg.TrustedProxyCount; count > 0 {
		var values []string
		for _, header := range r.Header.Values("X-Forwarded-For") {
			for _, item := range strings.Split(header, ",") {
				if item = strings.TrimSpace(item); item != "" {
					values = append(values, item)
				}
			}
		}
		if len(values) >= count {
			address = values[len(values)-count]
		}
	}
	return address
}

// --- responses ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, `{"error":"Internal Server Error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}

func (s *Server) jsonError(w http.ResponseWriter, r *http.Request, status int, message string, args ...any) {
	writeJSON(w, status, map[string]any{"error": s.tr(localeOf(r), message, args...)})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		s.logger.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal Server Error"})
}

// errBodyTooLarge reports a request body over the configured limit.
var errBodyTooLarge = errors.New("request body too large")

// readJSONObject mirrors request.get_json(silent=True) with a dict check: a
// non-JSON content type, malformed JSON or a non-object yields an empty map.
func readJSONObject(r *http.Request, limit int64) (map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) || int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	if err != nil {
		return map[string]any{}, nil
	}
	return decodeJSONObject(r.Header.Get("Content-Type"), body), nil
}

func decodeJSONObject(contentType string, body []byte) map[string]any {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	}
	if mediaType != "application/json" &&
		!(strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json")) {
		return map[string]any{}
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return map[string]any{}
	}
	if decoder.More() {
		return map[string]any{}
	}
	object, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return object
}

// jsonPayload reads a JSON object for an ordinary API request.
func (s *Server) jsonPayload(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	payload, err := readJSONObject(r, s.maxBody)
	if errors.Is(err, errBodyTooLarge) {
		s.jsonError(w, r, http.StatusRequestEntityTooLarge, "Upload exceeds the {max_upload_mb} MB limit",
			"max_upload_mb", s.cfg.MaxUploadMB)
		return nil, false
	}
	return payload, true
}

// authPayload caps unauthenticated auth bodies, including chunked ones: a
// body that reaches the limit was cut short and is rejected too.
func (s *Server) authPayload(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if r.ContentLength > authBodyLimit {
		s.jsonError(w, r, http.StatusRequestEntityTooLarge, "Authentication request is too large")
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, authBodyLimit))
	if err != nil && !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.jsonError(w, r, http.StatusRequestEntityTooLarge, "Authentication request is too large")
			return nil, false
		}
	}
	if len(body) >= authBodyLimit {
		s.jsonError(w, r, http.StatusRequestEntityTooLarge, "Authentication request is too large")
		return nil, false
	}
	return decodeJSONObject(r.Header.Get("Content-Type"), body), true
}

// mfaPayload mirrors MFA.payload: bodies over 8 KiB are treated as empty.
func mfaPayload(r *http.Request) map[string]any {
	if r.ContentLength > authBodyLimit {
		return map[string]any{}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, authBodyLimit+1))
	if err != nil || len(body) > authBodyLimit {
		return map[string]any{}
	}
	return decodeJSONObject(r.Header.Get("Content-Type"), body)
}
