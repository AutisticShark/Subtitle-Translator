"""Flask application for authenticated browser-based subtitle translation."""

from __future__ import annotations

import base64
import hashlib
import json
import logging
import os
import re
import secrets
import shutil
import sqlite3
import tempfile
import threading
import time
import unicodedata
import uuid
import zipfile
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
from functools import wraps
from pathlib import Path
from typing import Any, Callable

from argon2 import PasswordHasher
from argon2.exceptions import InvalidHashError, VerificationError, VerifyMismatchError
from cryptography.fernet import Fernet, InvalidToken
from flask import Flask, jsonify, make_response, render_template, request, send_file
from flask_jwt_extended import (
    JWTManager, create_access_token, get_jwt, get_jwt_identity, jwt_required,
    get_jwt_request_location, set_access_cookies, unset_jwt_cookies,
    verify_jwt_in_request,
)
from sqlalchemy import delete, func, insert, or_, select, update
from sqlalchemy.exc import IntegrityError, SQLAlchemyError
from werkzeug.middleware.proxy_fix import ProxyFix

from database import (
    bump_cache_revision, cache_revisions,
    connection, create_database_engine, initialize_database, jobs, mfa_accounts,
    retry_database_race, revoked_tokens, rate_limit_buckets,
    settings as settings_table, startup_recovery_completed, transaction, users,
)
from redis_cache import RedisCache
from mfa import MFA
from i18n import (
    LOCALE_COOKIE, LOCALE_LABELS, current_locale, messages_for, normalize_locale,
    translate as tr,
)
from srt_translate import (
    LANGS, FatalTranslationError, Throttle, TranslationCanceled, make_anthropic,
    make_deepl, make_echo, make_google, make_openai, rebuild_cues, segment_cue,
    translate_segments,
)
import captcha
import quotas
from settings_schema import (
    BOOLEAN_SETTINGS, CALENDAR_LIMIT_KEYS, CALENDAR_PERIODS, CAPTCHA_ACTION_SETTINGS,
    CAPTCHA_PROVIDERS, RATE_LIMIT_KEYS, WINDOW_LIMIT_KEYS, validate_choice_and_flag_settings,
    validate_numeric_settings,
)
from subtitle_formats import SUPPORTED_EXTENSIONS, load_subtitle, translated_filename
from timeutil import parse_timestamp


LOGGER = logging.getLogger(__name__)
BASE_DIR = Path(__file__).resolve().parent
DATA_DIR = Path(os.environ.get("DATA_DIR", BASE_DIR / "data")).resolve()
JOBS_DIR = DATA_DIR / "jobs"
DB_PATH = DATA_DIR / "app.db"
MAX_UPLOAD_MB = int(os.environ.get("MAX_UPLOAD_MB", "200"))
# 3-64 characters that start and end with a letter or digit. Only account creation
# validates this; existing accounts keep signing in with their stored names.
USERNAME_PATTERN = re.compile(r"^[a-z0-9][a-z0-9_.-]{1,62}[a-z0-9]$")
PASSWORD_MIN_LENGTH = 12
LOGIN_FAILURE_LIMIT = 5
LOGIN_LOCK_MINUTES = 15
LOGIN_RATE_LIMIT = 30
REGISTER_RATE_LIMIT = 10
ATTEMPT_PRUNE_SECONDS = 60
MAX_TARGET_LANGUAGES = 20
MAX_FILENAME_STEM_BYTES = 150
FALLBACK_FILENAME_STEM = "subtitle"
UNSAFE_FILENAME_CHARACTERS = frozenset('"\'<>:|?*\\/')
WINDOWS_RESERVED_STEMS = frozenset({
    "con", "prn", "aux", "nul",
    *(f"com{number}" for number in range(1, 10)),
    *(f"lpt{number}" for number in range(1, 10)),
})
# Every DEFAULTS row is created at startup and never deleted, so locking this row
# serializes changes that could remove the final active administrator.
ADMIN_ROSTER_LOCK_SETTING = "registration_enabled"
REVOKED_SESSION_PREFIX = "sid:"
# Responses from these endpoints establish or end a session themselves; refreshing
# the request's previous cookie would overwrite (or resurrect) that decision.
NO_TOKEN_REFRESH_ENDPOINTS = {"logout", "login", "register", "setup_first_admin"}
TERMINAL_STATUSES = {"completed", "failed", "canceled"}
ACTIVE_STATUSES = {"queued", "processing", "canceling"}
ACCOUNT_THEMES = {"system", "light", "dark"}


def environment_flag(name: str, default: bool = False) -> bool:
    value = os.environ.get(name)
    if value is None:
        return default
    return value.strip().lower() in {"1", "true", "yes", "on"}


def now_datetime() -> datetime:
    return datetime.now(timezone.utc)


def now() -> str:
    return now_datetime().isoformat(timespec="seconds")


DEFAULTS = {
    "default_provider": "anthropic",
    "anthropic_model": "claude-sonnet-4-6",
    "openai_model": "gpt-5-mini",
    "openai_base_url": "https://api.openai.com/v1",
    "source_language": "English",
    "target_languages": "zh-TW,zh-CN",
    "batch_size": "20",
    "workers": "4",
    "rpm": "0",
    "width": "16",
    "max_lines": "2",
    "rate_limit_window_minutes": "60",
    "user_job_limit": "0",
    "admin_job_limit": "0",
    "panel_job_limit": "0",
    "registration_enabled": os.environ.get("REGISTRATION_ENABLED", "1").strip(),
    "captcha_provider": os.environ.get("CAPTCHA_PROVIDER", "none").strip().lower(),
    "captcha_hostname": os.environ.get("CAPTCHA_HOSTNAME", "").strip().lower(),
    "captcha_on_login": os.environ.get("CAPTCHA_ON_LOGIN", "1").strip(),
    "captcha_on_register": os.environ.get("CAPTCHA_ON_REGISTER", "1").strip(),
    "captcha_on_upload": os.environ.get("CAPTCHA_ON_UPLOAD", "1").strip(),
    "turnstile_site_key": os.environ.get("TURNSTILE_SITE_KEY", "").strip(),
    "recaptcha_site_key": os.environ.get("RECAPTCHA_SITE_KEY", "").strip(),
    "hcaptcha_site_key": os.environ.get("HCAPTCHA_SITE_KEY", "").strip(),
}
DEFAULTS.update({key: "0" for key in CALENDAR_LIMIT_KEYS})
SECRET_KEYS = {
    "anthropic_api_key", "openai_api_key", "deepl_api_key", "google_api_key",
    "turnstile_secret_key", "recaptcha_secret_key", "hcaptcha_secret_key",
}
PUBLIC_KEYS = set(DEFAULTS)
ALL_SETTING_KEYS = PUBLIC_KEYS | SECRET_KEYS
PROVIDER_LABELS = {
    "anthropic": "Anthropic", "openai": "OpenAI-compatible", "deepl": "DeepL",
    "google": "Google Cloud Translation", "echo": "Echo (offline test)",
}
PUBLIC_PROVIDERS = ("anthropic", "openai", "deepl", "google")
if DEFAULTS["captcha_provider"] not in {"none", *CAPTCHA_PROVIDERS}:
    raise RuntimeError("CAPTCHA_PROVIDER must be none, turnstile, recaptcha, or hcaptcha")
for _boolean_setting in BOOLEAN_SETTINGS:
    if DEFAULTS[_boolean_setting] not in {"0", "1"}:
        raise RuntimeError(f"{_boolean_setting.upper()} must be 0 or 1")


DATA_DIR.mkdir(parents=True, exist_ok=True)
JOBS_DIR.mkdir(parents=True, exist_ok=True)
engine = create_database_engine(DB_PATH)
# Under Gunicorn the master already recovered interrupted jobs before forking
# (gunicorn.conf.py); a worker must never fail jobs its siblings are running.
initialize_database(engine, DEFAULTS, now(), recover_jobs=not startup_recovery_completed())

configured_jwt_secret = os.environ.get("JWT_SECRET_KEY", "").strip()
jwt_secret = configured_jwt_secret or secrets.token_urlsafe(64)
if not configured_jwt_secret:
    LOGGER.warning(
        "JWT_SECRET_KEY is not configured; generated tokens will become invalid after restart "
        "and secrets cannot be saved"
    )


def encryption_key() -> bytes:
    explicit = os.environ.get("API_KEY_ENCRYPTION_KEY", "").strip()
    if explicit:
        try:
            key = explicit.encode("ascii")
            Fernet(key)
            return key
        except (UnicodeEncodeError, ValueError) as exc:
            raise RuntimeError("API_KEY_ENCRYPTION_KEY must be a valid Fernet key") from exc
    digest = hashlib.sha256(("subtitle-api-keys\0" + jwt_secret).encode()).digest()
    return base64.urlsafe_b64encode(digest)


secret_cipher = Fernet(encryption_key())
read_cache = RedisCache.from_environment(
    secret_cipher, engine.url.render_as_string(hide_password=False),
)
password_hasher = PasswordHasher()
DUMMY_PASSWORD_HASH = password_hasher.hash(secrets.token_urlsafe(32))

