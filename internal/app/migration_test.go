package app

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A database written by the pre-authentication releases: jobs without
// user_id/warning, users without theme, MFA without the management budget.
const legacySchema = `
CREATE TABLE settings (name VARCHAR(128) NOT NULL PRIMARY KEY, value TEXT NOT NULL, updated_at VARCHAR(40) NOT NULL);
CREATE TABLE jobs (id VARCHAR(32) NOT NULL PRIMARY KEY, filename TEXT NOT NULL, stored_name TEXT NOT NULL,
  status VARCHAR(24) NOT NULL, progress INTEGER DEFAULT 0 NOT NULL, stage TEXT DEFAULT ('') NOT NULL,
  options TEXT NOT NULL, outputs TEXT DEFAULT ('[]') NOT NULL, error TEXT,
  created_at VARCHAR(40) NOT NULL, updated_at VARCHAR(40) NOT NULL);
CREATE TABLE users (id VARCHAR(32) NOT NULL PRIMARY KEY, username VARCHAR(64) NOT NULL UNIQUE,
  password_hash TEXT NOT NULL, role VARCHAR(16) NOT NULL, active BOOLEAN DEFAULT 1 NOT NULL,
  token_version INTEGER DEFAULT 0 NOT NULL, failed_login_count INTEGER DEFAULT 0 NOT NULL,
  locked_until VARCHAR(40), created_at VARCHAR(40) NOT NULL, updated_at VARCHAR(40) NOT NULL);
CREATE TABLE mfa_accounts (user_id VARCHAR(32) NOT NULL PRIMARY KEY, method VARCHAR(16) NOT NULL,
  secret TEXT NOT NULL, email VARCHAR(254) NOT NULL, last_step INTEGER NOT NULL, recovery_hashes TEXT NOT NULL,
  failures INTEGER NOT NULL, locked_until INTEGER NOT NULL, next_send INTEGER NOT NULL,
  send_window INTEGER NOT NULL, send_count INTEGER NOT NULL);
INSERT INTO jobs VALUES ('old', 'a.srt', 'source.srt', 'completed', 100, 'Ready to download', '{}', '[]', NULL,
  '2025-01-01T00:00:00+00:00', '2025-01-01T00:00:00+00:00');
INSERT INTO settings VALUES ('anthropic_api_key', 'legacy-plaintext', '2025-01-01T00:00:00+00:00');
`

