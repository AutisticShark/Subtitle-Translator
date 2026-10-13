"""Regression tests for the 2026-10 code-quality audit of webapp.py."""

import io
import json

from sqlalchemy import delete, insert

# test_fixes_auth sets DATA_DIR (through test_webapp) before importing the application.
import test_fixes_auth as auth
from test_fixes_auth import ADMIN, PASSWORD, submit, wait_for_job, webapp
from database import bump_cache_revision, settings as settings_table

# Reuse its fixtures (the autouse one included) under the names tests request.
application, admin, make_user = auth.application, auth.admin, auth.make_user

# The autouse fixture replaces verify_captcha; keep the real one for the login path.
REAL_VERIFY_CAPTCHA = webapp.verify_captcha


def store_setting(name, value):
    with webapp.transaction(webapp.engine) as db:
        db.execute(delete(settings_table).where(settings_table.c.name == name))
        if value is not None:
            db.execute(insert(settings_table).values(
                name=name, value=value, updated_at=webapp.now(),
            ))
        bump_cache_revision(db, "settings")


def test_unreadable_unrelated_secret_does_not_break_login_or_jobs(admin, monkeypatch):
    monkeypatch.setattr(webapp, "verify_captcha", REAL_VERIFY_CAPTCHA)
    store_setting("google_api_key", "enc:v1:not-a-valid-token")
    try:
        client = webapp.app.test_client()
        response = client.post("/api/auth/login", json=ADMIN)
        assert response.status_code == 200, response.get_json()

        job_response = submit(client, "secret.srt", "ja")
        assert job_response.status_code == 202, job_response.get_json()
        job_id = job_response.get_json()["jobs"][0]
        job = wait_for_job(client, job_id)
        assert job["status"] == "completed", job["error"]
        client.delete(f"/api/jobs/{job_id}")
    finally:
        store_setting("google_api_key", None)


def test_unlock_must_be_a_boolean(admin, make_user):
    _username, user_id = make_user()
    response = admin.patch(f"/api/users/{user_id}", json={"unlock": "true"})
    assert response.status_code == 400
    assert response.get_json()["error"] == "unlock must be a boolean"
    assert admin.patch(f"/api/users/{user_id}", json={"unlock": True}).status_code == 200


def test_chunked_auth_body_is_capped_without_content_length():
    body = json.dumps({"username": "x" * 20000, "password": PASSWORD}).encode()
    response = webapp.app.test_client().post(
        "/api/auth/login", input_stream=io.BytesIO(body),
        headers={"Content-Type": "application/json", "Transfer-Encoding": "chunked"},
        environ_overrides={"wsgi.input_terminated": True},
    )
    assert response.status_code == 413
    assert response.get_json()["error"] == "Authentication request is too large"


def test_missing_output_files_are_not_found_rather_than_server_errors(admin):
    job_id = submit(admin, "gone.srt", "ja,zh-TW").get_json()["jobs"][0]
    try:
        job = wait_for_job(admin, job_id)
        assert job["status"] == "completed", job["error"]
        for output in job["outputs"]:
            (webapp.JOBS_DIR / job_id / output["name"]).unlink()
        name = job["outputs"][0]["name"]
        assert admin.get(f"/api/jobs/{job_id}/download/{name}").status_code == 404
        assert admin.get(f"/api/jobs/{job_id}/download").status_code == 404
    finally:
        admin.delete(f"/api/jobs/{job_id}")


def test_captcha_secret_removal_is_checked_against_current_sql_state(admin, monkeypatch):
    """The guard reads SQL inside its locked transaction, not a stale cached snapshot."""
    store_setting("turnstile_secret_key", webapp.encrypt_secret("secret"))
    stale = webapp.read_settings()
    for name, value in (("turnstile_site_key", "site"), ("captcha_hostname", "example.com"),
                        ("captcha_provider", "turnstile"), ("captcha_on_login", "1")):
        store_setting(name, value)
    monkeypatch.setattr(webapp, "read_settings", lambda *args, **kwargs: stale)
    try:
        response = admin.delete("/api/settings/keys/captcha-turnstile")
        assert response.status_code == 409, response.get_json()
    finally:
        for name in ("turnstile_secret_key", "turnstile_site_key", "captcha_hostname",
                     "captcha_provider", "captcha_on_login"):
            store_setting(name, None)
