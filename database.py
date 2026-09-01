"""Portable database schema and startup migrations for the web application."""

from __future__ import annotations

import os
from contextlib import contextmanager
from pathlib import Path
from typing import Iterator

from sqlalchemy import (
    Boolean,
    Column,
    ForeignKey,
    Index,
    Integer,
    MetaData,
    String,
    Table,
    Text,
    create_engine,
    delete,
    event,
    inspect,
    select,
    text,
    true,
    update,
)
from sqlalchemy.engine import Connection, Engine, make_url
from sqlalchemy.exc import SQLAlchemyError


NAMING_CONVENTION = {
    "ix": "ix_%(column_0_label)s",
    "uq": "uq_%(table_name)s_%(column_0_name)s",
    "ck": "ck_%(table_name)s_%(constraint_name)s",
    "fk": "fk_%(table_name)s_%(column_0_name)s_%(referred_table_name)s",
    "pk": "pk_%(table_name)s",
}
metadata = MetaData(naming_convention=NAMING_CONVENTION)

users = Table(
    "users",
    metadata,
    Column("id", String(32), primary_key=True),
    Column("username", String(64), nullable=False, unique=True),
    Column("password_hash", Text, nullable=False),
    Column("role", String(16), nullable=False, default="user"),
    Column(
        "theme",
        String(16),
        nullable=False,
        default="system",
        server_default=text("'system'"),
    ),
    Column("active", Boolean, nullable=False, default=True, server_default=true()),
    Column("token_version", Integer, nullable=False, default=0, server_default=text("0")),
    Column("failed_login_count", Integer, nullable=False, default=0, server_default=text("0")),
    Column("locked_until", String(40)),
    Column("created_at", String(40), nullable=False),
    Column("updated_at", String(40), nullable=False),
)

settings = Table(
    "settings",
    metadata,
    Column("name", String(128), primary_key=True),
    Column("value", Text, nullable=False),
    Column("updated_at", String(40), nullable=False),
)

jobs = Table(
    "jobs",
    metadata,
    Column("id", String(32), primary_key=True),
    Column(
        "user_id",
        String(32),
        ForeignKey("users.id", ondelete="SET NULL"),
        nullable=True,
    ),
    Column("filename", Text, nullable=False),
    Column("stored_name", Text, nullable=False),
    Column("status", String(24), nullable=False),
    Column("progress", Integer, nullable=False, default=0, server_default=text("0")),
    Column("stage", Text, nullable=False, default="", server_default=text("''")),
    Column("options", Text, nullable=False),
    Column("outputs", Text, nullable=False, default="[]", server_default=text("'[]'")),
    Column("error", Text),
    Column("created_at", String(40), nullable=False),
    Column("updated_at", String(40), nullable=False),
)
Index("ix_jobs_user_created", jobs.c.user_id, jobs.c.created_at)

rate_limit_buckets = Table(
    "rate_limit_buckets",
    metadata,
    Column("scope", String(64), primary_key=True),
    Column("window_started_at", String(40), nullable=False),
    Column("used", Integer, nullable=False, default=0, server_default=text("0")),
    Column("updated_at", String(40), nullable=False),
)

revoked_tokens = Table(
    "revoked_tokens",
    metadata,
    Column("jti", String(64), primary_key=True),
    Column("expires_at", String(40), nullable=False),
    Column("created_at", String(40), nullable=False),
)


def normalize_database_url(raw_url: str | None, sqlite_path: Path) -> str:
    """Return an explicit SQLAlchemy URL with supported production drivers."""
    if not raw_url:
        return f"sqlite:///{sqlite_path.as_posix()}"
    value = raw_url.strip()
    aliases = {
        "postgres://": "postgresql+pg8000://",
        "postgresql://": "postgresql+pg8000://",
        "mysql://": "mysql+pymysql://",
        "mariadb://": "mariadb+pymysql://",
    }
    for prefix, replacement in aliases.items():
        if value.startswith(prefix):
            return replacement + value[len(prefix):]
    return value


def create_database_engine(sqlite_path: Path) -> Engine:
    database_url = normalize_database_url(os.environ.get("DATABASE_URL"), sqlite_path)
    parsed = make_url(database_url)
    kwargs: dict = {"pool_pre_ping": True}
    if parsed.get_backend_name() == "sqlite":
        kwargs["connect_args"] = {"check_same_thread": False, "timeout": 30}
    engine = create_engine(database_url, **kwargs)

    if parsed.get_backend_name() == "sqlite":
        @event.listens_for(engine, "connect")
        def configure_sqlite(dbapi_connection, _connection_record) -> None:
            cursor = dbapi_connection.cursor()
            cursor.execute("PRAGMA foreign_keys=ON")
            cursor.execute("PRAGMA journal_mode=WAL")
            cursor.close()

    return engine


def _migrate_legacy_sqlite(engine: Engine) -> None:
    """Add ownership to databases created before authentication existed."""
    if engine.dialect.name != "sqlite":
        return
    inspector = inspect(engine)
    if not inspector.has_table("jobs"):
        return
    columns = {column["name"] for column in inspector.get_columns("jobs")}
    if "user_id" not in columns:
        with engine.begin() as connection:
            connection.execute(text("ALTER TABLE jobs ADD COLUMN user_id VARCHAR(32)"))
            connection.execute(
                text("CREATE INDEX IF NOT EXISTS ix_jobs_user_created "
                     "ON jobs (user_id, created_at)")
            )


def _migrate_user_theme(engine: Engine) -> None:
    """Add the account theme preference to databases created by older releases."""
    inspector = inspect(engine)
    if not inspector.has_table("users"):
        return
    columns = {column["name"] for column in inspector.get_columns("users")}
    if "theme" not in columns:
        try:
            with engine.begin() as connection:
                connection.execute(text(
                    "ALTER TABLE users ADD COLUMN theme "
                    "VARCHAR(16) NOT NULL DEFAULT 'system'"
                ))
        except SQLAlchemyError:
            # Another startup worker may have completed the same migration.
            refreshed = inspect(engine)
            refreshed_columns = {
                column["name"] for column in refreshed.get_columns("users")
            }
            if "theme" not in refreshed_columns:
                raise


def initialize_database(
    engine: Engine,
    defaults: dict[str, str],
    timestamp: str,
) -> None:
    _migrate_legacy_sqlite(engine)
    metadata.create_all(engine)
    _migrate_user_theme(engine)
    with engine.begin() as connection:
        existing = set(connection.execute(select(settings.c.name)).scalars())
        missing = [
            {"name": name, "value": value, "updated_at": timestamp}
            for name, value in defaults.items()
            if name not in existing
        ]
        if missing:
            connection.execute(settings.insert(), missing)
        connection.execute(
            update(jobs)
            .where(jobs.c.status.in_(("queued", "processing")))
            .values(
                status="failed",
                error="The server restarted before this job finished",
                updated_at=timestamp,
            )
        )
        connection.execute(
            delete(revoked_tokens).where(revoked_tokens.c.expires_at < timestamp)
        )
        connection.execute(
            update(jobs)
            .where(jobs.c.status == "canceling")
            .values(
                status="canceled",
                stage="Canceled",
                error=None,
                updated_at=timestamp,
            )
        )


@contextmanager
def transaction(engine: Engine) -> Iterator[Connection]:
    with engine.begin() as connection:
        yield connection


@contextmanager
def connection(engine: Engine) -> Iterator[Connection]:
    with engine.connect() as db_connection:
        yield db_connection
