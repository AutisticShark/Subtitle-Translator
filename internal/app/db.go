package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
	dialectMySQL
)

func (d dialect) String() string {
	switch d {
	case dialectPostgres:
		return "postgresql"
	case dialectMySQL:
		return "mysql"
	}
	return "sqlite"
}

// database wraps *sql.DB with the dialect, so queries can be written once with
// "?" placeholders.
type database struct {
	*sql.DB
	dialect dialect
	// identity is the normalized connection URL; it namespaces Redis keys.
	identity string
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// conn binds a querier to the dialect so helpers can rebind placeholders.
type conn struct {
	q       querier
	dialect dialect
}

func (c conn) rebind(query string) string {
	if c.dialect != dialectPostgres {
		return query
	}
	var out strings.Builder
	out.Grow(len(query) + 8)
	n := 0
	inString := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			inString = !inString
		}
		if ch == '?' && !inString {
			n++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(n))
			continue
		}
		out.WriteByte(ch)
	}
	return out.String()
}

func (c conn) exec(ctx context.Context, query string, args ...any) (int64, error) {
	result, err := c.q.ExecContext(ctx, c.rebind(query), args...)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return affected, nil
}

func (c conn) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return c.q.QueryContext(ctx, c.rebind(query), args...)
}

func (c conn) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return c.q.QueryRowContext(ctx, c.rebind(query), args...)
}

// scalarInt runs a single-value integer query.
func (c conn) scalarInt(ctx context.Context, query string, args ...any) (int64, error) {
	var value sql.NullInt64
	if err := c.queryRow(ctx, query, args...).Scan(&value); err != nil {
		return 0, err
	}
	return value.Int64, nil
}

// placeholders returns "?, ?, ?" for n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func stringArgs(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

func (d *database) conn() conn { return conn{q: d.DB, dialect: d.dialect} }

// tx runs fn in one transaction, committing when it returns nil. On SQLite the
// transaction takes the write lock at BEGIN (_txlock=immediate); on the server
// databases the first write of each serializing path takes its row lock.
//
// A serialization conflict (deadlock, or MariaDB's snapshot-isolation error
// 1020 "Record has changed since last read") rolls back and reruns fn, which
// must therefore only touch the database and reassign, not accumulate, state.
func (d *database) tx(ctx context.Context, fn func(c conn) error) error {
	const attempts = 6
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if err = d.txOnce(ctx, fn); err == nil || !isSerializationConflict(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(5*(attempt+1)) * time.Millisecond):
		}
	}
	return err
}

// isSerializationConflict reports errors that a retried transaction resolves.
func isSerializationConflict(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1213 || myErr.Number == 1020
	}
	return false
}

func (d *database) txOnce(ctx context.Context, fn func(c conn) error) error {
	transaction, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Rollback()
		}
	}()
	if err := fn(conn{q: transaction, dialect: d.dialect}); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// errRollback aborts a transaction without being reported as a failure; the
// caller has already decided the response.
var errRollback = errors.New("rollback")

// isIntegrityError reports constraint violations (duplicate keys, foreign keys).
func isIntegrityError(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code()&0xff == sqlite3.SQLITE_CONSTRAINT
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "23")
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		switch myErr.Number {
		case 1022, 1062, 1169, 1216, 1217, 1451, 1452, 1557, 1586, 1761, 1762, 1859:
			return true
		}
	}
	return false
}

// openDatabase mirrors database.create_database_engine: SQLite in dataDir by
// default, or the DATABASE_URL backend with libpq/MySQL-client TLS semantics.
func openDatabase(rawURL, dataDir string) (*database, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		absolute, err := filepath.Abs(filepath.Join(dataDir, "app.db"))
		if err != nil {
			return nil, err
		}
		return openSQLite(absolute, "sqlite:///"+filepath.ToSlash(absolute))
	}
	scheme, rest, ok := strings.Cut(rawURL, "://")
	if !ok {
		return nil, errors.New("DATABASE_URL must be a URL such as postgresql://user:password@host/database")
	}
	backend, _, _ := strings.Cut(strings.ToLower(scheme), "+")
	switch backend {
	case "sqlite":
		// sqlite:///relative.db and sqlite:////absolute/path.db, as in SQLAlchemy.
		file := strings.TrimPrefix(rest, "/")
		if before, _, found := strings.Cut(file, "?"); found {
			file = before
		}
		if file == "" || file == ":memory:" {
			return nil, errors.New("DATABASE_URL must name a SQLite database file")
		}
		absolute, err := filepath.Abs(filepath.FromSlash(file))
		if err != nil {
			return nil, err
		}
		return openSQLite(absolute, rawURL)
	case "postgres", "postgresql":
		return openPostgres(rest, rawURL)
	case "mysql", "mariadb":
		return openMySQL(rest, rawURL)
	}
	return nil, fmt.Errorf("DATABASE_URL backend %q is not supported; use sqlite, postgresql, mysql, or mariadb", scheme)
}

