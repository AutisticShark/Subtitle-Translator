"""Regression tests for MFA, CAPTCHA, and email-delivery fixes; fully offline."""

import http.client
import io
import json
import logging
import smtplib
import uuid
from unittest.mock import patch

import pytest
import urllib.error
import urllib.request
from sqlalchemy import delete, insert, select, update

# This harness sets DATA_DIR before importing the application, never using real data.
from test_webapp import webapp
import test_mfa as base
import captcha
import email_delivery
from database import mfa_accounts, mfa_challenges, users


PASSWORD = base.PASSWORD


@pytest.fixture
def account(monkeypatch):
    webapp.app.config.update(TESTING=True, JWT_COOKIE_CSRF_PROTECT=False)
    user_id = uuid.uuid4().hex
    username = "fix-" + user_id[:16]
    with webapp.transaction(webapp.engine) as db:
        db.execute(insert(users).values(
            id=user_id, username=username, password_hash=webapp.password_hasher.hash(PASSWORD),
            role="user", created_at=webapp.now(), updated_at=webapp.now(),
        ))
    monkeypatch.setattr(webapp, "verify_captcha", lambda *args: None)
    webapp.login_attempts.clear()
    client = webapp.app.test_client()
    payload = {"username": username, "password": PASSWORD}
    assert client.post("/api/auth/login", json=payload).status_code == 200
    yield client, user_id, payload
    with webapp.transaction(webapp.engine) as db:
        db.execute(delete(users).where(users.c.id == user_id))
    webapp.engine.dispose()


def login(payload):
    return webapp.app.test_client().post("/api/auth/login", json=payload)


def account_row(user_id):
    with webapp.connection(webapp.engine) as db:
        return db.execute(select(mfa_accounts).where(mfa_accounts.c.user_id == user_id)).first()


# Bug 1: a login during the email cooldown must not invalidate the emailed code.

def test_login_during_email_cooldown_keeps_the_emailed_code_valid(account, monkeypatch):
    client, user_id, payload = account
    sent = base.allow_email(monkeypatch)
    base.enroll_email(client, sent)
    base.clear_cooldown(user_id)
    owner, first = base.login_challenge(payload)
    assert first["email_sent"] is True
    emailed = sent[-1][1]
    sends = len(sent)
    # A password holder logs in again while the 60-second cooldown is active.
    _, second = base.login_challenge(payload)
    assert len(sent) == sends
    assert second["email_sent"] is False and second["warning"]
    assert "recovery code" in second["warning"]
    assert 0 < second["expires_in"] <= first["expires_in"]
    assert webapp.mfa.decode_challenge(second)["cid"] == webapp.mfa.decode_challenge(first)["cid"]
    # The owner's emailed code still works with the original challenge.
    assert base.verify(owner, first, emailed).status_code == 200
    # And it is single-use across both reissued tokens.
    assert base.verify(webapp.app.test_client(), second, emailed).status_code == 400


def test_hourly_email_limit_cannot_be_used_to_replace_the_emailed_code(account, monkeypatch):
    client, user_id, payload = account
    sent = base.allow_email(monkeypatch)
    base.enroll_email(client, sent)
    base.clear_cooldown(user_id)
    owner, first = base.login_challenge(payload)
    emailed = sent[-1][1]
    with webapp.transaction(webapp.engine) as db:
        db.execute(update(mfa_accounts).where(mfa_accounts.c.user_id == user_id).values(
            next_send=0, send_count=10, send_window=webapp.mfa.timestamp(),
        ))
    for _ in range(5):
        _, repeated = base.login_challenge(payload)
        assert repeated["email_sent"] is False
    with webapp.connection(webapp.engine) as db:
        rows = db.execute(select(mfa_challenges).where(
            mfa_challenges.c.user_id == user_id, mfa_challenges.c.purpose == "login",
        )).all()
    assert len(rows) == 1 and rows[0].code_hash
    assert base.verify(owner, first, emailed).status_code == 200


def test_reissued_login_challenge_honours_the_new_requests_transport(account, monkeypatch):
    client, user_id, payload = account
    sent = base.allow_email(monkeypatch)
    base.enroll_email(client, sent)
    base.clear_cooldown(user_id)
    base.login_challenge(payload)
    emailed = sent[-1][1]
    api, reissued = base.login_challenge({**payload, "token_transport": "header"})
    result = base.verify(api, reissued, emailed)
    assert result.status_code == 200
    assert "access_token" in result.json
    assert not api.get_cookie("access_token_cookie")


