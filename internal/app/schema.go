package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// recoveredJobError is stored on jobs that a previous process left active.
const recoveredJobError = "The server restarted before this job finished"

// The DDL is what SQLAlchemy generated for database.metadata on each dialect,
// so tables created by either implementation are identical (names, types,
// constraint names, and MySQL's parenthesized TEXT defaults).
type tableDDL struct {
	name    string
	create  string
	indexes []indexDDL
}

type indexDDL struct{ name, create string }

func schemaFor(d dialect) []tableDDL {
	boolean, trueDefault := "BOOLEAN", "1"
	switch d {
	case dialectPostgres:
		trueDefault = "true"
	case dialectMySQL:
		boolean, trueDefault = "BOOL", "true"
	}
	// MySQL puts DEFAULT after NOT NULL; the order is cosmetic elsewhere, but keep
	// SQLAlchemy's spelling per dialect.
	def := func(value string) string {
		if d == dialectMySQL {
			return "NOT NULL DEFAULT " + value
		}
		return "DEFAULT " + value + " NOT NULL"
	}
	role := "role"
	if d == dialectMySQL {
		role = "`role`"
	}
	return []tableDDL{
		{name: "cache_revisions", create: `CREATE TABLE cache_revisions (
	name VARCHAR(32) NOT NULL,
	revision VARCHAR(32) NOT NULL,
	CONSTRAINT pk_cache_revisions PRIMARY KEY (name)
)`},
		{name: "rate_limit_buckets", create: `CREATE TABLE rate_limit_buckets (
	scope VARCHAR(64) NOT NULL,
	window_started_at VARCHAR(40) NOT NULL,
	used INTEGER ` + def("0") + `,
	updated_at VARCHAR(40) NOT NULL,
	CONSTRAINT pk_rate_limit_buckets PRIMARY KEY (scope)
)`},
		{name: "revoked_tokens", create: `CREATE TABLE revoked_tokens (
	jti VARCHAR(64) NOT NULL,
	expires_at VARCHAR(40) NOT NULL,
	created_at VARCHAR(40) NOT NULL,
	CONSTRAINT pk_revoked_tokens PRIMARY KEY (jti)
)`},
		{name: "settings", create: `CREATE TABLE settings (
	name VARCHAR(128) NOT NULL,
	value TEXT NOT NULL,
	updated_at VARCHAR(40) NOT NULL,
	CONSTRAINT pk_settings PRIMARY KEY (name)
)`},
		{name: "users", create: `CREATE TABLE users (
	id VARCHAR(32) NOT NULL,
	username VARCHAR(64) NOT NULL,
	password_hash TEXT NOT NULL,
	` + role + ` VARCHAR(16) NOT NULL,
	theme VARCHAR(16) ` + def("'system'") + `,
	active ` + boolean + ` ` + def(trueDefault) + `,
	token_version INTEGER ` + def("0") + `,
	failed_login_count INTEGER ` + def("0") + `,
	locked_until VARCHAR(40),
	created_at VARCHAR(40) NOT NULL,
	updated_at VARCHAR(40) NOT NULL,
	CONSTRAINT pk_users PRIMARY KEY (id),
	CONSTRAINT uq_users_username UNIQUE (username)
)`},
		{name: "jobs", create: `CREATE TABLE jobs (
	id VARCHAR(32) NOT NULL,
	user_id VARCHAR(32),
	filename TEXT NOT NULL,
	stored_name TEXT NOT NULL,
	status VARCHAR(24) NOT NULL,
	progress INTEGER ` + def("0") + `,
	stage TEXT ` + def("('')") + `,
	options TEXT NOT NULL,
	outputs TEXT ` + def("('[]')") + `,
	error TEXT,
	warning TEXT,
	created_at VARCHAR(40) NOT NULL,
	updated_at VARCHAR(40) NOT NULL,
	CONSTRAINT pk_jobs PRIMARY KEY (id),
	CONSTRAINT fk_jobs_user_id_users FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE SET NULL
)`, indexes: []indexDDL{{"ix_jobs_user_created", "CREATE INDEX ix_jobs_user_created ON jobs (user_id, created_at)"}}},
		{name: "mfa_accounts", create: `CREATE TABLE mfa_accounts (
	user_id VARCHAR(32) NOT NULL,
	method VARCHAR(16) NOT NULL,
	secret TEXT NOT NULL,
	email VARCHAR(254) NOT NULL,
	last_step INTEGER NOT NULL,
	recovery_hashes TEXT NOT NULL,
	failures INTEGER NOT NULL,
	locked_until INTEGER NOT NULL,
	next_send INTEGER NOT NULL,
	send_window INTEGER NOT NULL,
	send_count INTEGER NOT NULL,
	manage_failures INTEGER NOT NULL,
	manage_locked_until INTEGER NOT NULL,
	CONSTRAINT pk_mfa_accounts PRIMARY KEY (user_id),
	CONSTRAINT fk_mfa_accounts_user_id_users FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
)`},
		{name: "mfa_challenges", create: `CREATE TABLE mfa_challenges (
	id VARCHAR(64) NOT NULL,
	user_id VARCHAR(32) NOT NULL,
	version INTEGER NOT NULL,
	purpose VARCHAR(16) NOT NULL,
	method VARCHAR(16) NOT NULL,
	secret TEXT NOT NULL,
	email VARCHAR(254) NOT NULL,
	code_hash VARCHAR(64) NOT NULL,
	expires INTEGER NOT NULL,
	transport VARCHAR(16) NOT NULL,
	CONSTRAINT pk_mfa_challenges PRIMARY KEY (id),
	CONSTRAINT fk_mfa_challenges_user_id_users FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
)`, indexes: []indexDDL{
			{"ix_mfa_challenges_user_id", "CREATE INDEX ix_mfa_challenges_user_id ON mfa_challenges (user_id)"},
			{"ix_mfa_challenges_expires", "CREATE INDEX ix_mfa_challenges_expires ON mfa_challenges (expires)"},
		}},
	}
}