app = Flask(__name__, template_folder="templates", static_folder="static")
app.config.update(
    DEBUG=environment_flag("FLASK_DEBUG"),
    MAX_CONTENT_LENGTH=MAX_UPLOAD_MB * 1024 * 1024,
    SEND_FILE_MAX_AGE_DEFAULT=0,
    JWT_SECRET_KEY=jwt_secret,
    JWT_TOKEN_LOCATION=["cookies", "headers"],
    JWT_ACCESS_TOKEN_EXPIRES=timedelta(
        minutes=max(5, int(os.environ.get("JWT_ACCESS_MINUTES", "30")))
    ),
    JWT_COOKIE_SECURE=environment_flag("JWT_COOKIE_SECURE"),
    JWT_COOKIE_SAMESITE="Strict",
    JWT_COOKIE_CSRF_PROTECT=True,
    JWT_SESSION_COOKIE=False,
)
jwt = JWTManager(app)


def configure_proxy_trust(flask_app: Flask, proxy_count: int) -> None:
    """Trust exactly ``proxy_count`` reverse proxies for client address, scheme, and host.

    Without this, every visitor behind a proxy shares the proxy's address, which
    makes the per-address login and registration limits global.
    """
    if proxy_count < 0 or proxy_count > 10:
        raise RuntimeError("TRUSTED_PROXY_COUNT must be between 0 and 10")
    if proxy_count:
        flask_app.wsgi_app = ProxyFix(  # type: ignore[method-assign]
            flask_app.wsgi_app, x_for=proxy_count, x_proto=proxy_count,
            x_host=proxy_count,
        )


try:
    configure_proxy_trust(app, int(os.environ.get("TRUSTED_PROXY_COUNT", "0") or "0"))
except ValueError:
    raise RuntimeError("TRUSTED_PROXY_COUNT must be a whole number") from None
executor = ThreadPoolExecutor(max_workers=max(1, int(os.environ.get("JOB_WORKERS", "2"))))
db_lock = threading.RLock()
cancel_events_lock = threading.Lock()
cancel_events: dict[str, threading.Event] = {}


class AttemptLimiter:
    """Process-local sliding-window limit per client address.

    ``reserve`` checks the limit and records the attempt in one locked step, so
    parallel requests cannot all pass the check before any of them is recorded.
    Expired addresses are pruned periodically so the map cannot grow without bound.
    """

    def __init__(self, limit: int, window: timedelta) -> None:
        self.limit = limit
        self.window = window
        self.lock = threading.Lock()
        self.attempts: dict[str, list[datetime]] = {}
        self.next_prune: datetime | None = None

    def _recent(self, key: str, current: datetime) -> list[datetime]:
        cutoff = current - self.window
        if self.next_prune is None or current >= self.next_prune:
            self.next_prune = current + timedelta(seconds=ATTEMPT_PRUNE_SECONDS)
            for address in list(self.attempts):
                kept = [value for value in self.attempts[address] if value > cutoff]
                if kept:
                    self.attempts[address] = kept
                else:
                    del self.attempts[address]
        recent = [value for value in self.attempts.get(key, []) if value > cutoff]
        if recent:
            self.attempts[key] = recent
        else:
            self.attempts.pop(key, None)
        return recent

    def limited(self, key: str) -> bool:
        with self.lock:
            return len(self._recent(key, now_datetime())) >= self.limit

    def reserve(self, key: str) -> datetime | None:
        """Record an attempt and return its marker, or ``None`` when over the limit."""
        current = now_datetime()
        with self.lock:
            recent = self._recent(key, current)
            if len(recent) >= self.limit:
                return None
            recent.append(current)
            self.attempts[key] = recent
            return current

    def release(self, key: str, marker: datetime) -> None:
        """Forget a reserved attempt that turned out not to count (a successful login)."""
        with self.lock:
            recent = self.attempts.get(key)
            if recent and marker in recent:
                recent.remove(marker)
                if not recent:
                    del self.attempts[key]


login_limiter = AttemptLimiter(LOGIN_RATE_LIMIT, timedelta(minutes=LOGIN_LOCK_MINUTES))
registration_limiter = AttemptLimiter(
    REGISTER_RATE_LIMIT, timedelta(minutes=LOGIN_LOCK_MINUTES),
)
login_attempts = login_limiter.attempts
registration_attempts = registration_limiter.attempts


def available_providers() -> tuple[str, ...]:
    return PUBLIC_PROVIDERS + (("echo",) if app.debug else ())


def normalize_username(value: Any) -> str:
    return str(value or "").strip().lower()


def json_payload() -> dict[str, Any]:
    payload = request.get_json(silent=True)
    return payload if isinstance(payload, dict) else {}


def validate_username(username: str) -> str | None:
    if not USERNAME_PATTERN.fullmatch(username):
        return tr("Username must be 3-64 lowercase letters, numbers, dots, dashes, or underscores")
    return None


def validate_password(password: Any) -> str | None:
    if not isinstance(password, str) or len(password) < PASSWORD_MIN_LENGTH:
        return tr("Password must contain at least {minimum} characters", minimum=PASSWORD_MIN_LENGTH)
    if len(password) > 256:
        return tr("Password is too long")
    return None


def public_user(row: Any) -> dict[str, Any]:
    values = row._mapping if hasattr(row, "_mapping") else row
    locked_until = parse_timestamp(values.get("locked_until"))
    result = {
        "id": values["id"], "username": values["username"], "role": values["role"],
        "theme": values.get("theme", "system"),
        "active": bool(values["active"]),
        "locked": bool(locked_until and locked_until > now_datetime()),
        "created_at": values["created_at"],
    }
    if "job_count" in values:
        result["job_count"] = int(values["job_count"])
    return result


def issue_token(user_row: Any, session_id: str | None = None) -> str:
    """Issue an access JWT; a new sign-in starts a new session, refreshes keep theirs.

    The ``sid`` claim lets logout revoke every token of one session, including
    earlier refreshed tokens, without signing the account out on other devices.
    """
    values = user_row._mapping if hasattr(user_row, "_mapping") else user_row
    return create_access_token(
        identity=values["id"],
        additional_claims={
            "role": values["role"], "ver": values["token_version"],
            "sid": session_id or uuid.uuid4().hex,
        },
        fresh=True,
    )


def revoked_session_key(session_id: str) -> str:
    return REVOKED_SESSION_PREFIX + session_id


def token_session_id(claims: dict) -> str | None:
    """Return the token's session id; tokens issued before ``sid`` use their own jti."""
    for name in ("sid", "jti"):
        value = claims.get(name)
        if isinstance(value, str) and value:
            return value
    return None


def token_revocation_keys(claims: dict) -> list[str]:
    keys = [str(claims.get("jti", ""))]
    session_id = token_session_id(claims)
    if session_id:
        keys.append(revoked_session_key(session_id))
    return keys


def insert_user(db, user_id: str, username: str, password: str, role: str, timestamp: str) -> None:
    db.execute(insert(users).values(
        id=user_id, username=username, password_hash=password_hasher.hash(password),
        role=role, active=True, token_version=0, failed_login_count=0,
        locked_until=None, created_at=timestamp, updated_at=timestamp,
    ))


def record_account_login_failure(user_id: str) -> None:
    """Count a failed password attempt atomically and lock the account at the limit.

    The increment is a single SQL UPDATE, which takes the row write lock, so
    parallel guesses cannot read the same old count and under-count failures.
    """
    with transaction(engine) as db:
        db.execute(update(users).where(users.c.id == user_id).values(
            failed_login_count=users.c.failed_login_count + 1, updated_at=now(),
        ))
        failures = db.scalar(select(users.c.failed_login_count).where(
            users.c.id == user_id
        ))
        if failures is not None and failures >= LOGIN_FAILURE_LIMIT:
            db.execute(update(users).where(users.c.id == user_id).values(
                locked_until=(now_datetime() + timedelta(
                    minutes=LOGIN_LOCK_MINUTES
                )).isoformat(timespec="seconds"),
                failed_login_count=0,
            ))


def observed_lock_condition(observed_locked_until: str | None):
    """Match the lock state read before verification, or a lock already cleared.

    A lock written by a concurrent failure has a different value, so a guarded
    UPDATE using this condition can neither ignore nor clear that newer lock.
    """
    if observed_locked_until is None:
        return users.c.locked_until.is_(None)
    return or_(
        users.c.locked_until.is_(None), users.c.locked_until == observed_locked_until,
    )


def reserve_account_login_attempt(user: Any) -> bool:
    """Reserve one password attempt before verifying it (a single conditional UPDATE).

    The reservation counts as a failure until a successful login resets it, so at
    most ``LOGIN_FAILURE_LIMIT`` guesses can be evaluated per lock period no matter
    how many requests run in parallel.
    """
    with transaction(engine) as db:
        reserved = db.execute(update(users).where(
            users.c.id == user.id, users.c.active.is_(True),
            users.c.failed_login_count < LOGIN_FAILURE_LIMIT,
            observed_lock_condition(user.locked_until),
        ).values(failed_login_count=users.c.failed_login_count + 1, updated_at=now()))
        return reserved.rowcount == 1


