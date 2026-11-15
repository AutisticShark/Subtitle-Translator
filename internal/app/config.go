// Package app is the Subtitle Translator web backend: the JSON API, browser
// session handling, background translation jobs and the initial HTML shell.
//
// It replaces the Flask application (webapp.py and its helper modules) and keeps
// its HTTP contract, database schema, encrypted secrets, password hashes and
// tokens compatible, so an existing data directory and database keep working.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AutisticShark/Subtitle-Translator/internal/email"
)

// EmailSender delivers one transactional message (see internal/email).
type EmailSender interface {
	Send(ctx context.Context, to, subject, text, html string) error
}

// Config holds every deployment setting. Tests construct it directly; zero
// values select the documented defaults (see withDefaults).
type Config struct {
	// Addr is the listen address used by the entrypoint; loopback by default.
	Addr string
	// DataDir holds app.db (SQLite) and the per-job upload folders.
	DataDir string
	// DatabaseURL selects PostgreSQL, MySQL or MariaDB; empty uses SQLite in DataDir.
	DatabaseURL string
	// JWTSecret signs access and MFA challenge tokens. Empty generates a random
	// per-process secret; secrets and MFA enrollment then cannot be saved.
	JWTSecret string
	// EncryptionKey is an explicit Fernet key for saved secrets
	// (API_KEY_ENCRYPTION_KEY); empty derives one from JWTSecret.
	EncryptionKey string
	// Debug enables the offline Echo provider (FLASK_DEBUG).
	Debug bool
	// MaxUploadMB caps request bodies (default 200).
	MaxUploadMB int
	// JWTAccessMinutes is the access-token lifetime (default 30, minimum 5).
	JWTAccessMinutes int
	// SessionMaxHours is the absolute browser-session lifetime (default 12, 1-720).
	SessionMaxHours int
	// CookieSecure marks session and locale cookies Secure (JWT_COOKIE_SECURE).
	CookieSecure bool
	// TrustedProxyCount is the exact number of reverse proxies in front (0-10).
	TrustedProxyCount int
	// JobWorkers is the number of concurrent translation jobs (default 2).
	JobWorkers int
	// RedisURL enables the optional encrypted read cache.
	RedisURL string
	// RedisTTL is the cache entry lifetime in seconds (default 60, 1-3600).
	RedisTTL int
	// RedisKeyPrefix namespaces cache keys (default "subtitle-translator").
	RedisKeyPrefix string
	// AdminUsername and AdminPassword bootstrap the first administrator.
	AdminUsername string
	AdminPassword string
	// LegacyAppPassword records that AdminPassword came from the deprecated APP_PASSWORD.
	LegacyAppPassword bool
	// Env supplies setting defaults and environment-provided secrets:
	// REGISTRATION_ENABLED, CAPTCHA_PROVIDER, CAPTCHA_HOSTNAME, CAPTCHA_ON_*,
	// *_SITE_KEY, *_SECRET_KEY and the provider *_API_KEY variables.
	Env map[string]string
	// Email delivers MFA codes; nil disables email verification.
	Email EmailSender
	// Translator runs translations; nil uses internal/engine.
	Translator Translator
	// CaptchaClient posts CAPTCHA verifications (tests replace its transport).
	CaptchaClient *http.Client
	// Logger receives operational logs; nil uses slog.Default().
	Logger *slog.Logger
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// SkipRecovery leaves jobs that a previous process left queued, processing
	// or canceling untouched at startup. Normally New finalizes them once.
	SkipRecovery bool
}

func (c *Config) env(name string) string {
	if c.Env == nil {
		return ""
	}
	return c.Env[name]
}

// ConfigFromEnv reads the process environment with the variable names the Python
// application used, so existing deployments keep their configuration.
func ConfigFromEnv() (Config, error) {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok && key != "" {
			env[key] = value
		}
	}
	return configFromMap(env)
}

