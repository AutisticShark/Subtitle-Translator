"""Offline MFA enrollment, authentication, recovery, and adversarial checks."""

import hashlib
import json
import smtplib
import uuid
from concurrent.futures import ThreadPoolExecutor
from unittest.mock import Mock

import jwt
import pyotp
import pytest
from sqlalchemy import delete, insert, select, update

# This harness sets DATA_DIR before importing the application, never using real data.
from test_webapp import webapp
import mfa as mfa_module
from database import mfa_accounts, mfa_challenges, users


PASSWORD = "a-long-mfa-test-password"


@pytest.fixture
def account(monkeypatch):
    webapp.app.config.update(TESTING=True, JWT_COOKIE_CSRF_PROTECT=False)
    user_id = uuid.uuid4().hex
    username = "mfa-" + user_id[:16]
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


def enroll_totp(client):
    response = client.post("/api/auth/mfa/setup", json={"password": PASSWORD, "method": "totp"})
    assert response.status_code == 200, response.json
    setup = response.json
    code = pyotp.TOTP(setup["secret"]).now()
    response = client.post("/api/auth/mfa/confirm", json={
        "challenge_token": setup["challenge_token"], "code": code,
    })
    assert response.status_code == 200, response.json
    return setup, response.json["recovery_codes"], code


def login_challenge(payload):
    client = webapp.app.test_client()
    response = client.post("/api/auth/login", json=payload)
    assert response.status_code == 200, response.json
    assert response.json["mfa_required"]
    assert "access_token" not in response.json
    assert "user" not in response.json
    assert not client.get_cookie("access_token_cookie")
    return client, response.json


def verify(client, challenge, code):
    return client.post("/api/auth/mfa/verify", json={
        "challenge_token": challenge["challenge_token"], "code": code,
    })


def allow_email(monkeypatch):
    sent = []
    monkeypatch.setenv("SMTP_HOST", "smtp.example.com")
    monkeypatch.setenv("SMTP_FROM", "noreply@example.com")
    monkeypatch.setattr(mfa_module, "send_email_code", lambda address, code: sent.append((address, code)))
    return sent


def clear_cooldown(user_id):
    with webapp.transaction(webapp.engine) as db:
        db.execute(update(mfa_accounts).where(mfa_accounts.c.user_id == user_id).values(next_send=0))


def enroll_email(client, sent):
    response = client.post("/api/auth/mfa/setup", json={
        "password": PASSWORD, "method": "email", "email": "owner@example.com",
    })
    assert response.status_code == 200, response.json
    setup = response.json
    code = sent[-1][1]
    response = client.post("/api/auth/mfa/confirm", json={
        "challenge_token": setup["challenge_token"], "code": code,
    })
    assert response.status_code == 200, response.json
    return response.json["recovery_codes"]


def test_totp_enrollment_is_pending_encrypted_and_scoped(account):
    client, user_id, payload = account
    old_cookie = client.get_cookie("access_token_cookie").value
    setup, recovery, used_code = enroll_totp(client)
    assert setup["provisioning_uri"].startswith("otpauth://totp/")
    assert setup["qr_code"].startswith("data:image/svg+xml;base64,")
    assert len(recovery) == len(set(recovery)) == 10
    status = client.get("/api/auth/mfa").json
    assert status["method"] == "totp"
    assert "secret" not in status and "recovery_codes" not in status
    old_client = webapp.app.test_client()
    old_client.set_cookie("access_token_cookie", old_cookie)
    assert old_client.get("/api/auth/me").status_code == 401
    with webapp.connection(webapp.engine) as db:
        row = db.execute(select(mfa_accounts).where(mfa_accounts.c.user_id == user_id)).first()
        assert row.secret.startswith("enc:v1:") and setup["secret"] not in row.secret
        assert all(code.replace("-", "") not in row.recovery_hashes for code in recovery)
        assert not db.execute(select(mfa_challenges).where(mfa_challenges.c.user_id == user_id)).first()
    guest, challenge = login_challenge(payload)
    assert guest.get("/api/jobs").status_code == 401
    assert verify(guest, challenge, used_code).status_code == 400
    next_code = pyotp.TOTP(setup["secret"]).at(webapp.mfa.timestamp() + 30)
    assert verify(guest, challenge, next_code).status_code == 200
    assert guest.get("/api/auth/me").status_code == 200
    assert verify(guest, challenge, next_code).status_code == 400


