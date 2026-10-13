"""Regression tests for the supporting-module audit (MFA, database, settings, email); offline."""

import logging
import re
import sqlite3
import ssl
import threading
import uuid
from contextlib import closing
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import Mock
from urllib.error import HTTPError

import pytest
from sqlalchemy import create_engine, delete, event, insert, inspect, select, update
from sqlalchemy.exc import DBAPIError, OperationalError

# This harness sets DATA_DIR before importing the application, never using real data.
from test_webapp import webapp
import test_mfa as base
import database
import email_delivery
import mfa as mfa_module
from database import (
    create_database_engine, database_connect_options, mfa_accounts, mfa_challenges,
    normalize_database_url, users,
)
from settings_schema import validate_numeric_settings


PASSWORD = base.PASSWORD
UNSENT = "Email could not be sent. Retry later or use a recovery code."
UNREADABLE = ("Your authenticator secret cannot be read. Use a recovery code "
              "and ask an administrator to restore the encryption key.")


def create_user():
    user_id = uuid.uuid4().hex
    with webapp.transaction(webapp.engine) as db:
        db.execute(insert(users).values(
            id=user_id, username="audit-" + user_id[:16],
            password_hash=webapp.password_hasher.hash(PASSWORD),
            role="user", created_at=webapp.now(), updated_at=webapp.now(),
        ))
    return user_id


@pytest.fixture
def account(monkeypatch):
    webapp.app.config.update(TESTING=True, JWT_COOKIE_CSRF_PROTECT=False)
    user_id = create_user()
    with webapp.connection(webapp.engine) as db:
        username = db.execute(select(users.c.username).where(users.c.id == user_id)).scalar()
    monkeypatch.setattr(webapp, "verify_captcha", lambda *args: None)
    webapp.login_attempts.clear()
    client = webapp.app.test_client()
    payload = {"username": username, "password": PASSWORD}
    assert client.post("/api/auth/login", json=payload).status_code == 200
    yield client, user_id, payload
    with webapp.transaction(webapp.engine) as db:
        db.execute(delete(users).where(users.c.id == user_id))
    webapp.engine.dispose()


def account_row(user_id):
    with webapp.connection(webapp.engine) as db:
        return db.execute(select(mfa_accounts).where(mfa_accounts.c.user_id == user_id)).first()


def challenge_rows(user_id, purpose="login"):
    with webapp.connection(webapp.engine) as db:
        return db.execute(select(mfa_challenges).where(
            mfa_challenges.c.user_id == user_id, mfa_challenges.c.purpose == purpose,
        )).all()


# Finding 1: an undecryptable TOTP secret is an unusable factor, not a 500.

def corrupt_account_secret(user_id):
    with webapp.transaction(webapp.engine) as db:
        db.execute(update(mfa_accounts).where(mfa_accounts.c.user_id == user_id).values(
            secret="enc:v1:not-a-fernet-token",
        ))


def test_unreadable_totp_secret_spends_the_login_budget_and_keeps_recovery(account, monkeypatch, caplog):
    client, user_id, payload = account
    _, recovery, _ = base.enroll_totp(client)
    corrupt_account_secret(user_id)
    monkeypatch.setattr(webapp.mfa, "warned_unreadable", False)
    caplog.set_level(logging.ERROR, logger=mfa_module.LOGGER.name)
    guest, challenge = base.login_challenge(payload)
    for attempt in (1, 2):
        response = base.verify(guest, challenge, "123456")
        assert response.status_code == 503 and response.is_json
        assert response.json["error"] == UNREADABLE
        # The failure is committed, not rolled back with an exception.
        assert account_row(user_id).failures == attempt
    logged = [record for record in caplog.records if "cannot be decrypted" in record.getMessage()]
    assert len(logged) == 1
    assert "not-a-fernet-token" not in caplog.text
    # Recovery codes never need the secret.
    assert base.verify(guest, challenge, recovery[0]).status_code == 200
    assert account_row(user_id).failures == 0


def test_unreadable_totp_secret_is_localized_and_locks_after_the_budget(account):
    client, user_id, payload = account
    base.enroll_totp(client)
    corrupt_account_secret(user_id)
    guest, challenge = base.login_challenge(payload)
    response = guest.post("/api/auth/mfa/verify?lang=zh-TW", json={
        "challenge_token": challenge["challenge_token"], "code": "123456",
    })
    assert response.status_code == 503
    assert response.json["error"] not in ("", UNREADABLE)
    for _ in range(4):
        base.verify(guest, challenge, "123456")
    assert account_row(user_id).locked_until > 0
    assert webapp.app.test_client().post("/api/auth/login", json=payload).status_code == 429


