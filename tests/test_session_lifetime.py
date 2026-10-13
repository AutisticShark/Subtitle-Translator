"""Absolute browser-session lifetime (SESSION_MAX_HOURS)."""

import os
import subprocess
import sys
from datetime import timedelta
from pathlib import Path

from sqlalchemy import select

# test_fixes_auth sets DATA_DIR (through test_webapp) before importing the application.
import test_fixes_auth as auth
from test_fixes_auth import access_cookie, claims_of, set_cookie_tokens, sign_in, webapp
from database import users

application, admin, make_user, near_expiry = (
    auth.application, auth.admin, auth.make_user, auth.near_expiry,
)


def test_sign_in_starts_a_session_and_caps_the_token_at_its_deadline(make_user, monkeypatch):
    username, _ = make_user()
    claims = claims_of(access_cookie(sign_in(username)))
    assert abs(claims["sst"] - claims["iat"]) <= 2
    assert claims["exp"] - claims["iat"] == 30 * 60  # the access lifetime, well inside 12 h

    monkeypatch.setattr(webapp, "SESSION_MAX_AGE", timedelta(minutes=10))
    short = claims_of(access_cookie(sign_in(username)))
    assert short["exp"] <= short["sst"] + 10 * 60


def test_refresh_keeps_the_session_start(make_user, near_expiry):
    username, _ = make_user()
    client = sign_in(username)
    original = claims_of(access_cookie(client))
    near_expiry()
    tokens = set_cookie_tokens(client.get("/api/auth/me"))
    assert len(tokens) == 1
    refreshed = claims_of(tokens[0])
    assert refreshed["sst"] == original["sst"]
    assert refreshed["exp"] <= original["sst"] + webapp.SESSION_MAX_AGE.total_seconds()


def test_refresh_stops_at_the_absolute_lifetime(make_user, near_expiry, monkeypatch):
    username, _ = make_user()
    monkeypatch.setattr(webapp, "SESSION_MAX_AGE", timedelta(minutes=20))
    client = sign_in(username)
    near_expiry()  # 25 minutes later: past the 20-minute session lifetime
    response = client.get("/api/auth/me")
    assert response.status_code == 200  # the current token is still valid until it expires
    assert set_cookie_tokens(response) == []


def test_tokens_without_a_session_start_are_bounded_from_their_issue_time(make_user):
    _username, user_id = make_user()
    with webapp.app.app_context():
        with webapp.connection(webapp.engine) as db:
            row = db.execute(select(users).where(users.c.id == user_id)).first()
        legacy = webapp.create_access_token(identity=user_id, additional_claims={
            "role": row.role, "ver": row.token_version, "sid": "legacy-session",
        })
        refreshed = claims_of(webapp.refreshed_session_token(claims_of(legacy)))
    assert refreshed["sst"] == claims_of(legacy)["iat"]
    assert refreshed["sid"] == "legacy-session"


def test_session_lifetime_setting_is_bounded(tmp_path):
    root = Path(__file__).resolve().parents[1]
    for value, ok in (("0", False), ("721", False), ("1", True), ("720", True)):
        environment = {**os.environ, "DATA_DIR": str(tmp_path / value), "SESSION_MAX_HOURS": value}
        result = subprocess.run(
            [sys.executable, "-c", "import webapp; print(webapp.SESSION_MAX_AGE)"],
            cwd=root, env=environment, capture_output=True, text=True, timeout=120,
        )
        assert (result.returncode == 0) is ok, result.stderr[-500:]
        if not ok:
            assert "SESSION_MAX_HOURS must be between 1 and 720" in result.stderr