func TestLegacySQLiteDatabaseIsMigratedAndClaimedByTheFirstAdmin(t *testing.T) {
	if testDatabaseURL() != "" {
		t.Skip("SQLite-specific migration")
	}
	dir := t.TempDir()
	db, err := openSQLite(filepath.Join(dir, "app.db"), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range strings.Split(legacySchema, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	db.Close()
	clock := &testClock{}
	env := startEnv(t, Config{DataDir: dir, JWTSecret: testJWTSecret, Debug: true, Env: map[string]string{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clock.now}, clock)
	for table, columns := range map[string][]string{
		"jobs": {"user_id", "warning"}, "users": {"theme"}, "mfa_accounts": {"manage_failures", "manage_locked_until"},
	} {
		present, err := env.server.db.columns(context.Background(), table)
		if err != nil {
			t.Fatal(err)
		}
		for _, column := range columns {
			if !present[column] {
				t.Fatalf("%s.%s not added", table, column)
			}
		}
	}
	if !strings.HasPrefix(env.text("SELECT value FROM settings WHERE name = 'anthropic_api_key'"), secretPrefix) {
		t.Fatal("legacy plaintext secret not encrypted")
	}
	admin := env.setupAdmin()
	if list := admin.get("/api/jobs").json(); list["total"] != float64(1) {
		t.Fatalf("first admin did not claim the legacy job: %v", list)
	}
	for _, table := range []string{"cache_revisions", "rate_limit_buckets", "revoked_tokens", "mfa_challenges"} {
		if exists, _ := env.server.db.tableExists(context.Background(), table); !exists {
			t.Fatalf("table %s not created", table)
		}
	}
}

func TestConcurrentStartupsInitializeAFreshDatabase(t *testing.T) {
	dir := t.TempDir()
	url := testDatabaseURL()
	if url != "" {
		resetExternalDatabase(t, url)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := openDatabase(url, dir)
			if err != nil {
				errs[i] = err
				return
			}
			defer db.Close()
			defaults, _ := settingDefaults(map[string]string{})
			errs[i] = db.initialize(context.Background(), defaults, isoSeconds(testNow()), true)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent initialization: %v", err)
		}
	}
	db, err := openDatabase(url, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count, err := db.conn().scalarInt(context.Background(), "SELECT COUNT(*) FROM cache_revisions")
	if err != nil || count != 2 {
		t.Fatalf("cache revisions %d %v", count, err)
	}
}

func TestDatabaseURLTLSSemantics(t *testing.T) {
	for _, mode := range []string{"prefer", "allow", "bogus"} {
		if _, err := openDatabase("postgresql://u:p@db.example/app?sslmode="+mode, t.TempDir()); err == nil {
			t.Errorf("sslmode=%s accepted", mode)
		}
	}
	if _, err := openDatabase("mysql://u:p@db.example/app?ssl-mode=PREFERRED", t.TempDir()); err == nil {
		t.Error("ssl-mode=PREFERRED accepted")
	}
	if _, err := openDatabase("mysql://u:p@db.example/app?unknown=1", t.TempDir()); err == nil {
		t.Error("unknown MySQL parameter accepted")
	}
	// require encrypts without verification; verify-full checks the host name.
	config, err := buildTLSConfig(postgresSSLModes["require"], "db.example", "", "", "")
	if err != nil || !config.InsecureSkipVerify || config.VerifyConnection != nil {
		t.Fatalf("require: %+v %v", config, err)
	}
	config, err = buildTLSConfig(postgresSSLModes["verify-ca"], "db.example", "", "", "")
	if err != nil || !config.InsecureSkipVerify || config.VerifyConnection == nil {
		t.Fatalf("verify-ca: %v", err)
	}
	config, err = buildTLSConfig(postgresSSLModes["verify-full"], "db.example", "", "", "")
	if err != nil || config.InsecureSkipVerify || config.ServerName != "db.example" {
		t.Fatalf("verify-full: %v", err)
	}
	if _, err := buildTLSConfig(postgresSSLModes["verify-full"], "db", filepath.Join(t.TempDir(), "missing.pem"), "", ""); err == nil {
		t.Fatal("missing CA file accepted")
	}
	for _, raw := range []string{"postgresql://u:p@db.example/app?sslmode=verify-full", "postgres://u@h/d",
		"postgresql+pg8000://u@h/d?sslmode=disable", "mariadb+pymysql://u:p@h:3307/d?ssl-mode=REQUIRED&charset=utf8mb4",
		"mysql://u:p@h/d?ssl_mode=verify_identity"} {
		db, err := openDatabase(raw, t.TempDir())
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		db.Close()
	}
	if _, err := openDatabase("oracle://x", t.TempDir()); err == nil {
		t.Fatal("unsupported backend accepted")
	}
}

func TestMySQLDDLKeepsTheTextDefaultRule(t *testing.T) {
	for _, table := range schemaFor(dialectMySQL) {
		for _, line := range strings.Split(table.create, "\n") {
			if strings.Contains(line, " TEXT ") && strings.Contains(line, "DEFAULT") && !strings.Contains(line, "DEFAULT (") {
				t.Fatalf("literal TEXT default on MySQL: %s", line)
			}
		}
	}
	for _, column := range addedColumns {
		if strings.Contains(column.statement, " TEXT ") && strings.Contains(column.statement, "DEFAULT '") {
			t.Fatalf("literal TEXT default in %s", column.statement)
		}
	}
}

func TestRawInsertsGetTheExpressionDefaults(t *testing.T) {
	env := newEnv(t)
	env.exec("INSERT INTO jobs (id, filename, stored_name, status, options, created_at, updated_at) " +
		"VALUES ('raw', 'a.srt', 'source.srt', 'completed', '{}', 'x', 'x')")
	if env.text("SELECT outputs FROM jobs WHERE id = 'raw'") != "[]" || env.text("SELECT stage FROM jobs WHERE id = 'raw'") != "" {
		t.Fatal("expression defaults missing")
	}
	var progress sql.NullInt64
	_ = env.server.db.conn().queryRow(context.Background(), "SELECT progress FROM jobs WHERE id = 'raw'").Scan(&progress)
	if progress.Int64 != 0 || !progress.Valid {
		t.Fatal("progress default missing")
	}
}

func TestUnreadableUnrelatedSecretDoesNotBreakLoginOrJobs(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	env.exec("INSERT INTO settings (name, value, updated_at) VALUES ('google_api_key', 'enc:v1:broken', 'x')")
	env.login("admin")
	id := jobIDs(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}))[0]
	if job := env.waitForJob(admin, id); job["status"] != "completed" {
		t.Fatalf("job %v", job)
	}
}