def test_unreadable_secret_during_management_uses_the_management_budget(account):
    client, user_id, _ = account
    _, recovery, _ = base.enroll_totp(client)
    corrupt_account_secret(user_id)
    response = client.post("/api/auth/mfa/disable", json={"password": PASSWORD, "code": "123456"})
    assert response.status_code == 503 and response.json["error"] == UNREADABLE
    row = account_row(user_id)
    assert row.manage_failures == 1 and row.failures == 0
    assert client.post("/api/auth/mfa/disable", json={
        "password": PASSWORD, "code": recovery[0],
    }).status_code == 200


def test_unreadable_pending_enrollment_secret_is_rejected_cleanly(account):
    client, user_id, _ = account
    setup = client.post("/api/auth/mfa/setup", json={"password": PASSWORD, "method": "totp"}).json
    with webapp.transaction(webapp.engine) as db:
        db.execute(update(mfa_challenges).where(mfa_challenges.c.user_id == user_id).values(
            secret="enc:v1:not-a-fernet-token",
        ))
    response = client.post("/api/auth/mfa/confirm", json={
        "challenge_token": setup["challenge_token"], "code": "123456",
    })
    assert response.status_code == 503 and response.json["error"] == UNREADABLE
    assert account_row(user_id).manage_failures == 1
    assert account_row(user_id).method == ""


# Finding 2: a stored code hash means an email with that code was attempted.

def test_suppressed_login_email_stores_no_code_and_reissue_says_so(account, monkeypatch):
    client, user_id, payload = account
    sent = base.allow_email(monkeypatch)
    recovery = base.enroll_email(client, sent)
    sends = len(sent)
    # Enrollment left the cooldown active, so the first sign-in sends nothing.
    _, first = base.login_challenge(payload)
    assert first["email_sent"] is False and first["warning"] == UNSENT
    assert [row.code_hash for row in challenge_rows(user_id)] == [""]
    guest, second = base.login_challenge(payload)
    assert len(sent) == sends
    assert second["email_sent"] is False and second["warning"] == UNSENT
    assert base.verify(guest, second, recovery[0]).status_code == 200


def test_failed_login_email_forgets_its_code(account, monkeypatch):
    client, user_id, payload = account
    sent = base.allow_email(monkeypatch)
    recovery = base.enroll_email(client, sent)
    base.clear_cooldown(user_id)
    monkeypatch.setattr(mfa_module, "send_email_code",
                        Mock(side_effect=email_delivery.EmailDeliveryError("offline")))
    _, first = base.login_challenge(payload)
    assert first["email_sent"] is False and first["warning"] == UNSENT
    assert "challenge_id" not in first
    assert [row.code_hash for row in challenge_rows(user_id)] == [""]
    # Still in the cooldown: the reissued challenge must not claim a code arrived.
    guest, second = base.login_challenge(payload)
    assert second["warning"] == UNSENT
    assert base.verify(guest, second, recovery[0]).status_code == 200


def test_delivered_login_code_is_still_reissued(account, monkeypatch):
    client, user_id, payload = account
    sent = base.allow_email(monkeypatch)
    base.enroll_email(client, sent)
    base.clear_cooldown(user_id)
    _, first = base.login_challenge(payload)
    assert first["email_sent"] is True and "challenge_id" not in first
    assert challenge_rows(user_id)[0].code_hash
    _, second = base.login_challenge(payload)
    assert "requested recently" in second["warning"]


# Finding 3: other accounts' expired challenges are purged outside the request transaction.

def insert_challenge(user_id, purpose, expires):
    challenge_id = uuid.uuid4().hex
    with webapp.transaction(webapp.engine) as db:
        db.execute(insert(mfa_challenges).values(
            id=challenge_id, user_id=user_id, version=0, purpose=purpose, method="totp",
            expires=expires,
        ))
    return challenge_id


def challenge_exists(challenge_id):
    with webapp.connection(webapp.engine) as db:
        return db.execute(select(mfa_challenges.c.id).where(
            mfa_challenges.c.id == challenge_id,
        )).first() is not None


