"""Portable database schema and startup migrations for the web application."""

from __future__ import annotations

import os
import ssl
import uuid
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
from sqlalchemy.engine import URL, Connection, Engine, make_url
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

# Separate tables allow existing accounts to adopt MFA without rewriting users.
# All MFA operations first lock the associated users row, including on SQLite.
mfa_accounts = Table(
    "mfa_accounts", metadata,
    Column("user_id", String(32), ForeignKey("users.id", ondelete="CASCADE"), primary_key=True),
    Column("method", String(16), nullable=False, default=""),
    Column("secret", Text, nullable=False, default=""),
    Column("email", String(254), nullable=False, default=""),
    Column("last_step", Integer, nullable=False, default=-1),
    Column("recovery_hashes", Text, nullable=False, default="[]"),
    Column("failures", Integer, nullable=False, default=0),
    Column("locked_until", Integer, nullable=False, default=0),
    Column("next_send", Integer, nullable=False, default=0),
    Column("send_window", Integer, nullable=False, default=0),
    Column("send_count", Integer, nullable=False, default=0),
    # MFA-management password/code failures have their own budget so that a
    # session holder cannot lock the owner out of signing in.
    Column("manage_failures", Integer, nullable=False, default=0),
    Column("manage_locked_until", Integer, nullable=False, default=0),
)

mfa_challenges = Table(
    "mfa_challenges", metadata,
    Column("id", String(64), primary_key=True),
    Column("user_id", String(32), ForeignKey("users.id", ondelete="CASCADE"), nullable=False),
    Column("version", Integer, nullable=False),
    Column("purpose", String(16), nullable=False),
    Column("method", String(16), nullable=False),
    Column("secret", Text, nullable=False, default=""),
    Column("email", String(254), nullable=False, default=""),
    Column("code_hash", String(64), nullable=False, default=""),
    Column("expires", Integer, nullable=False),
    Column("transport", String(16), nullable=False, default="cookies"),
)
Index("ix_mfa_challenges_user_id", mfa_challenges.c.user_id)
Index("ix_mfa_challenges_expires", mfa_challenges.c.expires)

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
    # MySQL 8 rejects literal defaults on TEXT columns but accepts parenthesized
    # expression defaults, which SQLite, PostgreSQL, and MariaDB also accept.
    Column("stage", Text, nullable=False, default="", server_default=text("('')")),
    Column("options", Text, nullable=False),
    Column("outputs", Text, nullable=False, default="[]", server_default=text("('[]')")),
    Column("error", Text),
    Column("warning", Text),
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

# Updated in the same SQL transaction as the records being cached. Redis never
# owns these markers, so an outage cannot lose an invalidation.
cache_revisions = Table(
    "cache_revisions",
    metadata,
    Column("name", String(32), primary_key=True),
    Column("revision", String(32), nullable=False),
)


def bump_cache_revision(db: Connection, name: str) -> None:
    db.execute(update(cache_revisions).where(cache_revisions.c.name == name).values(
        revision=uuid.uuid4().hex,
    ))


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


def _tls_context(*, verify_certificate: bool, verify_hostname: bool,
                 ca_file: str | None, cert_file: str | None,
                 key_file: str | None) -> ssl.SSLContext:
    context = ssl.create_default_context(cafile=ca_file or None)
    if verify_certificate:
        context.check_hostname = verify_hostname
    else:
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE
    if cert_file:
        context.load_cert_chain(cert_file, key_file or None)
    return context


def _pop_query(query: dict, *names: str) -> str | None:
    """Remove every spelling of a query parameter and return its last value."""
    found = None
    for key in [key for key in query if key.lower() in names]:
        value = query.pop(key)
        found = value[-1] if isinstance(value, tuple) else value
    return found


# libpq ``sslmode`` and MySQL ``ssl-mode`` semantics, mapped to
# (encrypt, verify certificate chain, verify hostname). The opportunistic
# ``prefer``/``allow``/``PREFERRED`` modes silently fall back to plaintext in
# libpq and the MySQL client; pg8000 and PyMySQL cannot do that, so they are
# rejected rather than guessed.
_POSTGRES_SSL_MODES = {
    "disable": (False, False, False),
    "require": (True, False, False),
    "verify-ca": (True, True, False),
    "verify-full": (True, True, True),
}
_MYSQL_SSL_MODES = {
    "disabled": (False, False, False),
    "required": (True, False, False),
    "verify_ca": (True, True, False),
    "verify_identity": (True, True, True),
}


