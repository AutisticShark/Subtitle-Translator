"""Job-list pagination/aggregates and locale-prefix negotiation used by the web UI."""

import json
import uuid
from unittest.mock import Mock

import pytest
from sqlalchemy import delete, func, insert, select

# This harness sets DATA_DIR before importing the application, never using real data.
from test_webapp import webapp
import i18n
from database import jobs, users


PASSWORD = "a-long-pagination-test-password"


def make_user(role="user"):
    user_id = uuid.uuid4().hex
    username = f"{role}-{user_id[:16]}"
    with webapp.transaction(webapp.engine) as db:
        db.execute(insert(users).values(
            id=user_id, username=username, password_hash=webapp.password_hasher.hash(PASSWORD),
            role=role, created_at=webapp.now(), updated_at=webapp.now(),
        ))
    return user_id, username


def login(username):
    client = webapp.app.test_client()
    response = client.post("/api/auth/login", json={"username": username, "password": PASSWORD})
    assert response.status_code == 200, response.json
    return client


def insert_jobs(user_id, statuses, prefix):
    """Insert jobs oldest first and return their ids newest first."""
    ids = []
    with webapp.transaction(webapp.engine) as db:
        for index, status in enumerate(statuses):
            job_id = uuid.uuid4().hex
            stamp = f"2026-01-01T00:{index:02d}:00+00:00"
            db.execute(insert(jobs).values(
                id=job_id, user_id=user_id, filename=f"{prefix}-{index}.srt",
                stored_name="source.srt", status=status, options=json.dumps({
                    "provider": "echo", "target_languages": ["fr"],
                }), outputs="[]", created_at=stamp, updated_at=stamp,
            ))
            ids.append(job_id)
        webapp.bump_cache_revision(db, "jobs")
    return ids[::-1]


@pytest.fixture
def accounts(monkeypatch):
    webapp.app.config.update(TESTING=True, JWT_COOKIE_CSRF_PROTECT=False)
    monkeypatch.setattr(webapp, "verify_captcha", lambda *args: None)
    webapp.login_attempts.clear()
    owner_id, owner_name = make_user()
    other_id, _other_name = make_user()
    admin_id, admin_name = make_user("admin")
    owner_statuses = ["completed", "failed", "completed", "queued", "canceled", "processing",
                      "completed"]
    owner_jobs = insert_jobs(owner_id, owner_statuses, "owner")
    other_jobs = insert_jobs(other_id, ["completed", "failed", "canceling"], "other")
    yield {
        "owner": login(owner_name), "admin": login(admin_name),
        "owner_jobs": owner_jobs, "other_jobs": other_jobs, "owner_statuses": owner_statuses,
    }
    with webapp.transaction(webapp.engine) as db:
        db.execute(delete(jobs).where(jobs.c.id.in_(owner_jobs + other_jobs)))
        db.execute(delete(users).where(users.c.id.in_([owner_id, other_id, admin_id])))
        webapp.bump_cache_revision(db, "jobs")
    webapp.engine.dispose()


def expected_counts(statuses):
    counts = {}
    for status in statuses:
        counts[status] = counts.get(status, 0) + 1
    return counts


def test_user_list_pages_through_every_job_with_scoped_aggregates(accounts):
    client = accounts["owner"]
    seen = []
    offset = 0
    while True:
        page = client.get(f"/api/jobs?limit=3&offset={offset}").json
        assert page["total"] == 7
        assert page["counts"] == expected_counts(accounts["owner_statuses"])
        assert page["offset"] == offset and page["limit"] == 3
        seen.extend(job["id"] for job in page["jobs"])
        assert page["has_more"] == (len(seen) < 7)
        if not page["has_more"]:
            break
        offset += 3
    assert seen == accounts["owner_jobs"]
    assert "owner" not in page["jobs"][0]

    # A regular user cannot widen the scope or the counts with ?all=1.
    widened = client.get("/api/jobs?all=1&limit=200").json
    assert {job["id"] for job in widened["jobs"]} == set(accounts["owner_jobs"])
    assert widened["total"] == 7


