"""Regression tests for authentication, account-administration, settings, and upload fixes."""

import io
import threading
import time
import urllib.parse
import uuid
from datetime import timedelta

import jwt as pyjwt
import pytest
from sqlalchemy import delete, insert, select, update

# This harness sets DATA_DIR before importing the application, never using real data.
from test_webapp import webapp
from database import mfa_accounts, revoked_tokens, users


ADMIN = {"username": "admin", "password": "correct-horse-battery-staple"}
PASSWORD = "a-long-regression-password"
SUBTITLE = b"1\n00:00:01,000 --> 00:00:02,000\nHello\n\n"


@pytest.fixture(autouse=True)
def application(monkeypatch):
    webapp.app.config.update(TESTING=True, DEBUG=True, JWT_COOKIE_CSRF_PROTECT=False)
    monkeypatch.setattr(webapp, "verify_captcha", lambda *args: None)
    webapp.login_attempts.clear()
    webapp.registration_attempts.clear()
    yield
    webapp.login_attempts.clear()
    webapp.registration_attempts.clear()
    webapp.engine.dispose()


def sign_in(username, password=PASSWORD):
    client = webapp.app.test_client()
    response = client.post("/api/auth/login", json={"username": username, "password": password})
    assert response.status_code == 200, response.get_json()
    return client


@pytest.fixture
def admin():
    return sign_in(ADMIN["username"], ADMIN["password"])


@pytest.fixture
def make_user(admin):
    created = []

    def factory(role="user", prefix="fix"):
        username = f"{prefix}-{uuid.uuid4().hex[:12]}"
        response = admin.post("/api/users", json={
            "username": username, "password": PASSWORD, "role": role,
        })
        assert response.status_code == 201, response.get_json()
        created.append(response.get_json()["user"]["id"])
        return username, response.get_json()["user"]["id"]

    yield factory
    with webapp.transaction(webapp.engine) as db:
        db.execute(update(webapp.jobs).where(webapp.jobs.c.user_id.in_(created)).values(user_id=None))
        db.execute(delete(users).where(users.c.id.in_(created)))


def access_cookie(client):
    cookie = client.get_cookie("access_token_cookie")
    return cookie.value if cookie else None


def claims_of(token):
    return pyjwt.decode(token, webapp.jwt_secret, algorithms=["HS256"],
                        options={"verify_exp": False})


def set_cookie_tokens(response):
    prefix = "access_token_cookie="
    return [
        header[len(prefix):].split(";", 1)[0]
        for header in response.headers.getlist("Set-Cookie") if header.startswith(prefix)
    ]


def bearer_status(token):
    return webapp.app.test_client().get(
        "/api/auth/me", headers={"Authorization": f"Bearer {token}"},
    ).status_code


@pytest.fixture
def near_expiry(monkeypatch):
    """Make every cookie look close to expiry so the refresh hook re-issues it."""
    real_now = webapp.now_datetime

    def shift():
        monkeypatch.setattr(webapp, "now_datetime", lambda: real_now() + timedelta(minutes=25))

    return shift


# 1 + 4: refresh must not outlive a concurrent version change, deactivation, or logout.

@pytest.mark.parametrize("change", ["password", "deactivate", "logout"])
def test_refresh_does_not_mint_a_token_after_a_concurrent_revocation(
    change, make_user, near_expiry, monkeypatch,
):
    username, user_id = make_user()
    victim = sign_in(username)
    stolen = access_cookie(victim)
    near_expiry()
    real_verify = webapp.verify_jwt_in_request

    def verify_then_commit_concurrent_change(*args, **kwargs):
        result = real_verify(*args, **kwargs)
        # Commits after the hook verified the old token, before it re-issues one.
        with webapp.transaction(webapp.engine) as db:
            if change == "password":
                db.execute(update(users).where(users.c.id == user_id).values(
                    token_version=users.c.token_version + 1,
                ))
            elif change == "deactivate":
                db.execute(update(users).where(users.c.id == user_id).values(active=False))
            else:
                db.execute(insert(revoked_tokens).values(
                    jti=webapp.revoked_session_key(claims_of(stolen)["sid"]),
                    expires_at="9999-01-01T00:00:00+00:00", created_at=webapp.now(),
                ))
        return result

    monkeypatch.setattr(webapp, "verify_jwt_in_request", verify_then_commit_concurrent_change)
    response = victim.get("/api/settings")
    assert response.status_code == 200
    monkeypatch.setattr(webapp, "verify_jwt_in_request", real_verify)
    for token in set_cookie_tokens(response):
        assert bearer_status(token) == 401
    assert bearer_status(stolen) == 401