def lock_exhausted_account(user_id: str) -> None:
    """Lock an account whose reserved attempts reached the limit (one atomic UPDATE)."""
    with transaction(engine) as db:
        db.execute(update(users).where(
            users.c.id == user_id, users.c.failed_login_count >= LOGIN_FAILURE_LIMIT,
        ).values(
            locked_until=(now_datetime() + timedelta(
                minutes=LOGIN_LOCK_MINUTES
            )).isoformat(timespec="seconds"),
            failed_login_count=0, updated_at=now(),
        ))


def bootstrap_admin() -> None:
    password = os.environ.get("ADMIN_PASSWORD", "") or os.environ.get("APP_PASSWORD", "")
    if not password:
        return
    username = normalize_username(os.environ.get("ADMIN_USERNAME", "admin"))
    with connection(engine) as db:
        if db.scalar(select(func.count()).select_from(users)):
            # Already bootstrapped: never reject a previously accepted account name.
            return
    error = validate_username(username) or validate_password(password)
    if error:
        raise RuntimeError(error)
    timestamp = now()
    try:
        with transaction(engine) as db:
            if db.scalar(select(func.count()).select_from(users)):
                return
            user_id = uuid.uuid4().hex
            db.execute(insert(settings_table).values(
                name="_auth_setup_complete", value="1", updated_at=timestamp
            ))
            insert_user(db, user_id, username, password, "admin", timestamp)
            db.execute(update(jobs).where(jobs.c.user_id.is_(None)).values(user_id=user_id))
            bump_cache_revision(db, "jobs")
    except IntegrityError:
        with connection(engine) as db:
            if not db.scalar(select(func.count()).select_from(users)):
                raise
    if os.environ.get("APP_PASSWORD") and not os.environ.get("ADMIN_PASSWORD"):
        LOGGER.warning("APP_PASSWORD is deprecated; it was used to bootstrap the admin account")


retry_database_race(bootstrap_admin)


def connect_db() -> sqlite3.Connection:
    """Compatibility helper for SQLite diagnostics and the existing test fixtures."""
    if engine.dialect.name != "sqlite":
        raise RuntimeError("connect_db is available only with the SQLite backend")
    raw_connection = sqlite3.connect(DB_PATH, timeout=30)
    raw_connection.row_factory = sqlite3.Row
    raw_connection.execute("PRAGMA foreign_keys=ON")
    return raw_connection


def encrypt_secret(value: str) -> str:
    return "enc:v1:" + secret_cipher.encrypt(value.encode()).decode("ascii")


def decrypt_secret(value: str) -> str:
    if not value.startswith("enc:v1:"):
        return value
    try:
        return secret_cipher.decrypt(value[7:].encode("ascii")).decode()
    except (InvalidToken, UnicodeDecodeError) as exc:
        raise RuntimeError(
            "A saved secret cannot be decrypted; restore the configured JWT/encryption key"
        ) from exc


def encrypt_existing_api_keys() -> None:
    """Upgrade values written by versions that stored secrets as plaintext."""
    if not (configured_jwt_secret or os.environ.get("API_KEY_ENCRYPTION_KEY")):
        return
    with transaction(engine) as db:
        rows = db.execute(select(
            settings_table.c.name, settings_table.c.value
        ).where(settings_table.c.name.in_(SECRET_KEYS))).all()
        for row in rows:
            if row.value and row.value.startswith("enc:v1:"):
                decrypt_secret(row.value)
            elif row.value:
                db.execute(update(settings_table).where(
                    settings_table.c.name == row.name
                ).values(value=encrypt_secret(row.value), updated_at=now()))
        bump_cache_revision(db, "settings")


retry_database_race(encrypt_existing_api_keys)

mfa = MFA(
    app, engine, password_hasher=password_hasher, encrypt=encrypt_secret,
    decrypt=decrypt_secret, issue_token=issue_token, public_user=public_user,
    clock=now_datetime, signing_secret=jwt_secret,
    stable_keys=bool(configured_jwt_secret or os.environ.get("API_KEY_ENCRYPTION_KEY")),
)


@jwt.token_in_blocklist_loader
def token_is_revoked(_header: dict, payload: dict) -> bool:
    with connection(engine) as db:
        if db.scalar(select(revoked_tokens.c.jti).where(
            revoked_tokens.c.jti.in_(token_revocation_keys(payload))
        ).limit(1)):
            return True
        user = db.execute(select(users.c.active, users.c.token_version).where(
            users.c.id == payload.get("sub")
        )).first()
    return user is None or not user.active or int(payload.get("ver", -1)) != user.token_version


@jwt.unauthorized_loader
def missing_token(reason: str):
    return jsonify(error=tr("Authentication required"), detail=reason), 401


@jwt.invalid_token_loader
def invalid_token(reason: str):
    return jsonify(error=tr("Invalid authentication token"), detail=reason), 401


@jwt.expired_token_loader
def expired_token(_header: dict, _payload: dict):
    return jsonify(error=tr("Authentication token expired")), 401


@jwt.revoked_token_loader
def revoked_token(_header: dict, _payload: dict):
    return jsonify(error=tr("Authentication token revoked")), 401


def current_user_row() -> Any | None:
    with connection(engine) as db:
        return db.execute(select(users).where(users.c.id == get_jwt_identity())).first()


def admin_required(function: Callable):
    @wraps(function)
    @jwt_required()
    def wrapped(*args, **kwargs):
        user = current_user_row()
        if user is None or user.role != "admin":
            return jsonify(error=tr("Administrator access required")), 403
        return function(*args, **kwargs)
    return wrapped


@app.after_request
def security_headers(response):
    response.headers.setdefault("X-Content-Type-Options", "nosniff")
    response.headers.setdefault("X-Frame-Options", "DENY")
    response.headers.setdefault("Referrer-Policy", "no-referrer")
    response.headers.setdefault(
        "Content-Security-Policy",
        "default-src 'self'; script-src 'self' https://challenges.cloudflare.com "
        "https://www.google.com https://www.gstatic.com https://js.hcaptcha.com; "
        "style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; "
        "connect-src 'self' https://challenges.cloudflare.com https://www.google.com "
        "https://www.gstatic.com https://*.hcaptcha.com; frame-src "
        "https://challenges.cloudflare.com https://www.google.com https://recaptcha.google.com "
        "https://*.hcaptcha.com; object-src 'none'; base-uri 'self'; frame-ancestors 'none'",
    )
    response.headers.setdefault("Cache-Control", "no-store")
    response.headers.setdefault("Content-Language", current_locale())
    response.vary.add("Accept-Language")
    response.vary.add("Cookie")
    endpoint = request.endpoint or ""
    if (endpoint in NO_TOKEN_REFRESH_ENDPOINTS or endpoint.startswith("mfa_")
            or response_sets_access_cookie(response)):
        return response
    try:
        verify_jwt_in_request(optional=True)
        claims = get_jwt()
        if (get_jwt_request_location() == "cookies" and get_jwt_identity()
                and claims.get("exp") and datetime.fromtimestamp(
            claims["exp"], timezone.utc
        ) < now_datetime() + timedelta(minutes=10)):
            token = refreshed_session_token(claims)
            if token:
                set_access_cookies(response, token)
    except Exception:
        pass
    return response


def response_sets_access_cookie(response) -> bool:
    prefix = app.config.get("JWT_ACCESS_COOKIE_NAME", "access_token_cookie") + "="
    return any(cookie.startswith(prefix) for cookie in response.headers.getlist("Set-Cookie"))


def refreshed_session_token(claims: dict) -> str | None:
    """Re-issue a token only if the presented one is still exactly current.

    The request was verified earlier, but a password reset, role change,
    deactivation, or logout can commit while it runs. Re-check everything from one
    fresh read and keep the presented token version and session id: if a change
    commits after this read, the new token carries the old version or a revoked
    session and is rejected on its next use.
    """
    with connection(engine) as db:
        user = db.execute(select(users).where(users.c.id == claims.get("sub"))).first()
        if user is None or not user.active:
            return None
        try:
            if int(claims.get("ver", -1)) != user.token_version:
                return None
        except (TypeError, ValueError):
            return None
        if db.scalar(select(revoked_tokens.c.jti).where(
            revoked_tokens.c.jti.in_(token_revocation_keys(claims))
        ).limit(1)):
            return None
    return issue_token(user, token_session_id(claims))


def cached_rows(name: str, scope: str, statement) -> list[dict[str, Any]]:
    """Read a revision in SQL before using Redis; cache raw, locale-neutral rows."""
    with connection(engine) as db:
        def load():
            return [dict(row) for row in db.execute(statement).mappings()]

        if not read_cache.available:
            return load()
        revision = db.scalar(select(cache_revisions.c.revision).where(
            cache_revisions.c.name == name,
        ))
        if revision is None:
            return load()
        return read_cache.remember(f"{name}:{revision}:{scope}", load)