func openSQLite(path, identity string) (*database, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return &database{DB: db, dialect: dialectSQLite, identity: identity}, nil
}

// tlsMode is (encrypt, verify certificate chain, verify host name).
type tlsMode struct{ encrypt, verifyChain, verifyHost bool }

// The opportunistic prefer/allow/PREFERRED modes silently fall back to
// plaintext in libpq and the MySQL client, so they are rejected rather than guessed.
var postgresSSLModes = map[string]tlsMode{
	"disable":     {false, false, false},
	"require":     {true, false, false},
	"verify-ca":   {true, true, false},
	"verify-full": {true, true, true},
}

var mysqlSSLModes = map[string]tlsMode{
	"disabled":        {false, false, false},
	"required":        {true, false, false},
	"verify_ca":       {true, true, false},
	"verify_identity": {true, true, true},
}

// popQuery removes every spelling of a parameter and returns its last value.
func popQuery(query url.Values, names ...string) (string, bool) {
	value, found := "", false
	for key, values := range query {
		for _, name := range names {
			if strings.ToLower(key) == name {
				if len(values) > 0 {
					value = values[len(values)-1]
				}
				found = true
				delete(query, key)
			}
		}
	}
	return value, found
}

func supportedModes(modes map[string]tlsMode) string {
	order := []string{"disable", "require", "verify-ca", "verify-full", "disabled", "required", "verify_ca", "verify_identity"}
	var names []string
	for _, name := range order {
		if _, ok := modes[name]; ok {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

// buildTLSConfig follows ssl.create_default_context: the CA file replaces the
// system roots, "require" encrypts without verification, verify-ca checks the
// chain only, and verify-full also checks the host name.
func buildTLSConfig(mode tlsMode, host, caFile, certFile, keyFile string) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	var roots *x509.CertPool
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read the database CA file: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("the database CA file contains no PEM certificates")
		}
		config.RootCAs = roots
	}
	if certFile != "" {
		if keyFile == "" {
			keyFile = certFile
		}
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("cannot load the database client certificate: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	switch {
	case !mode.verifyChain:
		config.InsecureSkipVerify = true
	case !mode.verifyHost:
		// Verify the chain against the roots but not the host name.
		config.InsecureSkipVerify = true
		config.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("the database server sent no certificate")
			}
			options := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
			for _, certificate := range state.PeerCertificates[1:] {
				options.Intermediates.AddCert(certificate)
			}
			_, err := state.PeerCertificates[0].Verify(options)
			return err
		}
	}
	return config, nil
}

func openPostgres(rest, identity string) (*database, error) {
	parsed, err := url.Parse("postgresql://" + rest)
	if err != nil {
		return nil, errors.New("DATABASE_URL is not a valid PostgreSQL URL")
	}
	query := parsed.Query()
	mode, hasMode := popQuery(query, "sslmode")
	caFile, _ := popQuery(query, "sslrootcert")
	certFile, hasCert := popQuery(query, "sslcert")
	keyFile, hasKey := popQuery(query, "sslkey")
	if hasMode {
		mode = strings.ToLower(strings.TrimSpace(mode))
	} else if caFile != "" || hasCert || hasKey {
		// libpq's default (prefer) cannot be expressed without a fallback. A
		// certificate file signals that TLS is intended, so require it.
		mode = "require"
		hasMode = true
	}
	// libpq verifies the chain for sslmode=require when a root certificate is supplied.
	if mode == "require" && caFile != "" {
		mode = "verify-ca"
	}
	var selected tlsMode
	if hasMode {
		var ok bool
		if selected, ok = postgresSSLModes[mode]; !ok {
			return nil, fmt.Errorf("DATABASE_URL sslmode=%s is not supported; use one of: %s",
				mode, supportedModes(postgresSSLModes))
		}
	}
	// pgx must not negotiate TLS on its own (its default "prefer" falls back to
	// plaintext); the mode above decides alone.
	query.Set("sslmode", "disable")
	parsed.RawQuery = query.Encode()
	config, err := pgx.ParseConfig(parsed.String())
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL is not a valid PostgreSQL URL: %w", err)
	}
	config.Fallbacks = nil
	if selected.encrypt {
		tlsConfig, err := buildTLSConfig(selected, config.Host, caFile, certFile, keyFile)
		if err != nil {
			return nil, err
		}
		config.TLSConfig = tlsConfig
	}
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(16)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return &database{DB: db, dialect: dialectPostgres, identity: identity}, nil
}