def test_refresh_keeps_the_session_id_and_token_version(make_user, near_expiry):
    username, _ = make_user()
    client = sign_in(username)
    original = claims_of(access_cookie(client))
    near_expiry()
    response = client.get("/api/auth/me")
    tokens = set_cookie_tokens(response)
    assert len(tokens) == 1
    refreshed = claims_of(tokens[0])
    assert refreshed["sid"] == original["sid"]
    assert refreshed["ver"] == original["ver"]
    assert refreshed["jti"] != original["jti"]


def test_logout_revokes_every_token_of_the_session_but_not_other_sessions(
    make_user, near_expiry,
):
    username, _ = make_user()
    client = sign_in(username)
    first = access_cookie(client)
    other_device = access_cookie(sign_in(username))
    near_expiry()
    client.get("/api/auth/me")
    refreshed = access_cookie(client)
    assert refreshed != first
    assert claims_of(refreshed)["sid"] == claims_of(first)["sid"]

    assert client.post("/api/auth/logout").status_code == 200
    assert bearer_status(first) == 401
    assert bearer_status(refreshed) == 401
    assert bearer_status(other_device) == 200
    # A request that verified before the logout cannot mint a usable token for it.
    late = webapp.refreshed_session_token(claims_of(first))
    assert late is None


def test_logout_also_revokes_legacy_tokens_without_a_session_claim(make_user):
    username, user_id = make_user()
    with webapp.app.app_context():
        with webapp.connection(webapp.engine) as db:
            row = db.execute(select(users).where(users.c.id == user_id)).first()
        legacy = webapp.create_access_token(identity=user_id, additional_claims={
            "role": row.role, "ver": row.token_version,
        })
        refreshed = webapp.refreshed_session_token(claims_of(legacy))
    assert claims_of(refreshed)["sid"] == claims_of(legacy)["jti"]
    response = webapp.app.test_client().post(
        "/api/auth/logout", headers={"Authorization": f"Bearer {refreshed}"},
    )
    assert response.status_code == 200
    assert bearer_status(legacy) == 401


# 5: signing in as somebody else must not be overwritten by the old cookie's refresh.

def test_login_as_another_user_replaces_a_near_expiry_cookie(make_user, near_expiry):
    first, _ = make_user()
    second, _ = make_user()
    client = sign_in(first)
    near_expiry()
    response = client.post("/api/auth/login", json={"username": second, "password": PASSWORD})
    assert response.status_code == 200, response.get_json()
    assert len(set_cookie_tokens(response)) == 1
    assert client.get("/api/auth/me").get_json()["user"]["username"] == second


# 2: two administrators removing each other concurrently must leave one.