def test_list_parameters_are_validated_and_bounded(accounts):
    client = accounts["owner"]
    default = client.get("/api/jobs").json
    assert (default["offset"], default["limit"]) == (0, 50)
    garbage = client.get("/api/jobs?limit=lots&offset=later").json
    assert (garbage["offset"], garbage["limit"]) == (0, 50)
    clamped = client.get("/api/jobs?limit=100000&offset=-4").json
    assert (clamped["offset"], clamped["limit"]) == (0, 200)
    assert client.get("/api/jobs?limit=0").json["limit"] == 1
    beyond = client.get("/api/jobs?offset=99999999999").json
    assert beyond["offset"] == webapp.JOB_LIST_MAX_OFFSET
    assert beyond["jobs"] == [] and beyond["has_more"] is False and beyond["total"] == 7


def test_admin_panel_view_reports_panel_wide_aggregates(accounts):
    client = accounts["admin"]
    with webapp.connection(webapp.engine) as db:
        expected = {
            row.status: row.count for row in db.execute(
                select(jobs.c.status, func.count().label("count")).group_by(jobs.c.status)
            )
        }
    panel = client.get("/api/jobs?all=1&limit=2").json
    assert panel["counts"] == expected
    assert panel["total"] == sum(expected.values())
    assert len(panel["jobs"]) == 2 and panel["has_more"] is True
    assert "owner" in panel["jobs"][0]
    # Without ?all=1 an administrator sees only their own (here: no) jobs.
    own = client.get("/api/jobs").json
    assert own == {"jobs": [], "total": 0, "counts": {}, "offset": 0, "limit": 50,
                   "has_more": False}


def test_cached_pages_are_keyed_by_offset_and_counts_follow_revisions(accounts, monkeypatch):
    redis = Mock()
    redis.values = {}
    redis.get.side_effect = redis.values.get

    def put(key, value, *, ex):
        redis.values[key] = value
        return True

    redis.set.side_effect = put
    monkeypatch.setattr(webapp, "read_cache", webapp.RedisCache(
        redis, webapp.secret_cipher, "ui-fix-tests",
    ))
    client = accounts["owner"]
    first = client.get("/api/jobs?limit=3").json
    second = client.get("/api/jobs?limit=3&offset=3").json
    assert not {job["id"] for job in first["jobs"]} & {job["id"] for job in second["jobs"]}
    assert client.get("/api/jobs?limit=3&offset=3").json == second

    owner_id = None
    with webapp.connection(webapp.engine) as db:
        owner_id = db.scalar(select(jobs.c.user_id).where(
            jobs.c.id == accounts["owner_jobs"][0]))
    newest = insert_jobs(owner_id, ["queued"], "fresh")
    accounts["owner_jobs"].extend(newest)  # cleaned up by the fixture
    refreshed = client.get("/api/jobs?limit=3").json
    assert refreshed["total"] == 8
    assert refreshed["counts"]["queued"] == 2


@pytest.mark.parametrize(("tag", "expected"), [
    ("zh-Hant-HK", "zh-TW"), ("zh-Hant-MO", "zh-TW"), ("zh_hant_tw", "zh-TW"),
    ("zh-HK", "zh-TW"), ("zh-MO", "zh-TW"), ("zh-Hans-HK", "zh-CN"), ("zh-Hans-CN", "zh-CN"),
    ("zh", "zh-CN"), ("en-Latn-US", "en"), ("fr-CA", None),
])
def test_locale_tags_fall_back_through_progressively_shorter_prefixes(tag, expected):
    assert i18n.normalize_locale(tag) == expected


def test_long_traditional_chinese_tags_apply_to_every_locale_source():
    webapp.app.config.update(TESTING=True)
    client = webapp.app.test_client()
    assert client.get("/api/i18n?lang=zh-Hant-HK").json["locale"] == "zh-TW"
    client.set_cookie(i18n.LOCALE_COOKIE, "zh-Hant-MO")
    assert client.get("/api/i18n").json["locale"] == "zh-TW"
    browser = webapp.app.test_client()
    response = browser.get("/api/i18n", headers={"Accept-Language": "zh-Hant-HK, en;q=0.5"})
    assert response.json["locale"] == "zh-TW"
