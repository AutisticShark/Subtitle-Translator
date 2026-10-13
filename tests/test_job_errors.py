"""Job failure details: submitters see their own file problems, not provider responses."""

import io

# test_fixes_auth sets DATA_DIR (through test_webapp) before importing the application.
import test_fixes_auth as auth
from test_fixes_auth import ADMIN, sign_in, wait_for_job, webapp
from srt_translate import FatalTranslationError

application, admin, make_user = auth.application, auth.admin, auth.make_user

PROVIDER_ERROR = "HTTP 401: Incorrect API key provided: sk-proj-abc***xyz"
GENERIC = "Translation failed. An administrator can see the details in the translation history."


def submit(client, content=auth.SUBTITLE):
    response = client.post("/api/jobs", data={
        "files": (io.BytesIO(content), "errors.srt"), "provider": "echo",
        "target_languages": "ja",
    }, content_type="multipart/form-data")
    assert response.status_code == 202, response.get_json()
    return response.get_json()["jobs"][0]


def failing_provider(*_args, **_kwargs):
    def provider(texts, target, source):
        raise FatalTranslationError(PROVIDER_ERROR)
    return provider


def test_provider_failure_details_are_for_administrators_only(make_user, monkeypatch):
    username, _ = make_user()
    client = sign_in(username)
    monkeypatch.setattr(webapp, "provider_for", failing_provider)
    job_id = submit(client)
    try:
        job = wait_for_job(client, job_id)
        assert job["status"] == "failed"
        assert job["error"] == GENERIC
        listed = client.get("/api/jobs").get_json()["jobs"]
        assert [item["error"] for item in listed if item["id"] == job_id] == [GENERIC]
        assert PROVIDER_ERROR not in client.get(
            f"/api/jobs/{job_id}?lang=zh-TW"
        ).get_data(as_text=True)

        administrator = sign_in(ADMIN["username"], ADMIN["password"])
        detail = administrator.get(f"/api/jobs/{job_id}").get_json()
        assert detail["error"] == f"FatalTranslationError: {PROVIDER_ERROR}"
        listed = administrator.get("/api/jobs?all=1").get_json()["jobs"]
        assert [item["error"] for item in listed if item["id"] == job_id] == [detail["error"]]
    finally:
        client.delete(f"/api/jobs/{job_id}")


def test_submitters_still_see_problems_with_their_own_file(make_user):
    username, _ = make_user()
    client = sign_in(username)
    job_id = submit(client, b"not a subtitle")
    try:
        job = wait_for_job(client, job_id)
        assert job["status"] == "failed"
        assert job["error"].startswith("SubtitleFormatError: ")
        assert "No subtitle cues" in job["error"]
    finally:
        client.delete(f"/api/jobs/{job_id}")


def test_visible_job_error_allows_only_file_problems():
    user = type("Viewer", (), {"role": "user"})()
    administrator = type("Viewer", (), {"role": "admin"})()
    with webapp.app.test_request_context():
        assert webapp.visible_job_error(None, user) is None
        assert webapp.visible_job_error("UnicodeDecodeError: bad byte", user) == "UnicodeDecodeError: bad byte"
        assert webapp.visible_job_error("LookupError: unknown encoding: x", user).startswith("LookupError")
        for stored in ("TranslationError: network error: refused", "RuntimeError: boom",
                       "SubtitleFormatErrorX: spoofed", "no colon at all"):
            assert webapp.visible_job_error(stored, user) == GENERIC
            assert webapp.visible_job_error(stored, administrator) == stored
