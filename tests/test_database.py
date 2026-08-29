from pathlib import Path
import sqlite3
import tempfile
from contextlib import closing

from sqlalchemy.dialects import mysql, postgresql, sqlite
from sqlalchemy.schema import CreateIndex, CreateTable

from database import (
    create_database_engine,
    initialize_database,
    jobs,
    metadata,
    normalize_database_url,
)


def test_database_url_aliases_select_installed_drivers():
    path = Path("C:/temporary/app.db")
    assert normalize_database_url(None, path) == "sqlite:///C:/temporary/app.db"
    assert normalize_database_url(
        "postgresql://user:pass@db/app", path
    ) == "postgresql+pg8000://user:pass@db/app"
    assert normalize_database_url(
        "mariadb://user:pass@db/app", path
    ) == "mariadb+pymysql://user:pass@db/app"
    assert normalize_database_url(
        "mysql://user:pass@db/app", path
    ) == "mysql+pymysql://user:pass@db/app"


def test_schema_compiles_for_sqlite_mariadb_and_postgresql():
    dialects = (sqlite.dialect(), mysql.dialect(), postgresql.dialect())
    for dialect in dialects:
        for table in metadata.sorted_tables:
            assert str(CreateTable(table).compile(dialect=dialect))
        for index in jobs.indexes:
            assert str(CreateIndex(index).compile(dialect=dialect))


def test_legacy_sqlite_jobs_gain_ownership_without_losing_records():
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / "legacy.db"
        with closing(sqlite3.connect(path)) as legacy_connection:
            legacy_connection.executescript("""
                CREATE TABLE settings (
                    name TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL
                );
                CREATE TABLE jobs (
                    id TEXT PRIMARY KEY,
                    filename TEXT NOT NULL,
                    stored_name TEXT NOT NULL,
                    status TEXT NOT NULL,
                    progress INTEGER NOT NULL DEFAULT 0,
                    stage TEXT NOT NULL DEFAULT '',
                    options TEXT NOT NULL,
                    outputs TEXT NOT NULL DEFAULT '[]',
                    error TEXT,
                    created_at TEXT NOT NULL,
                    updated_at TEXT NOT NULL
                );
                INSERT INTO jobs (
                    id, filename, stored_name, status, options, created_at, updated_at
                ) VALUES (
                    'legacy', 'legacy.srt', 'source.srt', 'queued', '{}',
                    '2026-01-01T00:00:00+00:00', '2026-01-01T00:00:00+00:00'
                );
            """)
            legacy_connection.commit()
        engine = create_database_engine(path)
        try:
            initialize_database(engine, {"default_provider": "anthropic"},
                                "2026-01-02T00:00:00+00:00")
            with closing(sqlite3.connect(path)) as verification_connection:
                columns = {
                    row[1] for row in verification_connection.execute("PRAGMA table_info(jobs)")
                }
                tables = {
                    row[0] for row in verification_connection.execute(
                        "SELECT name FROM sqlite_master WHERE type='table'"
                    )
                }
                row = verification_connection.execute(
                    "SELECT id, user_id, status FROM jobs WHERE id='legacy'"
                ).fetchone()
            assert "user_id" in columns
            assert "rate_limit_buckets" in tables
            assert row == ("legacy", None, "failed")
        finally:
            engine.dispose()