// addedColumns lists columns added after a table's first release. Keep MySQL's
// rule in mind: no literal DEFAULT on TEXT columns.
var addedColumns = []struct{ table, column, statement string }{
	{"users", "theme", "ALTER TABLE users ADD COLUMN theme VARCHAR(16) NOT NULL DEFAULT 'system'"},
	{"jobs", "warning", "ALTER TABLE jobs ADD COLUMN warning TEXT"},
	{"mfa_accounts", "manage_failures",
		"ALTER TABLE mfa_accounts ADD COLUMN manage_failures INTEGER NOT NULL DEFAULT 0"},
	{"mfa_accounts", "manage_locked_until",
		"ALTER TABLE mfa_accounts ADD COLUMN manage_locked_until INTEGER NOT NULL DEFAULT 0"},
}

func (d *database) tableExists(ctx context.Context, table string) (bool, error) {
	var query string
	switch d.dialect {
	case dialectSQLite:
		query = "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?"
	case dialectPostgres:
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?"
	default:
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?"
	}
	count, err := d.conn().scalarInt(ctx, query, table)
	return count > 0, err
}

func (d *database) columns(ctx context.Context, table string) (map[string]bool, error) {
	var rows *sql.Rows
	var err error
	c := d.conn()
	switch d.dialect {
	case dialectSQLite:
		// PRAGMA table_info cannot take a bound table name; table is a constant.
		rows, err = d.QueryContext(ctx, fmt.Sprintf("SELECT name FROM pragma_table_info('%s')", table))
	case dialectPostgres:
		rows, err = c.query(ctx, "SELECT column_name FROM information_schema.columns "+
			"WHERE table_schema = current_schema() AND table_name = ?", table)
	default:
		rows, err = c.query(ctx, "SELECT column_name FROM information_schema.columns "+
			"WHERE table_schema = DATABASE() AND table_name = ?", table)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result[strings.ToLower(name)] = true
	}
	return result, rows.Err()
}

func (d *database) indexExists(ctx context.Context, table, index string) (bool, error) {
	var query string
	switch d.dialect {
	case dialectSQLite:
		query = "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND name = ?"
	case dialectPostgres:
		query = "SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND tablename = ? AND indexname = ?"
	default:
		query = "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() " +
			"AND table_name = ? AND index_name = ?"
	}
	count, err := d.conn().scalarInt(ctx, query, table, index)
	return count > 0, err
}

// addColumnIfMissing runs statement unless the column exists, tolerating a
// concurrent startup that added it first. A missing table is left to createTables.
func (d *database) addColumnIfMissing(ctx context.Context, table, column, statement string) (bool, error) {
	exists, err := d.tableExists(ctx, table)
	if err != nil || !exists {
		return false, err
	}
	columns, err := d.columns(ctx, table)
	if err != nil || columns[column] {
		return false, err
	}
	if _, err := d.ExecContext(ctx, statement); err != nil {
		columns, checkErr := d.columns(ctx, table)
		if checkErr != nil || !columns[column] {
			return false, err
		}
	}
	return true, nil
}