def test_pending_enrollment_requires_password_confirmation_and_ownership(account):
    client, user_id, payload = account
    assert client.post("/api/auth/mfa/setup", json={
        "method": "totp", "password": "wrong",
    }).status_code == 400
    setup = client.post("/api/auth/mfa/setup", json={"method": "totp", "password": PASSWORD}).json
    assert client.get("/api/auth/mfa").json["method"] == ""
    other = webapp.app.test_client()
    # Without a completed enrollment the account still logs in normally.
    assert not other.post("/api/auth/login", json=payload).json.get("mfa_required")
    with webapp.transaction(webapp.engine) as db:
        db.execute(update(users).where(users.c.id == user_id).values(token_version=users.c.token_version + 1))
    assert other.post("/api/auth/login", json=payload).status_code == 200
    assert other.post("/api/auth/mfa/confirm", json={
        "challenge_token": setup["challenge_token"], "code": pyotp.TOTP(setup["secret"]).now(),
    }).status_code == 400


def test_challenge_is_not_an_access_token_and_access_token_is_not_a_challenge(account):
    client, _, payload = account
    setup, recovery, _ = enroll_totp(client)
    guest, challenge = login_challenge(payload)
    assert guest.get("/api/auth/me", headers={
        "Authorization": "Bearer " + challenge["challenge_token"],
    }).status_code == 401
    access = client.get_cookie("access_token_cookie").value
    assert verify(guest, {"challenge_token": access}, recovery[0]).status_code == 400
    assert verify(guest, setup, recovery[0]).status_code == 400
    altered = challenge["challenge_token"][:-5] + "abcde"
    assert verify(guest, {"challenge_token": altered}, recovery[0]).status_code == 400


def test_recovery_codes_are_single_use_and_bearer_transport_is_retained(account):
    client, _, payload = account
    _, recovery, _ = enroll_totp(client)
    guest, challenge = login_challenge({**payload, "token_transport": "header"})
    result = verify(guest, challenge, recovery[0])
    assert result.status_code == 200
    assert "access_token" in result.json
    assert not guest.get_cookie("access_token_cookie")
    assert guest.get("/api/auth/me", headers={
        "Authorization": "Bearer " + result.json["access_token"],
    }).status_code == 200
    guest, challenge = login_challenge(payload)
    assert verify(guest, challenge, recovery[0]).status_code == 400
    assert verify(guest, challenge, recovery[1]).status_code == 200


def test_failure_budget_is_persistent_across_new_password_logins(account):
    client, _, payload = account
    _, recovery, _ = enroll_totp(client)
    for _ in range(5):
        guest, challenge = login_challenge(payload)
        assert verify(guest, challenge, "invalid").status_code == 400
    response = webapp.app.test_client().post("/api/auth/login", json=payload)
    assert response.status_code == 429 and int(response.headers["Retry-After"]) > 0
    assert verify(guest, challenge, recovery[0]).status_code == 429


@pytest.mark.parametrize("change", ["password", "role", "inactive"])
def test_account_changes_invalidate_pending_challenges(account, change):
    client, user_id, payload = account
    _, recovery, _ = enroll_totp(client)
    guest, challenge = login_challenge(payload)
    values = {"token_version": users.c.token_version + 1}
    if change == "inactive":
        values["active"] = False
    elif change == "role":
        values["role"] = "admin"
    else:
        values["password_hash"] = webapp.password_hasher.hash("replacement-password")
    with webapp.transaction(webapp.engine) as db:
        db.execute(update(users).where(users.c.id == user_id).values(**values))
    assert verify(guest, challenge, recovery[0]).status_code == 401


def test_expired_and_superseded_challenges_fail(account):
    client, user_id, payload = account
    _, recovery, _ = enroll_totp(client)
    guest, first = login_challenge(payload)
    _, second = login_challenge(payload)
    assert verify(guest, first, recovery[0]).status_code == 400
    with webapp.transaction(webapp.engine) as db:
        db.execute(update(mfa_challenges).where(mfa_challenges.c.user_id == user_id).values(expires=1))
    assert verify(guest, second, recovery[0]).status_code == 400
    claims = webapp.mfa.decode_challenge(second)
    claims["exp"] = 1
    expired = jwt.encode(claims, webapp.mfa.key, algorithm="HS256")
    assert verify(guest, {"challenge_token": expired}, recovery[0]).status_code == 400


def test_disable_and_regeneration_require_both_factors_and_invalidate_sessions(account):
    client, _, payload = account
    _, recovery, _ = enroll_totp(client)
    guest, challenge = login_challenge(payload)
    assert verify(guest, challenge, recovery[0]).status_code == 200
    assert client.post("/api/auth/mfa/disable", json={"password": PASSWORD}).status_code == 400
    assert client.post("/api/auth/mfa/disable", json={"code": recovery[1]}).status_code == 400
    result = client.post("/api/auth/mfa/recovery", json={"password": PASSWORD, "code": recovery[1]})
    assert result.status_code == 200
    assert guest.get("/api/auth/me").status_code == 401
    replacement = result.json["recovery_codes"]
    assert not set(recovery) & set(replacement)
    guest, challenge = login_challenge(payload)
    assert verify(guest, challenge, recovery[2]).status_code == 400
    result = client.post("/api/auth/mfa/disable", json={"password": PASSWORD, "code": replacement[0]})
    assert result.status_code == 200
    assert client.get("/api/auth/mfa").json["method"] == ""
    assert verify(guest, challenge, replacement[1]).status_code == 401
    assert "mfa_required" not in guest.post("/api/auth/login", json=payload).json