def test_cooldown_without_a_pending_code_still_returns_a_recovery_challenge(account, monkeypatch):
    client, user_id, payload = account
    sent = base.allow_email(monkeypatch)
    recovery = base.enroll_email(client, sent)
    # Enrollment set a cooldown and left no login challenge to reissue.
    guest, challenge = base.login_challenge(payload)
    assert challenge["email_sent"] is False
    assert challenge["warning"] == "Email could not be sent. Retry later or use a recovery code."
    assert base.verify(guest, challenge, recovery[0]).status_code == 200


def test_reissue_ignores_challenges_from_an_older_token_version(account, monkeypatch):
    client, user_id, payload = account
    sent = base.allow_email(monkeypatch)
    recovery = base.enroll_email(client, sent)
    base.clear_cooldown(user_id)
    base.login_challenge(payload)
    stale_code = sent[-1][1]
    with webapp.transaction(webapp.engine) as db:
        db.execute(update(users).where(users.c.id == user_id).values(token_version=users.c.token_version + 1))
    guest, challenge = base.login_challenge(payload)
    assert challenge["email_sent"] is False
    assert base.verify(guest, challenge, stale_code).status_code == 400
    assert base.verify(guest, challenge, recovery[0]).status_code == 200


# Bug 2: MFA-management password failures must not lock sign-in.

def test_setup_password_failures_do_not_lock_login_without_mfa(account):
    client, user_id, payload = account
    for _ in range(5):
        response = client.post("/api/auth/mfa/setup", json={"method": "totp", "password": "wrong"})
        assert response.status_code == 400
        assert response.json["error"] == "Invalid password"
    assert client.post("/api/auth/mfa/setup", json={
        "method": "totp", "password": PASSWORD,
    }).status_code == 429
    response = login(payload)
    assert response.status_code == 200, response.json
    assert "mfa_required" not in response.json
    assert account_row(user_id).locked_until == 0


def test_management_lockout_does_not_lock_mfa_login(account):
    client, user_id, payload = account
    _, recovery, _ = base.enroll_totp(client)
    for _ in range(5):
        response = client.post("/api/auth/mfa/disable", json={"password": "wrong", "code": recovery[0]})
        assert response.status_code == 400
        assert response.json["error"] == "Invalid password"
    response = client.post("/api/auth/mfa/disable", json={"password": PASSWORD, "code": recovery[0]})
    assert response.status_code == 429 and int(response.headers["Retry-After"]) > 0
    guest, challenge = base.login_challenge(payload)
    assert base.verify(guest, challenge, recovery[0]).status_code == 200
    assert account_row(user_id).manage_locked_until > 0


def test_management_code_failures_use_the_management_budget(account):
    client, user_id, payload = account
    _, recovery, _ = base.enroll_totp(client)
    guest, challenge = base.login_challenge(payload)
    for _ in range(5):
        assert client.post("/api/auth/mfa/recovery", json={
            "password": PASSWORD, "code": "invalid",
        }).status_code == 400
    assert client.post("/api/auth/mfa/recovery", json={
        "password": PASSWORD, "code": recovery[0],
    }).status_code == 429
    # The pending sign-in challenge survives the management lockout.
    assert base.verify(guest, challenge, recovery[0]).status_code == 200


def test_login_lockout_still_applies_to_enrolled_accounts(account):
    client, _, payload = account
    _, recovery, _ = base.enroll_totp(client)
    for _ in range(5):
        guest, challenge = base.login_challenge(payload)
        assert base.verify(guest, challenge, "invalid").status_code == 400
    response = login(payload)
    assert response.status_code == 429 and int(response.headers["Retry-After"]) > 0