def read_settings(include_secrets: bool = False) -> dict[str, Any]:
    values = {row["name"]: row["value"] for row in cached_rows(
        "settings", "all", select(settings_table.c.name, settings_table.c.value),
    )}
    result: dict[str, Any] = {key: values.get(key, default) for key, default in DEFAULTS.items()}
    providers = available_providers()
    if result["default_provider"] not in providers:
        result["default_provider"] = DEFAULTS["default_provider"]
    configured = {
        "anthropic": bool(values.get("anthropic_api_key") or os.environ.get("ANTHROPIC_API_KEY")),
        "openai": bool(values.get("openai_api_key") or os.environ.get("OPENAI_API_KEY")),
        "deepl": bool(values.get("deepl_api_key") or os.environ.get("DEEPL_API_KEY")),
        "google": bool(values.get("google_api_key") or os.environ.get("GOOGLE_API_KEY")),
        "echo": True,
    }
    result["providers"] = {name: tr(PROVIDER_LABELS[name]) for name in providers}
    result["configured"] = {name: configured[name] for name in providers}
    result["captcha_configured"] = {
        provider: bool(
            result.get(f"{provider}_site_key")
            and (values.get(f"{provider}_secret_key")
                 or os.environ.get(f"{provider}_secret_key".upper()))
        )
        for provider in CAPTCHA_PROVIDERS
    }
    if include_secrets:
        for key in SECRET_KEYS:
            stored = values.get(key, "")
            result[key] = decrypt_secret(stored) if stored else os.environ.get(key.upper(), "")
    return result


def public_auth_configuration() -> dict[str, Any]:
    settings = read_settings()
    provider = settings["captcha_provider"]
    protected_actions = [
        action for action, key in CAPTCHA_ACTION_SETTINGS.items()
        if settings[key] == "1"
    ]
    return {
        "registration_enabled": settings["registration_enabled"] == "1",
        "captcha": {
            "provider": provider,
            "site_key": settings.get(f"{provider}_site_key", "")
            if provider in CAPTCHA_PROVIDERS else "",
            "protected_actions": protected_actions if provider != "none" else [],
        },
    }


def captcha_required(action: str, settings: dict[str, Any]) -> bool:
    provider = settings.get("captcha_provider", "none")
    setting = CAPTCHA_ACTION_SETTINGS.get(action)
    return provider in CAPTCHA_PROVIDERS and bool(setting) and settings.get(setting) == "1"


def verify_captcha(action: str, token: Any):
    """Return a Flask error response when a required CAPTCHA is not valid."""
    settings = read_settings(include_secrets=True)
    if not captcha_required(action, settings):
        return None
    provider = settings["captcha_provider"]
    failure = captcha.verify_token(
        provider=provider, action=action, token=token,
        site_key=settings.get(f"{provider}_site_key", ""),
        secret_key=settings.get(f"{provider}_secret_key", ""),
        configured_hostname=settings.get("captcha_hostname", ""),
        remote_addr=request.remote_addr,
    )
    if failure is None:
        return None
    message, status = failure
    return jsonify(error=message), status


def consume_job_quota(db, user: Any, amount: int) -> dict[str, Any] | None:
    """Atomically consume job quota or describe the exceeded limit (see ``quotas``)."""
    return quotas.consume_job_quota(db, user, amount, defaults=DEFAULTS, clock=now_datetime)


def localized_job_stage(stage: str) -> str:
    if stage in {"Reading subtitle", "Ready to download", "Canceled", "Canceling",
                 "Translation failed"}:
        return tr(stage)
    parsed = re.fullmatch(r"Parsed (\d+) cues", stage)
    if parsed:
        return tr("Parsed {count} cues", count=parsed.group(1))
    translating = re.fullmatch(
        r"Translating to (.+?)(?: \((\d+)/(\d+) segments\))?", stage,
    )
    if translating:
        source_name, done, total = translating.groups()
        language = tr(source_name)
        if done is not None:
            return tr(
                "Translating to {language} ({done}/{total} segments)",
                language=language, done=done, total=total,
            )
        return tr("Translating to {language}", language=language)
    return stage


def localized_job_warning(warning: str | None) -> str | None:
    if not warning:
        return None
    parsed = re.fullmatch(r"Left untranslated: (\d+) of (\d+) segments", warning)
    if parsed:
        return tr("Left untranslated: {count} of {total} segments",
                  count=parsed.group(1), total=parsed.group(2))
    return warning


def job_dict(row: Any, include_owner: bool = False) -> dict[str, Any]:
    source = dict(row._mapping if hasattr(row, "_mapping") else row)
    result = {key: source.get(key) for key in jobs.c.keys()}
    result["options"] = json.loads(result["options"])
    result["outputs"] = json.loads(result["outputs"])
    result["stage"] = localized_job_stage(result.get("stage") or "")
    result["warning"] = localized_job_warning(result.get("warning"))
    result.pop("stored_name", None)
    result.pop("user_id", None)
    if include_owner:
        result["owner"] = source.get("owner")
    return result


def update_job(job_id: str, *, if_status: set[str] | None = None, **fields: Any) -> None:
    """Update a job; with ``if_status`` only while it is still in one of those states."""
    fields["updated_at"] = now()
    with db_lock, transaction(engine) as db:
        statement = update(jobs).where(jobs.c.id == job_id)
        if if_status is not None:
            statement = statement.where(jobs.c.status.in_(if_status))
        if db.execute(statement.values(**fields)).rowcount:
            bump_cache_revision(db, "jobs")


def update_job_if_status(job_id: str, statuses: set[str], **fields: Any) -> bool:
    fields["updated_at"] = now()
    with db_lock, transaction(engine) as db:
        result = db.execute(update(jobs).where(
            jobs.c.id == job_id, jobs.c.status.in_(statuses)
        ).values(**fields))
        if result.rowcount:
            bump_cache_revision(db, "jobs")
        return result.rowcount == 1


def cancel_event_for(job_id: str) -> threading.Event:
    with cancel_events_lock:
        return cancel_events.setdefault(job_id, threading.Event())


def signal_local_cancel(job_id: str) -> None:
    """Wake a worker thread in this process; other processes poll the database.

    Only events registered by a running ``run_job`` are signaled, so requests
    for jobs owned by another worker process never leak an entry here.
    """
    with cancel_events_lock:
        event = cancel_events.get(job_id)
    if event is not None:
        event.set()


# How often a running job re-reads its status so a cancellation recorded by
# another web worker process is honored without one query per batch callback.
CANCEL_POLL_SECONDS = 1.0


def job_cancel_monitor(job_id: str, cancel_event: threading.Event) -> Callable[[], bool]:
    """Return a thread-safe "stop now?" check for one running job.

    The in-memory event covers cancellation from this process immediately.
    Cancellation from another process, or any transition that removed the job
    from ``queued``/``processing``, is detected by a throttled status query.
    """
    poll_lock = threading.Lock()
    next_poll = [0.0]

    def stop_requested() -> bool:
        if cancel_event.is_set():
            return True
        moment = time.monotonic()
        with poll_lock:
            if moment < next_poll[0]:
                return False
            next_poll[0] = moment + CANCEL_POLL_SECONDS
        try:
            with connection(engine) as db:
                status = db.scalar(select(jobs.c.status).where(jobs.c.id == job_id))
        except SQLAlchemyError:
            LOGGER.warning("Could not poll the cancellation state of job %s", job_id,
                           exc_info=True)
            return False
        if status not in {"queued", "processing"}:
            cancel_event.set()
            return True
        return False

    return stop_requested


def provider_for(name: str, provider_settings: dict[str, Any], throttle: Throttle,
                 model: str | None):
    if name == "echo":
        if not app.debug:
            raise FatalTranslationError("Echo provider is available only in debug mode")
        return make_echo()
    if name == "anthropic":
        key = provider_settings.get("anthropic_api_key")
        if not key:
            raise FatalTranslationError("Anthropic API key is not configured")
        return make_anthropic(model or provider_settings["anthropic_model"], key, throttle)
    if name == "openai":
        key = provider_settings.get("openai_api_key")
        if not key:
            raise FatalTranslationError("OpenAI-compatible API key is not configured")
        return make_openai(model or provider_settings["openai_model"], key, throttle,
                           provider_settings["openai_base_url"])
    if name == "deepl":
        key = provider_settings.get("deepl_api_key")
        if not key:
            raise FatalTranslationError("DeepL API key is not configured")
        return make_deepl(key, throttle)
    if name == "google":
        key = provider_settings.get("google_api_key")
        if not key:
            raise FatalTranslationError("Google Cloud Translation API key is not configured")
        return make_google(key, throttle)
    raise FatalTranslationError(f"Unknown provider: {name}")