def test_email_verification_resend_hashing_and_management(account, monkeypatch):
    client, user_id, payload = account
    sent = allow_email(monkeypatch)
    recovery = enroll_email(client, sent)
    assert sent[0][0] == "owner@example.com"
    assert client.get("/api/auth/mfa").json["email"] == "o***@example.com"
    clear_cooldown(user_id)
    guest, challenge = login_challenge(payload)
    first_code = sent[-1][1]
    with webapp.connection(webapp.engine) as db:
        row = db.execute(select(mfa_challenges).where(mfa_challenges.c.user_id == user_id)).first()
        assert row.code_hash != hashlib.sha256(first_code.encode()).hexdigest()
        assert row.code_hash != first_code
    response = guest.post("/api/auth/mfa/resend", json=challenge)
    assert response.status_code == 429 and "Retry-After" in response.headers
    clear_cooldown(user_id)
    response = guest.post("/api/auth/mfa/resend", json=challenge)
    assert response.status_code == 200
    replacement = response.json
    assert verify(guest, challenge, first_code).status_code == 400
    assert verify(guest, replacement, sent[-1][1]).status_code == 200
    assert verify(guest, replacement, sent[-1][1]).status_code == 400
    clear_cooldown(user_id)
    management = client.post("/api/auth/mfa/email", json={"password": PASSWORD}).json
    assert client.post("/api/auth/mfa/disable", json={
        "password": PASSWORD, "challenge_token": replacement["challenge_token"], "code": sent[-1][1],
    }).status_code == 400
    result = client.post("/api/auth/mfa/disable", json={
        "password": PASSWORD, "challenge_token": management["challenge_token"], "code": sent[-1][1],
    })
    assert result.status_code == 200


def test_email_outage_preserves_recovery_and_never_bypasses_mfa(account, monkeypatch):
    client, user_id, payload = account
    sent = allow_email(monkeypatch)
    recovery = enroll_email(client, sent)
    clear_cooldown(user_id)
    monkeypatch.setattr(mfa_module, "send_email_code", Mock(side_effect=smtplib.SMTPException("offline")))
    guest, challenge = login_challenge(payload)
    assert challenge["email_sent"] is False
    assert guest.get("/api/auth/me").status_code == 401
    assert verify(guest, challenge, recovery[0]).status_code == 200
    monkeypatch.delenv("SMTP_HOST")
    guest, challenge = login_challenge(payload)
    assert verify(guest, challenge, recovery[1]).status_code == 200


def test_mfa_csrf_and_anonymous_management_boundaries(account):
    client, _, payload = account
    webapp.app.config["JWT_COOKIE_CSRF_PROTECT"] = True
    try:
        assert client.post("/api/auth/login", json=payload).status_code == 200
        for endpoint in ("setup", "confirm", "email", "disable", "recovery"):
            assert client.post("/api/auth/mfa/" + endpoint, json={}).status_code == 401
            assert webapp.app.test_client().post("/api/auth/mfa/" + endpoint, json={}).status_code == 401
        csrf = client.get_cookie("csrf_access_token").value
        result = client.post("/api/auth/mfa/setup", json={"method": "totp", "password": PASSWORD},
                             headers={"X-CSRF-TOKEN": csrf})
        assert result.status_code == 200
    finally:
        webapp.app.config["JWT_COOKIE_CSRF_PROTECT"] = False


def test_concurrent_recovery_redemption_succeeds_only_once(account):
    client, _, payload = account
    _, recovery, _ = enroll_totp(client)
    _, challenge = login_challenge(payload)
    def redeem(_):
        return verify(webapp.app.test_client(), challenge, recovery[0]).status_code
    with ThreadPoolExecutor(max_workers=2) as pool:
        statuses = list(pool.map(redeem, range(2)))
    assert sorted(statuses) == [200, 400]