def test_challenge_creation_purges_only_the_current_accounts_expired_rows(account, monkeypatch):
    client, user_id, payload = account
    base.enroll_totp(client)
    other = create_user()
    try:
        now = webapp.mfa.timestamp()
        own_expired = insert_challenge(user_id, "manage", now - 1)
        other_expired = insert_challenge(other, "login", now - 1)
        other_live = insert_challenge(other, "manage", now + 300)
        statements = []

        def record(conn, cursor, statement, parameters, context, executemany):
            if statement.lstrip().upper().startswith("DELETE FROM MFA_CHALLENGES"):
                statements.append(statement)

        monkeypatch.setattr(webapp.mfa, "next_purge", now + 3600)  # throttle the global purge
        event.listen(webapp.engine, "before_cursor_execute", record)
        try:
            base.login_challenge(payload)
        finally:
            event.remove(webapp.engine, "before_cursor_execute", record)
        assert statements and all("user_id" in statement for statement in statements)
        assert not challenge_exists(own_expired)
        assert challenge_exists(other_expired) and challenge_exists(other_live)

        monkeypatch.setattr(webapp.mfa, "next_purge", 0)
        webapp.mfa.purge_expired_challenges()
        assert not challenge_exists(other_expired)
        assert challenge_exists(other_live)
        assert len(challenge_rows(user_id)) == 1  # the live sign-in challenge survives
        assert webapp.mfa.next_purge > now
    finally:
        with webapp.transaction(webapp.engine) as db:
            db.execute(delete(users).where(users.c.id == other))


def test_failed_challenge_purge_is_best_effort(monkeypatch, tmp_path):
    broken = create_engine(f"sqlite:///{(tmp_path / 'empty.db').as_posix()}")
    try:
        monkeypatch.setattr(webapp.mfa, "engine", broken)
        monkeypatch.setattr(webapp.mfa, "next_purge", 0)
        webapp.mfa.purge_expired_challenges()  # "no such table" must not escape
    finally:
        broken.dispose()


# Finding 4: TLS file parameters without sslmode/ssl-mode.

def options(raw):
    return database_connect_options(normalize_database_url(raw, Path("unused.db")))


@pytest.fixture
def tls_calls(monkeypatch):
    calls = []

    def fake_context(**kwargs):
        calls.append(kwargs)
        return ssl.create_default_context()

    monkeypatch.setattr(database, "_tls_context", fake_context)
    return calls


@pytest.mark.parametrize("query, verify, files", [
    ("sslrootcert=/ca.pem", True, ("/ca.pem", None, None)),
    ("SSLROOTCERT=/ca.pem&sslcert=/c.pem&sslkey=/k.pem", True, ("/ca.pem", "/c.pem", "/k.pem")),
    ("sslcert=/c.pem&sslkey=/k.pem", False, (None, "/c.pem", "/k.pem")),
], ids=["root-cert", "all-files", "client-cert"])
def test_postgres_tls_files_without_sslmode_infer_libpq_semantics(tls_calls, query, verify, files):
    url, connect_args = options("postgresql://u:p@db.example/app?" + query)
    assert not {key.lower() for key in url.query} & {"sslrootcert", "sslcert", "sslkey"}
    assert "ssl_context" in connect_args
    [call] = tls_calls
    assert call["verify_certificate"] is verify and call["verify_hostname"] is False
    assert (call["ca_file"], call["cert_file"], call["key_file"]) == files


def test_postgres_tls_files_without_sslmode_reach_pg8000_without_type_error(tls_calls, monkeypatch):
    monkeypatch.setenv("DATABASE_URL", "postgres://u:p@127.0.0.1:1/app?sslrootcert=/ca.pem")
    engine = create_database_engine(Path("unused.db"))
    try:
        # Port 1 refuses the connection; previously pg8000 raised TypeError first.
        with pytest.raises(DBAPIError):
            with engine.connect():
                pass
    finally:
        engine.dispose()


@pytest.mark.parametrize("query, expected", [
    ("ssl-ca=/ca.pem", {"ca": "/ca.pem"}),
    ("ssl_ca=/ca.pem&ssl-cert=/c.pem&SSL-KEY=/k.pem", {"ca": "/ca.pem", "cert": "/c.pem", "key": "/k.pem"}),
], ids=["hyphen-ca", "mixed"])
def test_mysql_tls_files_without_ssl_mode_use_pymysql_ssl_options(tls_calls, query, expected):
    url, connect_args = options("mysql://u:p@db.example/app?charset=utf8mb4&" + query)
    assert connect_args == {} and not tls_calls
    assert not {key for key in url.query if "-" in key}
    engine = create_engine(url)
    _, arguments = engine.dialect.create_connect_args(engine.url)
    assert arguments["ssl"] == expected and arguments["charset"] == "utf8mb4"
    assert not {key for key in arguments if key.startswith("ssl") and key != "ssl"}