def run_job(job_id: str) -> None:
    cancel_event = cancel_event_for(job_id)
    stop_requested = job_cancel_monitor(job_id, cancel_event)

    def check_canceled() -> None:
        if stop_requested():
            raise TranslationCanceled("Translation canceled")

    def report(**fields: Any) -> None:
        # Never overwrite the "Canceling" stage, or a state written by another
        # process, with progress from work that is being abandoned.
        update_job(job_id, if_status={"processing"}, **fields)

    try:
        with connection(engine) as db:
            row = db.execute(select(jobs).where(jobs.c.id == job_id)).first()
        if row is None:
            return
        if row.status == "canceling":
            cancel_event.set()
        if cancel_event.is_set():
            raise TranslationCanceled("Translation canceled")
        options = json.loads(row.options)
        folder = JOBS_DIR / job_id
        source = folder / row.stored_name
        if not update_job_if_status(
            job_id, {"queued"}, status="processing", progress=2, stage="Reading subtitle",
        ):
            # Already claimed, canceled while queued, or finalized elsewhere.
            return
        document = load_subtitle(source, options.get("encoding", "utf-8"))
        segments = []
        for cue_i, cue in enumerate(document.cues):
            segments.extend(segment_cue(cue, cue_i))
        check_canceled()
        report(progress=5, stage=f"Parsed {len(document.cues)} cues")

        provider_settings = read_settings(include_secrets=True)
        throttle = Throttle(float(options["rpm"]), stop_requested)
        provider = provider_for(
            options["provider"], provider_settings, throttle, options.get("model")
        )
        targets = options["target_languages"]
        cache_path = folder / "translation-cache.json"
        try:
            cache = json.loads(cache_path.read_text("utf-8")) if cache_path.exists() else {}
        except (OSError, json.JSONDecodeError):
            cache = {}
        outputs = []
        untranslated = 0
        for index, language in enumerate(targets):
            check_canceled()
            start_pct = 5 + round(index / len(targets) * 90)
            report(progress=start_pct, stage=f"Translating to {LANGS[language]['name']}")

            def report_progress(done: int, total: int, *, target_index: int = index,
                                target_language: str = language) -> None:
                fraction = done / total if total else 1
                progress = 5 + round((target_index + fraction) / len(targets) * 90)
                report(
                    progress=progress,
                    stage=(f"Translating to {LANGS[target_language]['name']} "
                           f"({done}/{total} segments)"),
                )

            def count_untranslated(count: int) -> None:
                nonlocal untranslated
                untranslated += count

            translated = translate_segments(
                segments, provider, language, options["source_language"],
                int(options["batch_size"]), 4, 10, throttle, cache,
                int(options["workers"]), True, report_progress, stop_requested,
                count_untranslated,
            )
            check_canceled()
            cues = rebuild_cues(
                document.cues, segments, translated, language,
                float(options["width"]), int(options["max_lines"]),
            )
            output_name = translated_filename(row.filename, LANGS[language]["suffix"])
            (folder / output_name).write_bytes(document.clone_with_cues(cues).to_bytes())
            outputs.append({"name": output_name, "language": language})
            cache_path.write_text(json.dumps(cache, ensure_ascii=False), "utf-8")
            report(outputs=json.dumps(outputs),
                   progress=5 + round((index + 1) / len(targets) * 90))
        if not update_job_if_status(
            job_id, {"processing"}, status="completed", progress=100,
            stage="Ready to download", outputs=json.dumps(outputs), error=None,
            warning=(
                f"Left untranslated: {untranslated} of "
                f"{len(segments) * len(targets)} segments"
            ) if untranslated else None,
        ):
            raise TranslationCanceled("Translation canceled")
    except TranslationCanceled:
        update_job_if_status(
            job_id, {"queued", "processing", "canceling"}, status="canceled",
            stage="Canceled", error=None,
        )
    except Exception as exc:
        if cancel_event.is_set() or not update_job_if_status(
            job_id, {"queued", "processing"}, status="failed",
            stage="Translation failed", error=f"{type(exc).__name__}: {exc}",
        ):
            # A cancellation requested here or in another worker process wins
            # over the failure; never leave the job stuck in "canceling".
            update_job_if_status(
                job_id, {"queued", "processing", "canceling"}, status="canceled",
                stage="Canceled", error=None,
            )
    finally:
        with cancel_events_lock:
            if cancel_events.get(job_id) is cancel_event:
                cancel_events.pop(job_id, None)


def owned_job(job_id: str, user: Any) -> Any | None:
    statement = select(jobs).where(jobs.c.id == job_id)
    if user.role != "admin":
        statement = statement.where(jobs.c.user_id == user.id)
    with connection(engine) as db:
        return db.execute(statement).first()


@app.get("/")
def index():
    locale = current_locale()
    theme = "system"
    try:
        verify_jwt_in_request(optional=True)
        if get_jwt_identity():
            user = current_user_row()
            if user is not None and user.active and user.theme in ACCOUNT_THEMES:
                theme = user.theme
    except Exception:
        # Invalid or expired browser credentials should still receive the public shell.
        pass
    languages = {
        code: {**language, "name": tr(language["name"])}
        for code, language in LANGS.items()
    }
    response = make_response(render_template(
        "index.html", languages=languages, max_upload_mb=MAX_UPLOAD_MB,
        locale=locale, locales=LOCALE_LABELS, theme=theme, tr=tr,
    ))
    selected = normalize_locale(request.args.get("lang"))
    if selected:
        response.set_cookie(
            LOCALE_COOKIE, selected, max_age=365 * 24 * 60 * 60,
            secure=app.config["JWT_COOKIE_SECURE"], httponly=False, samesite="Strict",
        )
    return response


@app.get("/api/i18n")
def i18n_catalog():
    return jsonify(
        locale=current_locale(), messages=messages_for(),
        languages={code: tr(language["name"]) for code, language in LANGS.items()},
    )


@app.get("/healthz")
def health():
    return jsonify(status="ok")


@app.get("/api/auth/setup-status")
def setup_status():
    with connection(engine) as db:
        configured = bool(db.scalar(select(func.count()).select_from(users)))
    return jsonify(configured=configured, **public_auth_configuration())


@app.post("/api/auth/setup")
def setup_first_admin():
    if request.content_length and request.content_length > 8192:
        return jsonify(error=tr("Authentication request is too large")), 413
    payload = json_payload()
    username = normalize_username(payload.get("username"))
    password = payload.get("password")
    error = validate_username(username) or validate_password(password)
    if error:
        return jsonify(error=error), 400
    timestamp = now()
    user_id = uuid.uuid4().hex
    try:
        with transaction(engine) as db:
            if db.scalar(select(func.count()).select_from(users)):
                return jsonify(error=tr("Initial setup is already complete")), 409
            db.execute(insert(settings_table).values(
                name="_auth_setup_complete", value="1", updated_at=timestamp
            ))
            insert_user(db, user_id, username, password, "admin", timestamp)
            db.execute(update(jobs).where(jobs.c.user_id.is_(None)).values(user_id=user_id))
            bump_cache_revision(db, "jobs")
    except IntegrityError:
        return jsonify(error=tr("Initial setup is already complete")), 409
    with connection(engine) as db:
        user = db.execute(select(users).where(users.c.id == user_id)).first()
    response = jsonify(user=public_user(user))
    set_access_cookies(response, issue_token(user))
    return response, 201


@app.post("/api/auth/login")
def login():
    if request.content_length and request.content_length > 8192:
        return jsonify(error=tr("Authentication request is too large")), 413
    remote_address = request.remote_addr or "unknown"
    too_many = (jsonify(error=tr("Too many login attempts; try again later")), 429)
    if login_limiter.limited(remote_address):
        return too_many
    payload = json_payload()
    captcha_error = verify_captcha("login", payload.get("captcha_token"))
    if captcha_error:
        return captcha_error
    # Reserve the per-address attempt before the slow password check; parallel
    # guesses cannot all pass a check that is recorded only after verification.
    address_attempt = login_limiter.reserve(remote_address)
    if address_attempt is None:
        return too_many
    raw_username = payload.get("username")
    password = payload.get("password")
    valid_input = (
        isinstance(raw_username, str) and len(raw_username) <= 64
        and isinstance(password, str) and len(password) <= 256
    )
    username = normalize_username(raw_username) if valid_input else ""
    candidate_password = password if valid_input else ""
    with connection(engine) as db:
        user = db.execute(select(users).where(users.c.username == username)).first()
    locked_until = parse_timestamp(user.locked_until) if user is not None else None
    locked = bool(locked_until and locked_until > now_datetime())
    reserved = bool(
        user is not None and valid_input and user.active and not locked
        and reserve_account_login_attempt(user)
    )
    # Without a reservation the outcome is already a failure; hash the dummy value
    # so the response time does not reveal whether the account is locked.
    candidate_hash = user.password_hash if reserved else DUMMY_PASSWORD_HASH
    try:
        verified = password_hasher.verify(candidate_hash, candidate_password) and reserved
    except (VerifyMismatchError, VerificationError, InvalidHashError):
        verified = False
    if not verified:
        if reserved:
            lock_exhausted_account(user.id)
        return jsonify(error=tr("Invalid username or password")), 401
    values = {"failed_login_count": 0, "locked_until": None, "updated_at": now()}
    if password_hasher.check_needs_rehash(user.password_hash):
        values["password_hash"] = password_hasher.hash(candidate_password)
    with transaction(engine) as db:
        # Conditional on the lock state observed before verification, so a lock set
        # by concurrent failures is never cleared by this success.
        changed = db.execute(update(users).where(
            users.c.id == user.id, users.c.active.is_(True),
            users.c.token_version == user.token_version,
            users.c.password_hash == user.password_hash,
            observed_lock_condition(user.locked_until),
        ).values(**values))
        if not changed.rowcount:
            return jsonify(error=tr("Invalid username or password")), 401
        refreshed_user = db.execute(select(users).where(users.c.id == user.id)).first()
    login_limiter.release(remote_address, address_attempt)
    return mfa.begin_login(refreshed_user, payload)