@pytest.mark.parametrize("action", ["demote", "deactivate", "delete"])
def test_concurrent_mutual_admin_removal_keeps_one_active_admin(
    action, make_user, monkeypatch,
):
    first_name, first_id = make_user("admin")
    second_name, second_id = make_user("admin")
    with webapp.transaction(webapp.engine) as db:
        others = list(db.execute(select(users.c.id).where(
            users.c.role == "admin", users.c.active.is_(True),
            users.c.id.not_in([first_id, second_id]),
        )).scalars())
        db.execute(update(users).where(users.c.id.in_(others)).values(role="user"))
    try:
        clients = {first_id: sign_in(first_name), second_id: sign_in(second_name)}
        real_count = webapp.active_admin_count

        def slow_count(db):
            count = real_count(db)
            time.sleep(0.3)  # Widen the window between the check and the write.
            return count

        monkeypatch.setattr(webapp, "active_admin_count", slow_count)
        barrier = threading.Barrier(2)
        statuses = {}

        def remove(actor, target):
            barrier.wait()
            client = clients[actor]
            if action == "delete":
                response = client.delete(f"/api/users/{target}")
            else:
                body = {"role": "user"} if action == "demote" else {"active": False}
                response = client.patch(f"/api/users/{target}", json=body)
            statuses[actor] = response.status_code

        threads = [
            threading.Thread(target=remove, args=(first_id, second_id)),
            threading.Thread(target=remove, args=(second_id, first_id)),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(30)
        assert sorted(statuses.values())[0] == 200, statuses
        assert sorted(statuses.values())[1] in {403, 409}, statuses
        with webapp.connection(webapp.engine) as db:
            assert real_count(db) == 1
    finally:
        with webapp.transaction(webapp.engine) as db:
            db.execute(update(users).where(users.c.id.in_(others)).values(role="admin"))


# 3: account lockout reserves attempts before the slow password check.

class BlockingHasher:
    """Delegate to the real hasher, holding chosen verifications until released."""

    def __init__(self, real, should_block):
        self.real = real
        self.should_block = should_block
        self.release = threading.Event()
        self.lock = threading.Lock()
        self.blocked = 0
        self.real_hash_checks = 0
        self.target_hash = None

    def verify(self, password_hash, password):
        if password_hash == self.target_hash:
            with self.lock:
                self.real_hash_checks += 1
            if self.should_block(password):
                with self.lock:
                    self.blocked += 1
                self.release.wait(10)
        return self.real.verify(password_hash, password)

    def __getattr__(self, name):
        return getattr(self.real, name)


def wait_until(condition, timeout=10):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if condition():
            return True
        time.sleep(0.01)
    return False


def account_row(user_id):
    with webapp.connection(webapp.engine) as db:
        return db.execute(select(users).where(users.c.id == user_id)).first()


def test_parallel_guesses_cannot_exceed_the_lockout_budget(make_user, monkeypatch):
    username, user_id = make_user()
    hasher = BlockingHasher(webapp.password_hasher, lambda password: password != PASSWORD)
    hasher.target_hash = account_row(user_id).password_hash
    monkeypatch.setattr(webapp, "password_hasher", hasher)
    results = []

    def guess(index):
        response = webapp.app.test_client().post("/api/auth/login", json={
            "username": username, "password": f"wrong-password-{index:03d}",
        })
        results.append(response.status_code)

    threads = [threading.Thread(target=guess, args=(index,)) for index in range(9)]
    for thread in threads:
        thread.start()
    try:
        # Only the reserved budget reaches the real hash; the rest fail immediately.
        assert wait_until(lambda: hasher.blocked == webapp.LOGIN_FAILURE_LIMIT
                          and len(results) == 9 - webapp.LOGIN_FAILURE_LIMIT), (
            hasher.blocked, results)
        correct = webapp.app.test_client().post("/api/auth/login", json={
            "username": username, "password": PASSWORD,
        })
        assert correct.status_code == 401
    finally:
        hasher.release.set()
        for thread in threads:
            thread.join(30)
    assert results == [401] * 9
    assert hasher.real_hash_checks == webapp.LOGIN_FAILURE_LIMIT
    row = account_row(user_id)
    assert row.locked_until is not None
    assert webapp.app.test_client().post("/api/auth/login", json={
        "username": username, "password": PASSWORD,
    }).status_code == 401


def test_successful_login_cannot_clear_a_lock_set_while_it_verified(make_user, monkeypatch):
    username, user_id = make_user()
    hasher = BlockingHasher(webapp.password_hasher, lambda password: password == PASSWORD)
    hasher.target_hash = account_row(user_id).password_hash
    monkeypatch.setattr(webapp, "password_hasher", hasher)
    outcome = []
    thread = threading.Thread(target=lambda: outcome.append(
        webapp.app.test_client().post("/api/auth/login", json={
            "username": username, "password": PASSWORD,
        }).status_code
    ))
    thread.start()
    try:
        assert wait_until(lambda: hasher.blocked == 1)
        for _ in range(webapp.LOGIN_FAILURE_LIMIT):
            webapp.record_account_login_failure(user_id)
        assert account_row(user_id).locked_until is not None
    finally:
        hasher.release.set()
        thread.join(30)
    assert outcome == [401]
    assert account_row(user_id).locked_until is not None


def test_address_limiter_reserves_atomically_and_prunes_expired_addresses():
    limiter = webapp.AttemptLimiter(3, timedelta(minutes=15))
    barrier = threading.Barrier(20)
    reserved = []

    def attempt():
        barrier.wait()
        reserved.append(limiter.reserve("203.0.113.9"))

    threads = [threading.Thread(target=attempt) for _ in range(20)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert sum(marker is not None for marker in reserved) == 3
    assert limiter.limited("203.0.113.9")

    old = webapp.now_datetime() - timedelta(hours=1)
    for index in range(500):
        limiter.attempts[f"198.51.100.{index}"] = [old]
    limiter.next_prune = None
    assert limiter.reserve("192.0.2.1") is not None
    assert set(limiter.attempts) == {"203.0.113.9", "192.0.2.1"}

    marker = limiter.reserve("192.0.2.2")
    limiter.release("192.0.2.2", marker)
    assert "192.0.2.2" not in limiter.attempts


def test_successful_login_does_not_count_against_the_address_limit(make_user):
    username, _ = make_user()
    sign_in(username)
    assert not webapp.login_attempts


# 6 + 7: malformed JSON values are rejected instead of raising or being stored as text.

def test_unhashable_role_and_theme_values_are_rejected(admin, make_user):
    username, user_id = make_user()
    client = sign_in(username)
    for theme in ([], {}, None, True):
        assert client.patch("/api/auth/me", json={"theme": theme}).status_code == 400
    for role in ([], {}, True):
        assert admin.post("/api/users", json={
            "username": f"role-{uuid.uuid4().hex[:8]}", "password": PASSWORD, "role": role,
        }).status_code == 400
        assert admin.patch(f"/api/users/{user_id}", json={"role": role}).status_code == 400


@pytest.mark.parametrize("key", [
    "captcha_provider", "default_provider", "registration_enabled", "rpm", "width",
    "workers", "captcha_hostname", "openai_api_key", "source_language",
])
@pytest.mark.parametrize("value", [[], {}, None, True, False], ids=str)
def test_settings_reject_non_scalar_boolean_and_null_values(admin, key, value):
    before = webapp.read_settings(include_secrets=True)
    response = admin.put("/api/settings", json={key: value})
    assert response.status_code == 400, response.get_json()
    assert key in response.get_json()["error"]
    assert webapp.read_settings(include_secrets=True) == before


def test_numeric_settings_store_the_validated_text(admin):
    response = admin.put("/api/settings", json={"rpm": " 5 ", "workers": 3})
    assert response.status_code == 200, response.get_json()
    assert response.get_json()["rpm"] == "5"
    assert response.get_json()["workers"] == "3"
    admin.put("/api/settings", json={"rpm": "0", "workers": "4"})


# 8: usernames follow the documented 3-64 rule on creation only.

def test_username_rule_matches_the_documented_length(admin, make_user):
    for name in ("a", "ab", "a" * 65, "-ab", "ab-"):
        assert webapp.validate_username(name), name
    for name in ("abc", "a.b", "a" * 64, "a_b-c.d"):
        assert webapp.validate_username(name) is None, name
    assert admin.post("/api/users", json={
        "username": "ab", "password": PASSWORD,
    }).status_code == 400

    # An account created under the old rule keeps signing in.
    user_id = uuid.uuid4().hex
    with webapp.transaction(webapp.engine) as db:
        webapp.insert_user(db, user_id, "x", PASSWORD, "user", webapp.now())
    try:
        sign_in("x")
    finally:
        with webapp.transaction(webapp.engine) as db:
            db.execute(delete(users).where(users.c.id == user_id))


# 9: Unicode upload names are preserved safely.

@pytest.mark.parametrize(("raw", "expected"), [
    ("字幕.srt", "字幕.srt"),
    ("Фильм.vtt", "Фильм.vtt"),
    ("映画 第1話.ass", "映画 第1話.ass"),
    ("Ame\u0301lie.srt", "Am\u00e9lie.srt"),
    ("Movie.SRT", "Movie.SRT"),
    ("../../etc/passwd.srt", "passwd.srt"),
    ("C:\\Users\\me\\episode.ssa", "episode.ssa"),
    ('a"b\'c<d>e:f|g?h*.srt', "abcdefgh.srt"),
    (".hidden.srt", "hidden.srt"),
    ("\u202eevil\x00\x07.srt", "evil.srt"),
    ("tab\there\u3000wide.srt", "tabhere wide.srt"),
    ("..srt", "subtitle.srt"),
    ("CON.srt", "_CON.srt"),
    ("notes.txt", None),
    ("字幕", None),
    ("", None),
])
def test_upload_display_names(raw, expected):
    assert webapp.upload_display_name(raw) == expected


def test_upload_display_name_caps_the_encoded_length():
    name = webapp.upload_display_name("字" * 300 + ".srt")
    assert name.endswith(".srt")
    assert len(name.removesuffix(".srt").encode("utf-8")) <= webapp.MAX_FILENAME_STEM_BYTES


def wait_for_job(client, job_id):
    for _ in range(300):
        job = client.get(f"/api/jobs/{job_id}").get_json()
        if job["status"] in webapp.TERMINAL_STATUSES:
            return job
        time.sleep(0.05)
    raise AssertionError(f"job {job_id} did not finish: {job}")


def submit(client, filename, targets):
    return client.post("/api/jobs", data={
        "files": (io.BytesIO(SUBTITLE), filename), "provider": "echo",
        "target_languages": targets,
    }, content_type="multipart/form-data")


def test_unicode_upload_round_trips_to_downloads(admin):
    response = submit(admin, "字幕 第1話.srt", "ja,zh-TW")
    assert response.status_code == 202, response.get_json()
    job_id = response.get_json()["jobs"][0]
    try:
        job = wait_for_job(admin, job_id)
        assert job["status"] == "completed", job["error"]
        assert job["filename"] == "字幕 第1話.srt"
        assert (webapp.JOBS_DIR / job_id / "source.srt").exists()
        output = job["outputs"][0]["name"]
        assert output.startswith("字幕 第1話.")
        download = admin.get(f"/api/jobs/{job_id}/download/{urllib.parse.quote(output)}")
        assert download.status_code == 200
        disposition = download.headers["Content-Disposition"]
        assert "filename*=UTF-8''" + urllib.parse.quote(output) in disposition
        archive = admin.get(f"/api/jobs/{job_id}/download")
        assert archive.status_code == 200
        assert urllib.parse.quote("字幕 第1話.translations.zip") in archive.headers[
            "Content-Disposition"
        ]
    finally:
        admin.delete(f"/api/jobs/{job_id}")


# 10: repeated target languages are translated once; the count is bounded.

def test_duplicate_target_languages_are_collapsed(admin, monkeypatch):
    response = submit(admin, "dupes.srt", ",".join(["ja"] * 50 + ["zh-TW", "ja"]))
    assert response.status_code == 202, response.get_json()
    job_id = response.get_json()["jobs"][0]
    try:
        job = wait_for_job(admin, job_id)
        assert job["status"] == "completed", job["error"]
        assert job["options"]["target_languages"] == ["ja", "zh-TW"]
        assert [output["language"] for output in job["outputs"]] == ["ja", "zh-TW"]
    finally:
        admin.delete(f"/api/jobs/{job_id}")

    monkeypatch.setattr(webapp, "MAX_TARGET_LANGUAGES", 1)
    rejected = submit(admin, "many.srt", "ja,zh-TW")
    assert rejected.status_code == 400
    assert "at most 1" in rejected.get_json()["error"]


# 11: an administrator unlock also clears the MFA verification lock.

def test_admin_unlock_clears_the_mfa_lock(admin, make_user):
    _username, user_id = make_user()
    locks = {
        column: value for column, value in (
            ("failures", 3), ("locked_until", int(time.time()) + 900),
            ("manage_failures", 2), ("manage_locked_until", int(time.time()) + 900),
        ) if column in mfa_accounts.c
    }
    with webapp.transaction(webapp.engine) as db:
        db.execute(insert(mfa_accounts).values(
            user_id=user_id, method="totp", secret="", **locks,
        ))
        db.execute(update(users).where(users.c.id == user_id).values(
            failed_login_count=4, locked_until="9999-01-01T00:00:00+00:00",
        ))
    response = admin.patch(f"/api/users/{user_id}", json={"unlock": True})
    assert response.status_code == 200, response.get_json()
    assert response.get_json()["user"]["locked"] is False
    with webapp.connection(webapp.engine) as db:
        account = db.execute(select(mfa_accounts).where(
            mfa_accounts.c.user_id == user_id,
        )).first()
    assert account.method == "totp"
    assert {column: getattr(account, column) for column in locks} == dict.fromkeys(locks, 0)


# 12: an active CAPTCHA provider requires an expected hostname on save.

def test_active_captcha_provider_requires_a_hostname(admin):
    configuration = {
        "turnstile_site_key": "site-key", "turnstile_secret_key": "secret-key",
    }
    try:
        assert admin.put("/api/settings", json={
            "captcha_provider": "none", "captcha_hostname": "",
        }).status_code == 200
        rejected = admin.put("/api/settings", json={
            "captcha_provider": "turnstile", **configuration,
        })
        assert rejected.status_code == 400
        assert "hostname" in rejected.get_json()["error"]
        assert admin.get("/api/settings").get_json()["captcha_provider"] == "none"

        accepted = admin.put("/api/settings", json={
            "captcha_provider": "turnstile", "captcha_hostname": "Translate.Example.",
            **configuration,
        })
        assert accepted.status_code == 200, accepted.get_json()
        assert accepted.get_json()["captcha_hostname"] == "translate.example"

        # Payloads are partial: clearing only the hostname is judged on the merged state.
        cleared = admin.put("/api/settings", json={"captcha_hostname": " "})
        assert cleared.status_code == 400
        assert admin.get("/api/settings").get_json()["captcha_hostname"] == "translate.example"
    finally:
        assert admin.put("/api/settings", json={
            "captcha_provider": "none", "captcha_hostname": "",
        }).status_code == 200