func configFromMap(env map[string]string) (Config, error) {
	config := Config{Env: env}
	get := func(name string) string { return env[name] }
	integer := func(name string, fallback int) (int, error) {
		raw := strings.TrimSpace(get(name))
		if raw == "" {
			return fallback, nil
		}
		value, err := strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("%s must be a whole number", name)
		}
		return value, nil
	}
	var err error
	config.Addr = strings.TrimSpace(get("LISTEN_ADDR"))
	if config.Addr == "" {
		if port := strings.TrimSpace(get("PORT")); port != "" {
			if _, err := strconv.Atoi(port); err != nil {
				return Config{}, errors.New("PORT must be a whole number")
			}
			config.Addr = "127.0.0.1:" + port
		}
	}
	config.DataDir = get("DATA_DIR")
	config.DatabaseURL = strings.TrimSpace(get("DATABASE_URL"))
	config.JWTSecret = strings.TrimSpace(get("JWT_SECRET_KEY"))
	config.EncryptionKey = strings.TrimSpace(get("API_KEY_ENCRYPTION_KEY"))
	config.Debug = flag(get("FLASK_DEBUG")) || flag(get("APP_DEBUG"))
	config.CookieSecure = flag(get("JWT_COOKIE_SECURE"))
	if config.MaxUploadMB, err = integer("MAX_UPLOAD_MB", 200); err != nil {
		return Config{}, err
	}
	if config.JWTAccessMinutes, err = integer("JWT_ACCESS_MINUTES", 30); err != nil {
		return Config{}, err
	}
	if config.SessionMaxHours, err = integer("SESSION_MAX_HOURS", 12); err != nil {
		return Config{}, err
	}
	if config.SessionMaxHours < 1 || config.SessionMaxHours > 720 {
		return Config{}, errors.New("SESSION_MAX_HOURS must be between 1 and 720")
	}
	if config.TrustedProxyCount, err = integer("TRUSTED_PROXY_COUNT", 0); err != nil {
		return Config{}, errors.New("TRUSTED_PROXY_COUNT must be a whole number")
	}
	if config.JobWorkers, err = integer("JOB_WORKERS", 2); err != nil {
		return Config{}, err
	}
	config.RedisURL = strings.TrimSpace(get("REDIS_URL"))
	if config.RedisTTL, err = integer("REDIS_CACHE_TTL", 60); err != nil {
		return Config{}, err
	}
	if config.RedisTTL < 1 || config.RedisTTL > 3600 {
		return Config{}, errors.New("REDIS_CACHE_TTL must be between 1 and 3600 seconds")
	}
	config.RedisKeyPrefix = strings.TrimSpace(get("REDIS_KEY_PREFIX"))
	if config.RedisKeyPrefix == "" {
		config.RedisKeyPrefix = "subtitle-translator"
	}
	config.AdminUsername = get("ADMIN_USERNAME")
	config.AdminPassword = get("ADMIN_PASSWORD")
	if config.AdminPassword == "" && get("APP_PASSWORD") != "" {
		config.AdminPassword = get("APP_PASSWORD")
		config.LegacyAppPassword = true
	}
	// Like email_delivery.email_available(): an incomplete or unknown email
	// configuration disables email verification but never stops the server, so
	// TOTP and recovery codes keep working. email.New's errors carry no values
	// from the environment.
	sender, err := email.New(env)
	switch {
	case err != nil:
		slog.Default().Warn("Email delivery is disabled because its configuration is invalid", "reason", err.Error())
	case sender != nil:
		config.Email = sender
	}
	return config, nil
}

func flag(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// withDefaults validates a directly constructed Config and fills zero values.
func (c Config) withDefaults() (Config, error) {
	if c.Addr == "" {
		c.Addr = "127.0.0.1:8000"
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if c.MaxUploadMB == 0 {
		c.MaxUploadMB = 200
	}
	if c.MaxUploadMB < 1 {
		return c, errors.New("MAX_UPLOAD_MB must be positive")
	}
	if c.JWTAccessMinutes == 0 {
		c.JWTAccessMinutes = 30
	}
	c.JWTAccessMinutes = max(5, c.JWTAccessMinutes)
	if c.SessionMaxHours == 0 {
		c.SessionMaxHours = 12
	}
	if c.SessionMaxHours < 1 || c.SessionMaxHours > 720 {
		return c, errors.New("SESSION_MAX_HOURS must be between 1 and 720")
	}
	if c.TrustedProxyCount < 0 || c.TrustedProxyCount > 10 {
		return c, errors.New("TRUSTED_PROXY_COUNT must be between 0 and 10")
	}
	c.JobWorkers = max(1, c.JobWorkers)
	if c.RedisTTL == 0 {
		c.RedisTTL = 60
	}
	if c.RedisTTL < 1 || c.RedisTTL > 3600 {
		return c, errors.New("REDIS_CACHE_TTL must be between 1 and 3600 seconds")
	}
	if c.RedisKeyPrefix == "" {
		c.RedisKeyPrefix = "subtitle-translator"
	}
	if c.AdminUsername == "" {
		c.AdminUsername = "admin"
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.CaptchaClient == nil {
		c.CaptchaClient = &http.Client{
			Timeout: captchaTimeout,
			// The verification endpoints are fixed; never follow a redirect elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return c, nil
}