def test_smtp_requires_tls_and_does_not_send_without_it(monkeypatch):
    monkeypatch.setenv("SMTP_HOST", "smtp.example.com")
    monkeypatch.setenv("SMTP_FROM", "noreply@example.com")
    monkeypatch.setenv("SMTP_USERNAME", "mailer")
    monkeypatch.setenv("SMTP_PASSWORD", "smtp-password")
    monkeypatch.setenv("SMTP_SECURITY", "starttls")
    factory = Mock()
    smtp = factory.return_value.__enter__ = Mock(return_value=Mock())
    factory.return_value.__exit__ = Mock(return_value=False)
    monkeypatch.setattr(mfa_module.smtplib, "SMTP", factory)
    mfa_module.send_email_code("user@example.com", "123456")
    calls = smtp.return_value.method_calls
    assert [call[0] for call in calls] == ["starttls", "login", "send_message"]
    monkeypatch.setenv("SMTP_SECURITY", "none")
    with pytest.raises(RuntimeError):
        mfa_module.send_email_code("user@example.com", "123456")
    assert factory.call_count == 1


def test_enrollment_rejects_missing_keys_and_smtp_and_unsafe_addresses(account, monkeypatch):
    client, _, _ = account
    monkeypatch.setattr(webapp.mfa, "stable_keys", False)
    assert client.post("/api/auth/mfa/setup", json={
        "method": "totp", "password": PASSWORD,
    }).status_code == 503
    monkeypatch.setattr(webapp.mfa, "stable_keys", True)
    monkeypatch.delenv("SMTP_HOST", raising=False)
    assert client.post("/api/auth/mfa/setup", json={
        "method": "email", "email": "a@example.com", "password": PASSWORD,
    }).status_code == 503
    sent = allow_email(monkeypatch)
    for address in ("", "a@example.com\r\nBcc: victim@example.com", "a@example.com,b@example.com", {}, "a" * 65 + "@example.com"):
        assert client.post("/api/auth/mfa/setup", json={
            "method": "email", "email": address, "password": PASSWORD,
        }).status_code == 400
    assert not sent


def test_email_hourly_limit_is_shared_and_cannot_reset_with_new_challenges(account, monkeypatch):
    client, user_id, payload = account
    sent = allow_email(monkeypatch)
    recovery = enroll_email(client, sent)
    with webapp.transaction(webapp.engine) as db:
        db.execute(update(mfa_accounts).where(mfa_accounts.c.user_id == user_id).values(
            next_send=0, send_count=10, send_window=webapp.mfa.timestamp(),
        ))
    guest, challenge = login_challenge(payload)
    assert challenge["email_sent"] is False
    assert len(sent) == 1
    response = guest.post("/api/auth/mfa/resend", json=challenge)
    assert response.status_code == 429 and int(response.headers["Retry-After"]) > 3500
    assert verify(guest, challenge, recovery[0]).status_code == 200


def test_cannot_confirm_another_accounts_enrollment(account):
    client, user_id, _ = account
    setup = client.post("/api/auth/mfa/setup", json={"password": PASSWORD, "method": "totp"}).json
    admin = webapp.app.test_client()
    assert admin.post("/api/auth/login", json={
        "username": "admin", "password": "correct-horse-battery-staple",
    }).status_code == 200
    result = admin.post("/api/auth/mfa/confirm", json={
        "challenge_token": setup["challenge_token"], "code": pyotp.TOTP(setup["secret"]).now(),
    })
    assert result.status_code == 400
    assert client.get("/api/auth/mfa").json["method"] == ""


def test_administrator_password_reset_does_not_remove_mfa(account):
    client, user_id, payload = account
    _, recovery, _ = enroll_totp(client)
    guest, stale = login_challenge(payload)
    admin = webapp.app.test_client()
    assert admin.post("/api/auth/login", json={
        "username": "admin", "password": "correct-horse-battery-staple",
    }).status_code == 200
    new_password = "new-password-after-admin-reset"
    assert admin.patch("/api/users/" + user_id, json={"password": new_password}).status_code == 200
    assert verify(guest, stale, recovery[0]).status_code == 401
    guest, challenge = login_challenge({**payload, "password": new_password})
    assert verify(guest, challenge, recovery[0]).status_code == 200


def test_bearer_enrollment_returns_rotated_token_and_rejects_previous_token(account):
    client, _, payload = account
    bearer = webapp.app.test_client()
    token = bearer.post("/api/auth/login", json={**payload, "token_transport": "header"}).json["access_token"]
    headers = {"Authorization": "Bearer " + token}
    setup = bearer.post("/api/auth/mfa/setup", headers=headers, json={"method": "totp", "password": PASSWORD}).json
    response = bearer.post("/api/auth/mfa/confirm", headers=headers, json={
        "challenge_token": setup["challenge_token"], "code": pyotp.TOTP(setup["secret"]).now(),
    })
    assert response.status_code == 200
    assert "access_token" in response.json
    assert not bearer.get_cookie("access_token_cookie")
    assert bearer.get("/api/auth/me", headers=headers).status_code == 401
    assert bearer.get("/api/auth/me", headers={
        "Authorization": "Bearer " + response.json["access_token"],
    }).status_code == 200