def test_tls_files_with_a_mode_still_translate(tls_calls):
    _, connect_args = options("mysql://u:p@db.example/app?ssl-ca=/ca.pem&ssl-mode=VERIFY_CA")
    assert tls_calls[-1]["ca_file"] == "/ca.pem" and tls_calls[-1]["verify_hostname"] is False
    url, connect_args = options("postgres://u:p@db.example/app?sslmode=disable&sslrootcert=/ca.pem")
    assert connect_args == {} and "sslrootcert" not in url.query


# Finding 5: numeric settings never return raw Python conversion errors.

@pytest.mark.parametrize("value", ["abc", "", "nan", "1,5", [], True], ids=repr)
def test_number_settings_return_the_translated_bounds_message(value):
    assert validate_numeric_settings({"rpm": value}) == "rpm must be between 0 and 10000"


# Finding 6: one address validator shared by MFA setup and the transports.

@pytest.mark.parametrize("address, valid", [
    ("user@example.com", True), ("first.last+tag@sub.example.org", True),
    ("User <user@example.com>", False), ("user@example", False), ("", False), (None, False),
    ("a" * 65 + "@example.com", False), ("user@" + "a" * 250 + ".com", False),
    ("user@example.com\n", False), ("a@example.com,b@example.com", False),
], ids=["plain", "tagged", "display-name", "no-tld", "empty", "none", "long-local",
        "long-address", "newline", "list"])
def test_shared_address_validator(address, valid):
    assert email_delivery.is_valid_address(address) is valid


# Finding 7: one race-safe ADD COLUMN helper for every migration.

def test_added_column_statements_follow_the_mysql_text_default_rule():
    for table, column, statement in database.ADDED_COLUMNS:
        assert statement.startswith(f"ALTER TABLE {table} ADD COLUMN {column} ")
        assert not re.search(r"\b(?:TEXT|BLOB|JSON)\b[^,]*\bDEFAULT\s+'", statement, re.I)


def stale_inspector(engine):
    """Report the table without columns once, as seen just before a racing worker's ALTER."""
    real = inspect(engine)
    stale = Mock(has_table=Mock(return_value=True), get_columns=Mock(return_value=[]))
    calls = iter([stale])
    return lambda bind: next(calls, real)


def test_add_column_tolerates_a_concurrent_migration_but_not_other_errors(tmp_path, monkeypatch):
    path = tmp_path / "race.db"
    with closing(sqlite3.connect(path)) as raw:
        raw.execute("CREATE TABLE sample (id INTEGER PRIMARY KEY, added TEXT)")
        raw.commit()
    engine = create_engine(f"sqlite:///{path.as_posix()}")
    try:
        # Another worker added the column after our inspection: the duplicate
        # column error is swallowed because the column now exists.
        monkeypatch.setattr(database, "inspect", stale_inspector(engine))
        assert database._add_column_if_missing(
            engine, "sample", "added", "ALTER TABLE sample ADD COLUMN added TEXT") is True
        # A failure that leaves the column missing is re-raised.
        monkeypatch.setattr(database, "inspect", inspect)
        with pytest.raises(OperationalError):
            database._add_column_if_missing(
                engine, "sample", "missing", "ALTER TABLE sample ADD COLUMN missing BOGUS(")
        assert database._add_column_if_missing(
            engine, "sample", "added", "ALTER TABLE sample ADD COLUMN added TEXT") is False
        assert database._add_column_if_missing(
            engine, "absent", "added", "ALTER TABLE absent ADD COLUMN added TEXT") is False
    finally:
        engine.dispose()


# Finding 8: the real opener raises for every non-2xx status before _post_json reads it.

class StatusHandler(BaseHTTPRequestHandler):
    requests = []

    def do_POST(self):
        StatusHandler.requests.append(self.path)
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path == "/redirect":
            self.send_response(307)
            self.send_header("Location", "/accepted")
        else:
            self.send_response(429 if self.path == "/limited" else 200)
        body = b'{"id":"accepted"}'
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def test_post_json_opener_rejects_every_non_2xx_status():
    StatusHandler.requests = []
    server = ThreadingHTTPServer(("127.0.0.1", 0), StatusHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base_url = f"http://127.0.0.1:{server.server_address[1]}"
    try:
        assert email_delivery._post_json(base_url + "/accepted", b"{}", {}) == {"id": "accepted"}
        for path, status in (("/limited", 429), ("/redirect", 307)):
            with pytest.raises(HTTPError) as error:
                email_delivery._post_json(base_url + path, b"{}", {})
            assert error.value.code == status
        # The redirect was not followed.
        assert StatusHandler.requests == ["/accepted", "/limited", "/redirect"]
    finally:
        server.shutdown()
        server.server_close()