// migrateLegacySQLite adds ownership to databases created before authentication.
func (d *database) migrateLegacySQLite(ctx context.Context) error {
	if d.dialect != dialectSQLite {
		return nil
	}
	added, err := d.addColumnIfMissing(ctx, "jobs", "user_id", "ALTER TABLE jobs ADD COLUMN user_id VARCHAR(32)")
	if err != nil || !added {
		return err
	}
	_, err = d.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS ix_jobs_user_created ON jobs (user_id, created_at)")
	return err
}

// createTables mirrors metadata.create_all: create each missing table and its indexes.
func (d *database) createTables(ctx context.Context) error {
	for _, table := range schemaFor(d.dialect) {
		exists, err := d.tableExists(ctx, table.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := d.ExecContext(ctx, table.create); err != nil {
				// A concurrent startup may have created it; re-check before failing.
				if again, checkErr := d.tableExists(ctx, table.name); checkErr != nil || !again {
					return err
				}
			}
		}
		for _, index := range table.indexes {
			present, err := d.indexExists(ctx, table.name, index.name)
			if err != nil {
				return err
			}
			if present {
				continue
			}
			if _, err := d.ExecContext(ctx, index.create); err != nil {
				if again, checkErr := d.indexExists(ctx, table.name, index.name); checkErr != nil || !again {
					return err
				}
			}
		}
	}
	return nil
}

func (d *database) migrateAddedColumns(ctx context.Context) error {
	for _, item := range addedColumns {
		if _, err := d.addColumnIfMissing(ctx, item.table, item.column, item.statement); err != nil {
			return err
		}
	}
	return nil
}

// seedAndRecover mirrors database._seed_and_recover.
func (d *database) seedAndRecover(ctx context.Context, defaults map[string]string, timestamp string,
	recoverJobs bool) error {
	return d.tx(ctx, func(c conn) error {
		rows, err := c.query(ctx, "SELECT name FROM cache_revisions")
		if err != nil {
			return err
		}
		existing := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			existing[name] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, name := range []string{"settings", "jobs"} {
			if existing[name] {
				if err := bumpCacheRevision(ctx, c, name); err != nil {
					return err
				}
			} else if _, err := c.exec(ctx, "INSERT INTO cache_revisions (name, revision) VALUES (?, ?)",
				name, randomHex(16)); err != nil {
				return err
			}
		}
		stored, err := c.query(ctx, "SELECT name FROM settings")
		if err != nil {
			return err
		}
		present := map[string]bool{}
		for stored.Next() {
			var name string
			if err := stored.Scan(&name); err != nil {
				stored.Close()
				return err
			}
			present[name] = true
		}
		stored.Close()
		if err := stored.Err(); err != nil {
			return err
		}
		for _, name := range sortedKeys(defaults) {
			if present[name] {
				continue
			}
			if _, err := c.exec(ctx, "INSERT INTO settings (name, value, updated_at) VALUES (?, ?, ?)",
				name, defaults[name], timestamp); err != nil {
				return err
			}
		}
		if _, err := c.exec(ctx, "DELETE FROM revoked_tokens WHERE expires_at < ?", timestamp); err != nil {
			return err
		}
		if !recoverJobs {
			return nil
		}
		if _, err := c.exec(ctx, "UPDATE jobs SET status = 'failed', stage = 'Translation failed', "+
			"error = ?, updated_at = ? WHERE status IN ('queued', 'processing')",
			recoveredJobError, timestamp); err != nil {
			return err
		}
		_, err = c.exec(ctx, "UPDATE jobs SET status = 'canceled', stage = 'Canceled', error = NULL, "+
			"updated_at = ? WHERE status = 'canceling'", timestamp)
		return err
	})
}

// initialize mirrors database.initialize_database.
func (d *database) initialize(ctx context.Context, defaults map[string]string, timestamp string,
	recoverJobs bool) error {
	steps := []func() error{
		func() error { return d.migrateLegacySQLite(ctx) },
		func() error { return d.createTables(ctx) },
		func() error { return d.migrateAddedColumns(ctx) },
		func() error { return d.seedAndRecover(ctx, defaults, timestamp, recoverJobs) },
	}
	for _, step := range steps {
		if err := retryDatabaseRace(ctx, step); err != nil {
			return err
		}
	}
	return nil
}

// bumpCacheRevision changes a Redis revision marker in the caller's transaction.
func bumpCacheRevision(ctx context.Context, c conn, name string) error {
	_, err := c.exec(ctx, "UPDATE cache_revisions SET revision = ? WHERE name = ?", randomHex(16), name)
	return err
}