@app.post("/api/auth/register")
def register():
    if request.content_length and request.content_length > 8192:
        return jsonify(error=tr("Authentication request is too large")), 413
    remote_address = request.remote_addr or "unknown"
    if registration_limiter.reserve(remote_address) is None:
        return jsonify(error=tr("Too many registration attempts; try again later")), 429
    payload = json_payload()
    settings = read_settings()
    if settings["registration_enabled"] != "1":
        return jsonify(error=tr("New account registration is disabled")), 403
    with connection(engine) as db:
        if not db.scalar(select(func.count()).select_from(users)):
            return jsonify(error=tr("Create the first administrator before registering users")), 409
    captcha_error = verify_captcha("register", payload.get("captcha_token"))
    if captcha_error:
        return captcha_error
    username = normalize_username(payload.get("username"))
    password = payload.get("password")
    error = validate_username(username) or validate_password(password)
    if error:
        return jsonify(error=error), 400
    if payload.get("confirm_password") != password:
        return jsonify(error=tr("Passwords do not match")), 400
    user_id = uuid.uuid4().hex
    timestamp = now()
    try:
        with transaction(engine) as db:
            insert_user(db, user_id, username, password, "user", timestamp)
    except IntegrityError:
        return jsonify(error=tr("Username already exists")), 409
    with connection(engine) as db:
        user = db.execute(select(users).where(users.c.id == user_id)).first()
    response = jsonify(user=public_user(user))
    set_access_cookies(response, issue_token(user))
    return response, 201


def revoke_token_key(key: str, expires: datetime) -> None:
    """Record a revoked jti or session key; repeated logouts are harmless."""
    try:
        with transaction(engine) as db:
            if db.scalar(select(revoked_tokens.c.jti).where(revoked_tokens.c.jti == key)):
                return
            db.execute(insert(revoked_tokens).values(
                jti=key, expires_at=expires.isoformat(timespec="seconds"), created_at=now(),
            ))
    except IntegrityError:
        pass  # A concurrent logout recorded the same key first.


@app.post("/api/auth/logout")
@jwt_required()
def logout():
    claims = get_jwt()
    token_expires = datetime.fromtimestamp(claims["exp"], timezone.utc)
    revoke_token_key(claims["jti"], token_expires)
    session_id = token_session_id(claims)
    if session_id:
        # Revoke the whole session: earlier refreshed tokens, and any token an
        # in-flight request mints for it, which expires no later than this bound.
        revoke_token_key(revoked_session_key(session_id), max(
            token_expires, now_datetime() + app.config["JWT_ACCESS_TOKEN_EXPIRES"],
        ) + timedelta(minutes=1))
    response = jsonify(message=tr("Logged out"))
    unset_jwt_cookies(response)
    return response


@app.get("/api/auth/me")
@jwt_required()
def who_am_i():
    user = current_user_row()
    return jsonify(user=public_user(user)) if user else (jsonify(error=tr("User not found")), 401)


@app.patch("/api/auth/me")
@jwt_required()
def update_my_preferences():
    user = current_user_row()
    if user is None:
        return jsonify(error=tr("User not found")), 401
    payload = json_payload()
    unknown = set(payload) - {"theme"}
    if unknown:
        return jsonify(error=tr(
            "Unknown fields: {fields}", fields=', '.join(sorted(unknown))
        )), 400
    theme = payload.get("theme")
    if not isinstance(theme, str) or theme not in ACCOUNT_THEMES:
        return jsonify(error=tr("Theme must be system, light, or dark")), 400
    with transaction(engine) as db:
        db.execute(update(users).where(users.c.id == user.id).values(
            theme=theme, updated_at=now(),
        ))
    with connection(engine) as db:
        updated = db.execute(select(users).where(users.c.id == user.id)).first()
    return jsonify(user=public_user(updated))


@app.get("/api/users")
@admin_required
def list_users():
    job_count = select(func.count(jobs.c.id)).where(
        jobs.c.user_id == users.c.id
    ).correlate(users).scalar_subquery()
    with connection(engine) as db:
        rows = db.execute(select(users, job_count.label("job_count")).order_by(
            users.c.username
        )).all()
    return jsonify(users=[public_user(row) for row in rows])


@app.post("/api/users")
@admin_required
def create_user():
    payload = json_payload()
    username = normalize_username(payload.get("username"))
    password = payload.get("password")
    role = payload.get("role", "user")
    error = validate_username(username) or validate_password(password)
    if error:
        return jsonify(error=error), 400
    if not valid_role(role):
        return jsonify(error=tr("Invalid role")), 400
    user_id = uuid.uuid4().hex
    timestamp = now()
    try:
        with transaction(engine) as db:
            insert_user(db, user_id, username, password, role, timestamp)
    except IntegrityError:
        return jsonify(error=tr("Username already exists")), 409
    with connection(engine) as db:
        user = db.execute(select(users).where(users.c.id == user_id)).first()
    return jsonify(user=public_user(user)), 201


def valid_role(role: Any) -> bool:
    return isinstance(role, str) and role in {"user", "admin"}


def lock_admin_roster(db) -> None:
    """Serialize changes that can remove an active administrator.

    Writing the shared row first takes a row lock on PostgreSQL/MariaDB/MySQL and
    the database write lock on SQLite, so concurrent demotions, deactivations, and
    deletions re-count administrators one at a time (across all web workers).
    """
    db.execute(update(settings_table).where(
        settings_table.c.name == ADMIN_ROSTER_LOCK_SETTING
    ).values(updated_at=settings_table.c.updated_at))


def actor_still_admin(db, actor: Any) -> bool:
    """Re-check, inside the roster lock, that a concurrent change did not revoke the actor."""
    current = db.execute(select(users.c.role, users.c.active, users.c.token_version).where(
        users.c.id == actor.id
    )).first()
    return bool(
        current is not None and current.role == "admin" and current.active
        and current.token_version == actor.token_version
    )


def active_admin_count(db) -> int:
    """Count active administrators; call after ``lock_admin_roster`` in the same transaction."""
    return int(db.scalar(select(func.count()).select_from(users).where(
        users.c.role == "admin", users.c.active.is_(True)
    )) or 0)


@app.patch("/api/users/<user_id>")
@admin_required
def update_user(user_id: str):
    actor = current_user_row()
    payload = json_payload()
    unknown = set(payload) - {"role", "active", "password", "unlock"}
    if unknown:
        return jsonify(error=tr("Unknown fields: {fields}", fields=', '.join(sorted(unknown)))), 400
    values: dict[str, Any] = {"updated_at": now()}
    if "role" in payload:
        if not valid_role(payload["role"]):
            return jsonify(error=tr("Invalid role")), 400
        values["role"] = payload["role"]
    if "active" in payload:
        if not isinstance(payload["active"], bool):
            return jsonify(error=tr("active must be a boolean")), 400
        values["active"] = payload["active"]
    if "password" in payload:
        error = validate_password(payload["password"])
        if error:
            return jsonify(error=error), 400
        values["password_hash"] = password_hasher.hash(payload["password"])
        values["token_version"] = users.c.token_version + 1
    unlock = payload.get("unlock") is True
    if unlock:
        values["failed_login_count"] = 0
        values["locked_until"] = None
    if user_id == actor.id and (
        values.get("active") is False or values.get("role") == "user"
    ):
        return jsonify(error=tr("You cannot deactivate or demote your own account")), 409
    with transaction(engine) as db:
        lock_admin_roster(db)
        if not actor_still_admin(db, actor):
            return jsonify(error=tr("Administrator access required")), 403
        target = db.execute(select(users).where(users.c.id == user_id)).first()
        if target is None:
            return jsonify(error=tr("User not found")), 404
        removes_admin = target.role == "admin" and target.active and (
            values.get("active") is False or values.get("role") == "user"
        )
        if removes_admin and active_admin_count(db) <= 1:
            return jsonify(error=tr("At least one active administrator is required")), 409
        if "active" in values or "role" in values:
            values["token_version"] = users.c.token_version + 1
        db.execute(update(users).where(users.c.id == user_id).values(**values))
        if unlock:
            # MFA verification locks are separate from the password lock; an
            # administrator unlock clears all of them (MFA factors stay enrolled).
            mfa_locks = {
                column: 0 for column in (
                    "failures", "locked_until", "manage_failures", "manage_locked_until",
                ) if column in mfa_accounts.c
            }
            db.execute(update(mfa_accounts).where(
                mfa_accounts.c.user_id == user_id
            ).values(**mfa_locks))
    with connection(engine) as db:
        updated = db.execute(select(users).where(users.c.id == user_id)).first()
    return jsonify(user=public_user(updated))