def database_connect_options(database_url: str) -> tuple[URL, dict]:
    """Translate libpq/MySQL-client TLS URL parameters for pg8000 and PyMySQL.

    Neither pure-Python driver accepts ``sslmode``/``ssl-mode``; leaving them in
    the URL makes every connection fail. They are converted to an ``ssl.SSLContext``
    passed through ``connect_args`` (pg8000 ``ssl_context``, PyMySQL ``ssl``).
    """
    parsed = make_url(database_url)
    connect_args: dict = {}
    if parsed.get_backend_name() == "sqlite":
        return parsed, {"check_same_thread": False, "timeout": 30}
    query = dict(parsed.query)
    driver = parsed.get_driver_name()
    if parsed.get_backend_name() == "postgresql" and driver == "pg8000":
        mode = _pop_query(query, "sslmode")
        ca_file = _pop_query(query, "sslrootcert") if mode is not None else None
        cert_file = _pop_query(query, "sslcert") if mode is not None else None
        key_file = _pop_query(query, "sslkey") if mode is not None else None
        modes, argument = _POSTGRES_SSL_MODES, "ssl_context"
        mode_name = "sslmode"
        if mode is not None:
            mode = mode.strip().lower()
            # libpq verifies the chain for sslmode=require when a root
            # certificate is supplied; keep that behavior.
            if mode == "require" and ca_file:
                mode = "verify-ca"
    elif parsed.get_backend_name() in {"mysql", "mariadb"} and driver == "pymysql":
        mode = _pop_query(query, "ssl-mode", "ssl_mode")
        ca_file = _pop_query(query, "ssl-ca", "ssl_ca") if mode is not None else None
        cert_file = _pop_query(query, "ssl-cert", "ssl_cert") if mode is not None else None
        key_file = _pop_query(query, "ssl-key", "ssl_key") if mode is not None else None
        modes, argument = _MYSQL_SSL_MODES, "ssl"
        mode_name = "ssl-mode"
        if mode is not None:
            mode = mode.strip().lower().replace("-", "_")
    else:
        return parsed, connect_args
    if mode is None:
        return parsed, connect_args
    if mode not in modes:
        supported = ", ".join(modes)
        raise RuntimeError(
            f"DATABASE_URL {mode_name}={mode} is not supported by the {driver} driver; "
            f"use one of: {supported}"
        )
    encrypt, verify_certificate, verify_hostname = modes[mode]
    if encrypt:
        connect_args[argument] = _tls_context(
            verify_certificate=verify_certificate, verify_hostname=verify_hostname,
            ca_file=ca_file, cert_file=cert_file, key_file=key_file,
        )
    return parsed.set(query=query), connect_args


def create_database_engine(sqlite_path: Path) -> Engine:
    database_url = normalize_database_url(os.environ.get("DATABASE_URL"), sqlite_path)
    parsed, connect_args = database_connect_options(database_url)
    kwargs: dict = {"pool_pre_ping": True}
    if connect_args:
        kwargs["connect_args"] = connect_args
    engine = create_engine(parsed, **kwargs)

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


def _migrate_job_warning(engine: Engine) -> None:
    """Add the non-fatal job warning column to databases created by older releases."""
    inspector = inspect(engine)
    if not inspector.has_table("jobs"):
        return
    if "warning" in {column["name"] for column in inspector.get_columns("jobs")}:
        return
    try:
        with engine.begin() as connection:
            connection.execute(text("ALTER TABLE jobs ADD COLUMN warning TEXT"))
    except SQLAlchemyError:
        # Another startup worker may have completed the same migration.
        refreshed = {column["name"] for column in inspect(engine).get_columns("jobs")}
        if "warning" not in refreshed:
            raise


MFA_MANAGEMENT_BUDGET_COLUMNS = {
    "manage_failures": "ALTER TABLE mfa_accounts ADD COLUMN manage_failures INTEGER NOT NULL DEFAULT 0",
    "manage_locked_until": "ALTER TABLE mfa_accounts ADD COLUMN manage_locked_until INTEGER NOT NULL DEFAULT 0",
}


def _migrate_mfa_management_budget(engine: Engine) -> None:
    """Add the separate MFA-management failure budget to older databases."""
    inspector = inspect(engine)
    if not inspector.has_table("mfa_accounts"):
        return
    existing = {column["name"] for column in inspector.get_columns("mfa_accounts")}
    for name, statement in MFA_MANAGEMENT_BUDGET_COLUMNS.items():
        if name in existing:
            continue
        try:
            with engine.begin() as connection:
                connection.execute(text(statement))
        except SQLAlchemyError:
            # Another startup worker may have completed the same migration.
            refreshed = {column["name"] for column in inspect(engine).get_columns("mfa_accounts")}
            if name not in refreshed:
                raise


def initialize_database(
    engine: Engine,
    defaults: dict[str, str],
    timestamp: str,
) -> None:
    _migrate_legacy_sqlite(engine)
    metadata.create_all(engine)
    _migrate_user_theme(engine)
    _migrate_job_warning(engine)
    _migrate_mfa_management_budget(engine)
    with engine.begin() as connection:
        existing_revisions = set(connection.execute(select(cache_revisions.c.name)).scalars())
        for name in ("settings", "jobs"):
            if name not in existing_revisions:
                connection.execute(cache_revisions.insert().values(
                    name=name, revision=uuid.uuid4().hex,
                ))
            else:
                bump_cache_revision(connection, name)
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