def test_existing_databases_gain_the_management_budget_columns(tmp_path):
    import sqlite3
    from contextlib import closing
    from database import create_database_engine, initialize_database

    path = tmp_path / "old.db"
    with closing(sqlite3.connect(path)) as legacy:
        legacy.execute(
            "CREATE TABLE mfa_accounts (user_id VARCHAR(32) PRIMARY KEY, method VARCHAR(16) NOT NULL DEFAULT '', "
            "secret TEXT NOT NULL DEFAULT '', email VARCHAR(254) NOT NULL DEFAULT '', "
            "last_step INTEGER NOT NULL DEFAULT -1, recovery_hashes TEXT NOT NULL DEFAULT '[]', "
            "failures INTEGER NOT NULL DEFAULT 0, locked_until INTEGER NOT NULL DEFAULT 0, "
            "next_send INTEGER NOT NULL DEFAULT 0, send_window INTEGER NOT NULL DEFAULT 0, "
            "send_count INTEGER NOT NULL DEFAULT 0)"
        )
        legacy.execute("INSERT INTO mfa_accounts (user_id) VALUES ('legacy')")
        legacy.commit()
    engine = create_database_engine(path)
    try:
        initialize_database(engine, {}, "2026-01-01T00:00:00+00:00")
        initialize_database(engine, {}, "2026-01-01T00:00:00+00:00")  # idempotent
        with engine.connect() as db:
            row = db.execute(select(mfa_accounts)).first()
        assert row.manage_failures == 0 and row.manage_locked_until == 0
    finally:
        engine.dispose()


# Bugs 3 and 4: CAPTCHA hostname comes only from configuration; transport errors fail closed.

def provider_reply(result):
    return patch.object(urllib.request, "urlopen", return_value=io.BytesIO(json.dumps(result).encode()))


def verify_token(**overrides):
    arguments = {
        "provider": "turnstile", "action": "login", "token": "token", "site_key": "site",
        "secret_key": "secret", "configured_hostname": "app.example", "remote_addr": "192.0.2.1",
    }
    arguments.update(overrides)
    return captcha.verify_token(**arguments)


def test_captcha_hostname_must_match_the_configured_value():
    with provider_reply({"success": True, "hostname": "App.Example.", "action": "login"}):
        assert verify_token() is None
    with provider_reply({"success": True, "hostname": "evil.example", "action": "login"}):
        assert verify_token()[1] == 400
    with provider_reply({"success": True, "action": "login"}):
        assert verify_token()[1] == 400


def test_captcha_never_derives_the_hostname_from_request_headers(monkeypatch):
    settings = {
        "captcha_provider": "hcaptcha", "captcha_hostname": "app.example", "captcha_on_login": "1",
        "hcaptcha_site_key": "site", "hcaptcha_secret_key": "secret",
    }
    monkeypatch.setattr(webapp, "read_settings", lambda **_: settings)
    headers = {"Host": "evil.example", "X-Forwarded-Host": "evil.example"}
    with webapp.app.test_request_context("/api/auth/login", method="POST", headers=headers):
        with provider_reply({"success": True, "hostname": "evil.example"}):
            response, status = webapp.verify_captcha("login", "token")
        assert status == 400
        with provider_reply({"success": True, "hostname": "app.example"}):
            assert webapp.verify_captcha("login", "token") is None
        # A blank configuration never falls back to the client-supplied Host.
        settings["captcha_hostname"] = ""
        with patch.object(captcha, "verify_token", return_value=None) as verifier:
            assert webapp.verify_captcha("login", "token") is None
        arguments = verifier.call_args.kwargs
        assert "evil.example" not in json.dumps(arguments, default=str)
        assert arguments["configured_hostname"] == ""


def test_blank_captcha_hostname_skips_the_check_and_warns_once(monkeypatch, caplog):
    monkeypatch.setattr(captcha, "_warned_unpinned_hostname", False)
    caplog.set_level(logging.WARNING, logger=captcha.LOGGER.name)
    with provider_reply({"success": True, "hostname": "anything.example", "action": "login"}):
        assert verify_token(configured_hostname="") is None
    with provider_reply({"success": True, "hostname": "other.example", "action": "login"}):
        assert verify_token(configured_hostname="  ") is None
    warnings = [record for record in caplog.records if "hostname is not configured" in record.getMessage()]
    assert len(warnings) == 1
    # Every other check still fails closed.
    with provider_reply({"success": True, "hostname": "anything.example", "action": "upload"}):
        assert verify_token(configured_hostname="")[1] == 400
    with provider_reply({"success": False, "hostname": "anything.example", "action": "login"}):
        assert verify_token(configured_hostname="")[1] == 400


class BrokenResponse(io.BytesIO):
    def __init__(self, error):
        super().__init__(b"")
        self.error = error

    def read(self, *args):
        raise self.error