@app.delete("/api/users/<user_id>")
@admin_required
def delete_user(user_id: str):
    actor = current_user_row()
    if user_id == actor.id:
        return jsonify(error=tr("You cannot delete your own account")), 409
    with transaction(engine) as db:
        lock_admin_roster(db)
        if not actor_still_admin(db, actor):
            return jsonify(error=tr("Administrator access required")), 403
        target = db.execute(select(users).where(users.c.id == user_id)).first()
        if target is None:
            return jsonify(error=tr("User not found")), 404
        if target.role == "admin" and target.active and active_admin_count(db) <= 1:
            return jsonify(error=tr("At least one active administrator is required")), 409
        if db.scalar(select(func.count()).select_from(jobs).where(
            jobs.c.user_id == user_id, jobs.c.status.in_(ACTIVE_STATUSES)
        )):
            return jsonify(error=tr("Cancel or finish this user's active jobs first")), 409
        db.execute(update(jobs).where(jobs.c.user_id == user_id).values(user_id=None))
        db.execute(delete(rate_limit_buckets).where(
            rate_limit_buckets.c.scope.in_([
                f"user:{user_id}",
                *(f"user:{user_id}:{period}" for period in CALENDAR_PERIODS),
            ])
        ))
        db.execute(delete(users).where(users.c.id == user_id))
        bump_cache_revision(db, "jobs")
    return jsonify(deleted=user_id)


@app.get("/api/settings")
@jwt_required()
def get_settings():
    return jsonify(read_settings())


@app.put("/api/settings")
@admin_required
def save_settings():
    payload = json_payload()
    unknown = set(payload) - ALL_SETTING_KEYS
    if unknown:
        return jsonify(error=tr("Unknown settings: {settings}", settings=', '.join(sorted(unknown)))), 400
    error = validate_choice_and_flag_settings(payload, available_providers())
    if error:
        return jsonify(error=error), 400
    if any(key in payload and str(payload[key]).strip() for key in SECRET_KEYS):
        if not configured_jwt_secret and not os.environ.get("API_KEY_ENCRYPTION_KEY"):
            return jsonify(error=tr("Configure JWT_SECRET_KEY before saving secrets")), 503
    combined = read_settings()
    combined.update({key: str(value).strip() for key, value in payload.items()})
    captcha_provider = combined["captcha_provider"]
    if captcha_provider in CAPTCHA_PROVIDERS and any(
        combined[key] == "1" for key in CAPTCHA_ACTION_SETTINGS.values()
    ):
        site_key = combined.get(f"{captcha_provider}_site_key", "")
        secret_name = f"{captcha_provider}_secret_key"
        secret_configured = bool(
            str(payload.get(secret_name, "")).strip()
            or combined["captcha_configured"].get(captcha_provider)
        )
        if not site_key or not secret_configured:
            return jsonify(error=tr(
                "Configure the selected CAPTCHA site key and secret key before enabling protection"
            )), 400
    # Evaluated on the merged state because payloads are partial. Stored configurations
    # that predate this rule keep working until an administrator saves settings again.
    if captcha_provider != "none" and not combined.get("captcha_hostname"):
        return jsonify(error=tr(
            "Enter the expected CAPTCHA hostname before selecting a CAPTCHA provider"
        )), 400
    error = validate_numeric_settings(payload)
    if error:
        return jsonify(error=error), 400
    timestamp = now()
    with db_lock, transaction(engine) as db:
        changed_window = False
        if RATE_LIMIT_KEYS & payload.keys():
            db.execute(update(settings_table).where(
                settings_table.c.name == "panel_job_limit"
            ).values(updated_at=settings_table.c.updated_at))
            previous = dict(db.execute(select(
                settings_table.c.name, settings_table.c.value,
            ).where(settings_table.c.name.in_(WINDOW_LIMIT_KEYS))).all())
            changed_window = any(
                str(payload[key]).strip() != previous.get(key, DEFAULTS[key])
                for key in WINDOW_LIMIT_KEYS & payload.keys()
            )
        for key, value in payload.items():
            clean_value = str(value).strip()
            if key in SECRET_KEYS and not clean_value:
                continue
            if key in SECRET_KEYS:
                clean_value = encrypt_secret(clean_value)
            existing = db.scalar(select(settings_table.c.name).where(settings_table.c.name == key))
            if existing:
                db.execute(update(settings_table).where(
                    settings_table.c.name == key
                ).values(value=clean_value, updated_at=timestamp))
            else:
                db.execute(insert(settings_table).values(
                    name=key, value=clean_value, updated_at=timestamp
                ))
        if changed_window:
            # Calendar usage survives all settings edits, including disabling limits.
            db.execute(delete(rate_limit_buckets).where(
                ~rate_limit_buckets.c.scope.like("%:daily"),
                ~rate_limit_buckets.c.scope.like("%:weekly"),
                ~rate_limit_buckets.c.scope.like("%:monthly"),
            ))
        bump_cache_revision(db, "settings")
    return jsonify(read_settings())


@app.delete("/api/settings/keys/<provider>")
@admin_required
def delete_key(provider: str):
    captcha_provider = provider.removeprefix("captcha-")
    key = (f"{captcha_provider}_secret_key" if provider.startswith("captcha-")
           else f"{provider}_api_key")
    if key not in SECRET_KEYS:
        return jsonify(error=tr("Unknown provider")), 404
    current = read_settings()
    if (captcha_provider in CAPTCHA_PROVIDERS
            and current["captcha_provider"] == captcha_provider
            and any(current[name] == "1" for name in CAPTCHA_ACTION_SETTINGS.values())):
        return jsonify(error=tr("Disable CAPTCHA before removing its active secret key")), 409
    with db_lock, transaction(engine) as db:
        db.execute(delete(settings_table).where(settings_table.c.name == key))
        bump_cache_revision(db, "settings")
    return jsonify(read_settings())


def upload_display_name(raw_name: str) -> str | None:
    """Return a Unicode-preserving, filesystem-safe name, or ``None`` if unsupported.

    The extension is checked on the client's original name, so non-ASCII titles such
    as ``字幕.srt`` are accepted. The result names translated outputs inside the job
    folder, so it drops directories, separators, control/format characters, quotes,
    angle brackets, and Windows-reserved characters, normalizes to NFC, removes
    leading dots, avoids Windows device names, and caps the UTF-8 length. The
    uploaded source itself is always stored under a fixed ``source.<ext>`` name.
    """
    name = unicodedata.normalize("NFC", raw_name)
    name = re.split(r"[\\/]", name)[-1].strip()
    stem, dot, extension = name.rpartition(".")
    if not dot or f".{extension.lower()}" not in SUPPORTED_EXTENSIONS:
        return None
    characters = []
    for character in stem:
        category = unicodedata.category(character)
        if category.startswith("C") or character in UNSAFE_FILENAME_CHARACTERS:
            continue
        characters.append(" " if category.startswith("Z") else character)
    clean = re.sub(r" {2,}", " ", "".join(characters)).strip(" .")
    clean = clean.encode("utf-8")[:MAX_FILENAME_STEM_BYTES].decode("utf-8", "ignore")
    clean = clean.strip(" .") or FALLBACK_FILENAME_STEM
    if clean.split(".", 1)[0].strip().lower() in WINDOWS_RESERVED_STEMS:
        clean = "_" + clean
    return f"{clean}.{extension}"