func openMySQL(rest, identity string) (*database, error) {
	parsed, err := url.Parse("mysql://" + rest)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("DATABASE_URL is not a valid MySQL or MariaDB URL")
	}
	query := parsed.Query()
	mode, hasMode := popQuery(query, "ssl-mode", "ssl_mode")
	caFile, hasCA := popQuery(query, "ssl-ca", "ssl_ca")
	certFile, hasCert := popQuery(query, "ssl-cert", "ssl_cert")
	keyFile, hasKey := popQuery(query, "ssl-key", "ssl_key")
	var selected tlsMode
	if hasMode {
		mode = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(mode)), "-", "_")
		var ok bool
		if selected, ok = mysqlSSLModes[mode]; !ok {
			return nil, fmt.Errorf("DATABASE_URL ssl-mode=%s is not supported; use one of: %s",
				mode, supportedModes(mysqlSSLModes))
		}
	} else if hasCA {
		// PyMySQL: a CA verifies the chain and the host name.
		selected = tlsMode{true, true, true}
	} else if hasCert || hasKey {
		// PyMySQL: client certificates without a CA encrypt without verification.
		selected = tlsMode{true, false, false}
	}
	config := mysql.NewConfig()
	config.Net = "tcp"
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "3306"
	}
	config.Addr = net.JoinHostPort(host, port)
	config.User = parsed.User.Username()
	config.Passwd, _ = parsed.User.Password()
	config.DBName = strings.TrimPrefix(parsed.Path, "/")
	config.ParseTime = false
	config.InterpolateParams = false
	config.MultiStatements = false
	// Rows affected counts matched rows, as SQLAlchemy's MySQL dialects request
	// (CLIENT_FOUND_ROWS): conditional updates are checked by row count, and an
	// update that rewrites identical values must still count as applied.
	config.ClientFoundRows = true
	config.Params = map[string]string{}
	if err := config.Apply(mysql.Charset("utf8mb4", "utf8mb4_unicode_ci")); err != nil {
		return nil, err
	}
	for key, values := range query {
		if len(values) == 0 {
			continue
		}
		value := values[len(values)-1]
		switch strings.ToLower(key) {
		case "charset":
			if err := config.Apply(mysql.Charset(value, "")); err != nil {
				return nil, err
			}
		case "connect_timeout":
			seconds, err := strconv.Atoi(value)
			if err != nil {
				return nil, errors.New("DATABASE_URL connect_timeout must be whole seconds")
			}
			config.Timeout = time.Duration(seconds) * time.Second
		case "read_timeout":
			seconds, err := strconv.Atoi(value)
			if err != nil {
				return nil, errors.New("DATABASE_URL read_timeout must be whole seconds")
			}
			config.ReadTimeout = time.Duration(seconds) * time.Second
		case "write_timeout":
			seconds, err := strconv.Atoi(value)
			if err != nil {
				return nil, errors.New("DATABASE_URL write_timeout must be whole seconds")
			}
			config.WriteTimeout = time.Duration(seconds) * time.Second
		default:
			return nil, fmt.Errorf("DATABASE_URL parameter %q is not supported for MySQL or MariaDB", key)
		}
	}
	if selected.encrypt {
		tlsConfig, err := buildTLSConfig(selected, host, caFile, certFile, keyFile)
		if err != nil {
			return nil, err
		}
		config.TLS = tlsConfig
	}
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(16)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return &database{DB: db, dialect: dialectMySQL, identity: identity}, nil
}

// retryDatabaseRace mirrors database.retry_database_race: idempotent startup
// steps are retried when a concurrent process races them.
func retryDatabaseRace(ctx context.Context, operation func() error) error {
	const attempts = 8
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if err = operation(); err == nil {
			return nil
		}
		if attempt == attempts-1 {
			break
		}
		delay := min(time.Duration(50*(1<<attempt))*time.Millisecond, time.Second)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return err
}