@pytest.mark.parametrize("error", [
    http.client.BadStatusLine("garbage"),
    http.client.IncompleteRead(b"{"),
    http.client.LineTooLong("header line"),
    http.client.RemoteDisconnected("closed"),
    TimeoutError("slow"),
    urllib.error.URLError("offline"),
    ValueError("bad"),
], ids=lambda error: type(error).__name__)
def test_captcha_transport_errors_fail_closed_with_503(error, monkeypatch):
    with patch.object(urllib.request, "urlopen", side_effect=error):
        assert verify_token() == ("CAPTCHA is temporarily unavailable", 503)
    with patch.object(urllib.request, "urlopen", return_value=BrokenResponse(error)):
        assert verify_token() == ("CAPTCHA is temporarily unavailable", 503)


@pytest.mark.parametrize("body", [b"not json", b"\xff\xfe", b"[" * 70000], ids=["text", "encoding", "nested"])
def test_captcha_malformed_bodies_fail_closed(body):
    with patch.object(urllib.request, "urlopen", return_value=io.BytesIO(body)):
        failure = verify_token()
    assert failure is not None and failure[1] in (400, 503)


def test_captcha_transport_error_returns_localized_503_response(monkeypatch):
    settings = {
        "captcha_provider": "turnstile", "captcha_hostname": "app.example", "captcha_on_login": "1",
        "turnstile_site_key": "site", "turnstile_secret_key": "secret",
    }
    monkeypatch.setattr(webapp, "read_settings", lambda **_: settings)
    with webapp.app.test_request_context("/api/auth/login?lang=zh-TW", method="POST"), \
            patch.object(urllib.request, "urlopen", side_effect=http.client.BadStatusLine("x")):
        response, status = webapp.verify_captcha("login", "token")
    assert status == 503
    assert response.json["error"] != "CAPTCHA is temporarily unavailable"


# Bug 5: a failed SMTP QUIT after acceptance is not a delivery failure.

class FakeSMTP(smtplib.SMTP):
    """Exercises smtplib's real context-manager exit without a network."""

    instances = []
    quit_reply = (221, b"bye")
    send_error = None

    def connect(self, host="localhost", port=0, source_address=None):
        FakeSMTP.instances.append(self)
        self.calls = []
        return 220, b"ready"

    def starttls(self, **kwargs):
        self.calls.append("starttls")
        return 220, b"go ahead"

    def login(self, user, password, **kwargs):
        self.calls.append("login")
        return 235, b"ok"

    def send_message(self, message, *args, **kwargs):
        self.calls.append("send_message")
        if self.send_error:
            raise self.send_error
        return {}

    def docmd(self, cmd, args=""):
        self.calls.append(cmd)
        if isinstance(self.quit_reply, BaseException):
            raise self.quit_reply
        return self.quit_reply

    def close(self):
        self.calls.append("close")


@pytest.fixture
def fake_smtp(monkeypatch):
    for name, value in {
        "EMAIL_PROVIDER": "smtp", "EMAIL_FROM": "sender@example.com", "SMTP_HOST": "smtp.example.com",
        "SMTP_USERNAME": "mailer", "SMTP_PASSWORD": "smtp-password",
    }.items():
        monkeypatch.setenv(name, value)
    FakeSMTP.instances = []
    monkeypatch.setattr(FakeSMTP, "quit_reply", (221, b"bye"))
    monkeypatch.setattr(FakeSMTP, "send_error", None)
    monkeypatch.setattr(email_delivery.smtplib, "SMTP", FakeSMTP)
    return FakeSMTP


@pytest.mark.parametrize("reply", [
    (421, b"closing"), smtplib.SMTPServerDisconnected("gone"), ConnectionResetError("reset"),
], ids=["421", "disconnected", "reset"])
def test_smtp_quit_failure_after_acceptance_is_success(fake_smtp, monkeypatch, reply):
    monkeypatch.setattr(fake_smtp, "quit_reply", reply)
    email_delivery.send_email("user@example.com", "Subject", "Body")
    [smtp] = fake_smtp.instances
    assert smtp.calls[:3] == ["starttls", "login", "send_message"]
    assert smtp.calls.count("send_message") == 1
    assert smtp.calls[-1] == "close"


def test_smtp_failure_before_acceptance_is_reported_once_without_retry(fake_smtp, monkeypatch):
    monkeypatch.setattr(fake_smtp, "send_error", smtplib.SMTPDataError(451, b"try later"))
    monkeypatch.setattr(fake_smtp, "quit_reply", (421, b"closing"))
    with pytest.raises(email_delivery.EmailDeliveryError):
        email_delivery.send_email("user@example.com", "Subject", "Body")
    [smtp] = fake_smtp.instances
    assert smtp.calls.count("send_message") == 1
    assert smtp.calls[-1] == "close"