@app.post("/api/jobs")
@jwt_required()
def create_jobs():
    user = current_user_row()
    captcha_error = verify_captcha("upload", request.form.get("captcha_token"))
    if captcha_error:
        return captcha_error
    files = request.files.getlist("files")
    if not files or all(not item.filename for item in files):
        return jsonify(error=tr("Select at least one subtitle file")), 400
    validated_files = []
    for upload in files:
        original = upload_display_name(upload.filename or "")
        if original is None:
            return jsonify(error=tr("Unsupported file: {filename}", filename=upload.filename)), 400
        validated_files.append((upload, original))
    current_settings = read_settings()
    provider = request.form.get("provider", current_settings["default_provider"])
    if provider not in available_providers():
        return jsonify(error=tr("Invalid provider")), 400
    # Duplicates would translate the same language repeatedly into one output name.
    targets = list(dict.fromkeys(value.strip() for value in request.form.get(
        "target_languages", current_settings["target_languages"]
    ).split(",") if value.strip()))
    if not targets or any(language not in LANGS for language in targets):
        return jsonify(error=tr("Choose one or more valid target languages")), 400
    if len(targets) > MAX_TARGET_LANGUAGES:
        return jsonify(error=tr(
            "Choose at most {maximum} target languages", maximum=MAX_TARGET_LANGUAGES,
        )), 400
    options = {
        "provider": provider, "model": request.form.get("model", "").strip() or None,
        "source_language": request.form.get(
            "source_language", current_settings["source_language"]
        ).strip(),
        "target_languages": targets,
        "encoding": request.form.get("encoding", "utf-8").strip(),
        "batch_size": current_settings["batch_size"], "workers": current_settings["workers"],
        "rpm": current_settings["rpm"], "width": current_settings["width"],
        "max_lines": current_settings["max_lines"],
    }
    pending = []
    try:
        for upload, original in validated_files:
            job_id = uuid.uuid4().hex
            folder = JOBS_DIR / job_id
            folder.mkdir(parents=True)
            stored = "source" + Path(original).suffix.lower()
            upload.save(folder / stored)
            pending.append((job_id, folder, original, stored))
        timestamp = now()
        with db_lock, transaction(engine) as db:
            exceeded = consume_job_quota(db, user, len(pending))
            if exceeded is None:
                for job_id, _folder, original, stored in pending:
                    db.execute(insert(jobs).values(
                        id=job_id, user_id=user.id, filename=original, stored_name=stored,
                        status="queued", progress=0, stage="", options=json.dumps(options),
                        outputs="[]", error=None, created_at=timestamp, updated_at=timestamp,
                    ))
                bump_cache_revision(db, "jobs")
    except Exception:
        for _job_id, folder, _original, _stored in pending:
            shutil.rmtree(folder, ignore_errors=True)
        raise
    if exceeded is not None:
        for _job_id, folder, _original, _stored in pending:
            shutil.rmtree(folder, ignore_errors=True)
        response = jsonify(exceeded)
        response.status_code = 429
        response.headers["Retry-After"] = str(exceeded["retry_after"])
        return response
    created = [job_id for job_id, _folder, _original, _stored in pending]
    for job_id in created:
        executor.submit(run_job, job_id)
    return jsonify(jobs=created), 202


JOB_LIST_MAX_OFFSET = 1_000_000


def bounded_int_arg(name: str, default: int, minimum: int, maximum: int) -> int:
    """Read an integer query argument, falling back on garbage and clamping to bounds."""
    return min(max(request.args.get(name, default, type=int), minimum), maximum)


@app.get("/api/jobs")
@jwt_required()
def list_jobs():
    user = current_user_row()
    limit = bounded_int_arg("limit", 50, 1, 200)
    offset = bounded_int_arg("offset", 0, 0, JOB_LIST_MAX_OFFSET)
    show_all = user.role == "admin" and request.args.get("all") == "1"
    scope = "all" if show_all else user.id
    # The id tie-breaker keeps pages stable for jobs created by one multi-file upload.
    statement = select(jobs, users.c.username.label("owner")).outerjoin(
        users, jobs.c.user_id == users.c.id
    ).order_by(jobs.c.created_at.desc(), jobs.c.id.desc()).limit(limit).offset(offset)
    count_statement = select(
        jobs.c.status, func.count().label("count"),
    ).group_by(jobs.c.status)
    if not show_all:
        statement = statement.where(jobs.c.user_id == user.id)
        count_statement = count_statement.where(jobs.c.user_id == user.id)
    rows = cached_rows("jobs", f"list:{scope}:{limit}:{offset}", statement)
    counts = {
        str(row["status"]): int(row["count"])
        for row in cached_rows("jobs", f"counts:{scope}", count_statement)
    }
    total = sum(counts.values())
    return jsonify(
        jobs=[job_dict(row, include_owner=show_all) for row in rows],
        total=total, counts=counts, offset=offset, limit=limit,
        has_more=offset + len(rows) < total,
    )


@app.get("/api/jobs/<job_id>")
@jwt_required()
def get_job(job_id: str):
    user = current_user_row()
    statement = select(jobs).where(jobs.c.id == job_id)
    if user.role != "admin":
        statement = statement.where(jobs.c.user_id == user.id)
    rows = cached_rows("jobs", f"detail:{user.role}:{user.id}:{job_id}", statement)
    row = rows[0] if rows else None
    return jsonify(job_dict(row)) if row else (jsonify(error=tr("Job not found")), 404)


@app.post("/api/jobs/<job_id>/cancel")
@jwt_required()
def cancel_job(job_id: str):
    user = current_user_row()
    with db_lock, transaction(engine) as db:
        scope = [jobs.c.id == job_id]
        if user.role != "admin":
            scope.append(jobs.c.user_id == user.id)
        if db.execute(select(jobs.c.id).where(*scope)).first() is None:
            return jsonify(error=tr("Job not found")), 404
        timestamp = now()
        # Conditional transitions: db_lock is process-local, so another worker
        # process may claim, complete, or fail the job concurrently. A queued
        # job has no worker yet and becomes terminal at once; only a running
        # job needs its worker to acknowledge the request.
        changed = db.execute(update(jobs).where(*scope, jobs.c.status == "queued").values(
            status="canceled", stage="Canceled", error=None, updated_at=timestamp,
        )).rowcount
        if not changed:
            changed = db.execute(update(jobs).where(
                *scope, jobs.c.status == "processing",
            ).values(status="canceling", stage="Canceling", updated_at=timestamp)).rowcount
        if changed:
            bump_cache_revision(db, "jobs")
        row = db.execute(select(jobs).where(*scope)).first()
    if row is None:
        return jsonify(error=tr("Job not found")), 404
    if row.status == "canceling":
        signal_local_cancel(job_id)
        return jsonify(job_dict(row)), 202
    if changed and row.status == "canceled":
        return jsonify(job_dict(row)), 202
    return jsonify(error=tr("Only an active job can be canceled")), 409


@app.delete("/api/jobs/<job_id>")
@jwt_required()
def delete_job(job_id: str):
    user = current_user_row()
    with db_lock, transaction(engine) as db:
        statement = select(jobs.c.status).where(jobs.c.id == job_id)
        if user.role != "admin":
            statement = statement.where(jobs.c.user_id == user.id)
        row = db.execute(statement).first()
        if row is None:
            return jsonify(error=tr("Job not found")), 404
        if row.status not in TERMINAL_STATUSES:
            return jsonify(error=tr("Wait for the job to finish before deleting it")), 409
        jobs_root = JOBS_DIR.resolve()
        folder = (jobs_root / job_id).resolve()
        if folder.parent != jobs_root:
            return jsonify(error=tr("Invalid job path")), 400
        try:
            if folder.exists():
                shutil.rmtree(folder)
        except OSError:
            return jsonify(error=tr("Could not delete the job files")), 500
        db.execute(delete(jobs).where(jobs.c.id == job_id))
        bump_cache_revision(db, "jobs")
        with cancel_events_lock:
            cancel_events.pop(job_id, None)
    return jsonify(deleted=job_id)


@app.get("/api/jobs/<job_id>/download/<path:name>")
@jwt_required()
def download_output(job_id: str, name: str):
    user = current_user_row()
    row = owned_job(job_id, user)
    if row is None:
        return jsonify(error=tr("Job not found")), 404
    allowed = {item["name"] for item in json.loads(row.outputs)}
    if name not in allowed:
        return jsonify(error=tr("Output not found")), 404
    return send_file(JOBS_DIR / job_id / name, as_attachment=True, download_name=name)


@app.get("/api/jobs/<job_id>/download")
@jwt_required()
def download_all(job_id: str):
    user = current_user_row()
    row = owned_job(job_id, user)
    if row is None:
        return jsonify(error=tr("Job not found")), 404
    outputs = json.loads(row.outputs)
    if not outputs:
        return jsonify(error=tr("No outputs are ready")), 404
    if len(outputs) == 1:
        return download_output(job_id, outputs[0]["name"])
    folder = JOBS_DIR / job_id
    names = [output["name"] for output in outputs]
    # Key the cached archive by its exact contents: a ZIP requested while a
    # multi-language job is still running must not be served as the final one.
    digest = hashlib.sha256(json.dumps(names).encode("utf-8")).hexdigest()[:24]
    archive = folder / f".translations-{digest}.zip"
    if not archive.exists():
        build_archive(folder, names, archive)
    return send_file(archive, as_attachment=True,
                     download_name=Path(row.filename).stem + ".translations.zip",
                     mimetype="application/zip")


def build_archive(folder: Path, names: list[str], archive: Path) -> None:
    """Write the ZIP under a private temporary name and publish it atomically.

    Concurrent requests each build their own copy; none can observe (and
    serve) a partially written archive.
    """
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=".translations-", suffix=".partial", dir=folder,
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle, \
                zipfile.ZipFile(handle, "w", zipfile.ZIP_DEFLATED) as bundle:
            for name in names:
                bundle.write(folder / name, name)
        try:
            os.replace(temporary, archive)
        except PermissionError:
            # Windows cannot replace a file another request is sending. That
            # file is an identical, complete archive, so use it.
            if not archive.exists():
                raise
    finally:
        temporary.unlink(missing_ok=True)


@app.errorhandler(413)
def too_large(_error):
    return jsonify(error=tr(
        "Upload exceeds the {max_upload_mb} MB limit", max_upload_mb=MAX_UPLOAD_MB,
    )), 413


if __name__ == "__main__":
    app.run(host="127.0.0.1", port=int(os.environ.get("PORT", "8000")), debug=app.debug)
