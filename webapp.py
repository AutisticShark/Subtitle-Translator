"""Flask application for authenticated browser-based subtitle translation."""

from __future__ import annotations

import base64
import hashlib
import json
import logging
import math
import os
import re
import secrets
import shutil
import sqlite3
import threading
import urllib.error
import urllib.parse
import urllib.request
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
from sqlalchemy import delete, func, insert, select, update
from sqlalchemy.exc import IntegrityError
from werkzeug.utils import secure_filename

from database import (
    connection, create_database_engine, initialize_database, jobs, revoked_tokens,
    rate_limit_buckets, settings as settings_table, transaction, users,
)
from i18n import (
    LOCALE_COOKIE, LOCALE_LABELS, current_locale, messages_for, normalize_locale,
    translate as tr,
)
from srt_translate import (
    LANGS, FatalTranslationError, Throttle, TranslationCanceled, make_anthropic,
    make_deepl, make_echo, make_google, make_openai, rebuild_cues, segment_cue,
    translate_segments,
)
from subtitle_formats import SUPPORTED_EXTENSIONS, load_subtitle, translated_filename


LOGGER = logging.getLogger(__name__)
BASE_DIR = Path(__file__).resolve().parent
DATA_DIR = Path(os.environ.get("DATA_DIR", BASE_DIR / "data")).resolve()
JOBS_DIR = DATA_DIR / "jobs"
DB_PATH = DATA_DIR / "app.db"
MAX_UPLOAD_MB = int(os.environ.get("MAX_UPLOAD_MB", "200"))
USERNAME_PATTERN = re.compile(r"^[a-z0-9](?:[a-z0-9_.-]{1,62}[a-z0-9])?$")
PASSWORD_MIN_LENGTH = 12
LOGIN_FAILURE_LIMIT = 5
LOGIN_LOCK_MINUTES = 15
LOGIN_RATE_LIMIT = 30
REGISTER_RATE_LIMIT = 10
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


def parse_timestamp(value: str | None) -> datetime | None:
    if not value:
        return None
    try:
        parsed = datetime.fromisoformat(value)
    except ValueError:
        return None
    return parsed.replace(tzinfo=parsed.tzinfo or timezone.utc)


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
RATE_LIMIT_KEYS = {
    "rate_limit_window_minutes", "user_job_limit", "admin_job_limit",
    "panel_job_limit",
}
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
CAPTCHA_PROVIDERS = ("turnstile", "recaptcha", "hcaptcha")
CAPTCHA_ACTION_SETTINGS = {
    "login": "captcha_on_login",
    "register": "captcha_on_register",
    "upload": "captcha_on_upload",
}
CAPTCHA_VERIFY_URLS = {
    "turnstile": "https://challenges.cloudflare.com/turnstile/v0/siteverify",
    "recaptcha": "https://www.google.com/recaptcha/api/siteverify",
    "hcaptcha": "https://api.hcaptcha.com/siteverify",
}
if DEFAULTS["captcha_provider"] not in {"none", *CAPTCHA_PROVIDERS}:
    raise RuntimeError("CAPTCHA_PROVIDER must be none, turnstile, recaptcha, or hcaptcha")
for _boolean_setting in {
    "registration_enabled", *CAPTCHA_ACTION_SETTINGS.values(),
}:
    if DEFAULTS[_boolean_setting] not in {"0", "1"}:
        raise RuntimeError(f"{_boolean_setting.upper()} must be 0 or 1")


DATA_DIR.mkdir(parents=True, exist_ok=True)
JOBS_DIR.mkdir(parents=True, exist_ok=True)
engine = create_database_engine(DB_PATH)
initialize_database(engine, DEFAULTS, now())

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
executor = ThreadPoolExecutor(max_workers=max(1, int(os.environ.get("JOB_WORKERS", "2"))))
db_lock = threading.RLock()
cancel_events_lock = threading.Lock()
cancel_events: dict[str, threading.Event] = {}
login_attempts_lock = threading.Lock()
login_attempts: dict[str, list[datetime]] = {}
registration_attempts_lock = threading.Lock()
registration_attempts: dict[str, list[datetime]] = {}


def available_providers() -> tuple[str, ...]:
    return PUBLIC_PROVIDERS + (("echo",) if app.debug else ())


def normalize_username(value: Any) -> str:
    return str(value or "").strip().lower()


def json_payload() -> dict[str, Any]:
    payload = request.get_json(silent=True)
    return payload if isinstance(payload, dict) else {}


def login_rate_limited(remote_address: str) -> bool:
    cutoff = now_datetime() - timedelta(minutes=LOGIN_LOCK_MINUTES)
    with login_attempts_lock:
        recent = [value for value in login_attempts.get(remote_address, []) if value > cutoff]
        if recent:
            login_attempts[remote_address] = recent
        else:
            login_attempts.pop(remote_address, None)
        return len(recent) >= LOGIN_RATE_LIMIT


def record_login_failure(remote_address: str) -> None:
    with login_attempts_lock:
        login_attempts.setdefault(remote_address, []).append(now_datetime())


def registration_rate_limited(remote_address: str) -> bool:
    cutoff = now_datetime() - timedelta(minutes=LOGIN_LOCK_MINUTES)
    with registration_attempts_lock:
        recent = [
            value for value in registration_attempts.get(remote_address, [])
            if value > cutoff
        ]
        if len(recent) >= REGISTER_RATE_LIMIT:
            registration_attempts[remote_address] = recent
            return True
        recent.append(now_datetime())
        registration_attempts[remote_address] = recent
        return False


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


def issue_token(user_row: Any) -> str:
    values = user_row._mapping if hasattr(user_row, "_mapping") else user_row
    return create_access_token(
        identity=values["id"],
        additional_claims={"role": values["role"], "ver": values["token_version"]},
        fresh=True,
    )


def bootstrap_admin() -> None:
    password = os.environ.get("ADMIN_PASSWORD", "") or os.environ.get("APP_PASSWORD", "")
    if not password:
        return
    username = normalize_username(os.environ.get("ADMIN_USERNAME", "admin"))
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
            db.execute(insert(users).values(
                id=user_id, username=username, password_hash=password_hasher.hash(password),
                role="admin", active=True, token_version=0, failed_login_count=0,
                locked_until=None, created_at=timestamp, updated_at=timestamp,
            ))
            db.execute(update(jobs).where(jobs.c.user_id.is_(None)).values(user_id=user_id))
    except IntegrityError:
        with connection(engine) as db:
            if not db.scalar(select(func.count()).select_from(users)):
                raise
    if os.environ.get("APP_PASSWORD") and not os.environ.get("ADMIN_PASSWORD"):
        LOGGER.warning("APP_PASSWORD is deprecated; it was used to bootstrap the admin account")


bootstrap_admin()


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


encrypt_existing_api_keys()


@jwt.token_in_blocklist_loader
def token_is_revoked(_header: dict, payload: dict) -> bool:
    with connection(engine) as db:
        if db.scalar(select(revoked_tokens.c.jti).where(
            revoked_tokens.c.jti == payload.get("jti", "")
        )):
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
    if request.endpoint == "logout":
        return response
    try:
        verify_jwt_in_request(optional=True)
        claims = get_jwt()
        if (get_jwt_request_location() == "cookies" and get_jwt_identity()
                and claims.get("exp") and datetime.fromtimestamp(
            claims["exp"], timezone.utc
        ) < now_datetime() + timedelta(minutes=10)):
            user = current_user_row()
            if user is not None and user.active:
                set_access_cookies(response, issue_token(user))
    except Exception:
        pass
    return response


def read_settings(include_secrets: bool = False) -> dict[str, Any]:
    with connection(engine) as db:
        values = dict(db.execute(select(
            settings_table.c.name, settings_table.c.value
        )).all())
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
    site_key = settings.get(f"{provider}_site_key", "").strip()
    secret_key = settings.get(f"{provider}_secret_key", "").strip()
    if not site_key or not secret_key:
        LOGGER.error("CAPTCHA provider %s is active but is missing a site or secret key", provider)
        return jsonify(error=tr("CAPTCHA is temporarily unavailable")), 503
    if not isinstance(token, str) or not token.strip():
        return jsonify(error=tr("Complete the CAPTCHA challenge")), 400
    token = token.strip()
    if len(token) > 8192:
        return jsonify(error=tr("CAPTCHA verification failed; please try again")), 400
    form = {
        "secret": secret_key,
        "response": token,
    }
    if request.remote_addr:
        form["remoteip"] = request.remote_addr
    if provider == "hcaptcha":
        form["sitekey"] = site_key
    verification_request = urllib.request.Request(
        CAPTCHA_VERIFY_URLS[provider],
        data=urllib.parse.urlencode(form).encode("ascii"),
        headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "User-Agent": "Subtitle-Translator CAPTCHA verifier",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(verification_request, timeout=8) as response:
            result = json.loads(response.read(65537))
    except (urllib.error.URLError, TimeoutError, OSError, json.JSONDecodeError, ValueError) as exc:
        LOGGER.warning("%s CAPTCHA verification service error: %s", provider, type(exc).__name__)
        return jsonify(error=tr("CAPTCHA is temporarily unavailable")), 503
    if not isinstance(result, dict) or result.get("success") is not True:
        error_codes = result.get("error-codes", []) if isinstance(result, dict) else []
        LOGGER.info("%s CAPTCHA rejected a token: %s", provider, error_codes)
        return jsonify(error=tr("CAPTCHA verification failed; please try again")), 400
    expected_hostname = settings.get("captcha_hostname", "").strip().lower().rstrip(".")
    if not expected_hostname:
        expected_hostname = (urllib.parse.urlsplit(request.url_root).hostname or "").lower().rstrip(".")
    actual_hostname = str(result.get("hostname", "")).lower().rstrip(".")
    if expected_hostname and actual_hostname != expected_hostname:
        LOGGER.warning("%s CAPTCHA returned an unexpected hostname", provider)
        return jsonify(error=tr("CAPTCHA verification failed; please try again")), 400
    if provider == "turnstile" and result.get("action") != action:
        LOGGER.warning("Turnstile CAPTCHA returned an unexpected action")
        return jsonify(error=tr("CAPTCHA verification failed; please try again")), 400
    return None


def consume_job_quota(db, user: Any, amount: int) -> dict[str, Any] | None:
    """Atomically consume job quota or describe the exceeded limit."""
    timestamp_datetime = now_datetime()
    timestamp = timestamp_datetime.isoformat(timespec="seconds")

    # Every job submission and rate-setting update takes this database row lock first.
    # It serializes the panel counter across application workers and database backends.
    db.execute(update(settings_table).where(
        settings_table.c.name == "panel_job_limit"
    ).values(updated_at=settings_table.c.updated_at))
    stored = dict(db.execute(select(
        settings_table.c.name, settings_table.c.value
    ).where(settings_table.c.name.in_(RATE_LIMIT_KEYS))).all())
    limits = {key: int(stored.get(key, DEFAULTS[key])) for key in RATE_LIMIT_KEYS}
    window = timedelta(minutes=limits["rate_limit_window_minutes"])
    account_scope = f"user:{user.id}"
    rows = {
        row.scope: row
        for row in db.execute(select(rate_limit_buckets).where(
            rate_limit_buckets.c.scope.in_(("panel", account_scope))
        )).all()
    }

    def bucket(scope: str) -> tuple[datetime, int]:
        row = rows.get(scope)
        started = parse_timestamp(row.window_started_at) if row else None
        if started is None or timestamp_datetime >= started + window:
            return timestamp_datetime, 0
        return started, int(row.used)

    panel_started, panel_used = bucket("panel")
    account_started, account_used = bucket(account_scope)
    account_key = "admin_job_limit" if user.role == "admin" else "user_job_limit"
    checks = (
        (user.role, limits[account_key], account_started, account_used),
        ("panel", limits["panel_job_limit"], panel_started, panel_used),
    )
    for scope, limit, started, used in checks:
        if limit and used + amount > limit:
            retry_after = max(1, math.ceil((started + window - timestamp_datetime).total_seconds()))
            label = tr("Administrator") if scope == "admin" else (
                tr("Regular-user") if scope == "user" else tr("Panel-wide")
            )
            return {
                "error": tr(
                    "{label} rate limit of {limit} translation job{job_plural} per "
                    "{minutes} minute{minute_plural} exceeded",
                    label=label, limit=limit, job_plural="s" if limit != 1 else "",
                    minutes=limits["rate_limit_window_minutes"],
                    minute_plural=(
                        "s" if limits["rate_limit_window_minutes"] != 1 else ""
                    ),
                ),
                "scope": scope,
                "limit": limit,
                "retry_after": retry_after,
            }

    for scope, started, used in (
        ("panel", panel_started, panel_used),
        (account_scope, account_started, account_used),
    ):
        values = {
            "window_started_at": started.isoformat(timespec="seconds"),
            "used": used + amount,
            "updated_at": timestamp,
        }
        if scope in rows:
            db.execute(update(rate_limit_buckets).where(
                rate_limit_buckets.c.scope == scope
            ).values(**values))
        else:
            db.execute(insert(rate_limit_buckets).values(scope=scope, **values))
    return None


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


def job_dict(row: Any, include_owner: bool = False) -> dict[str, Any]:
    source = dict(row._mapping if hasattr(row, "_mapping") else row)
    result = {key: source.get(key) for key in jobs.c.keys()}
    result["options"] = json.loads(result["options"])
    result["outputs"] = json.loads(result["outputs"])
    result["stage"] = localized_job_stage(result.get("stage") or "")
    result.pop("stored_name", None)
    result.pop("user_id", None)
    if include_owner:
        result["owner"] = source.get("owner")
    return result


def update_job(job_id: str, **fields: Any) -> None:
    fields["updated_at"] = now()
    with db_lock, transaction(engine) as db:
        db.execute(update(jobs).where(jobs.c.id == job_id).values(**fields))


def update_job_if_status(job_id: str, statuses: set[str], **fields: Any) -> bool:
    fields["updated_at"] = now()
    with db_lock, transaction(engine) as db:
        result = db.execute(update(jobs).where(
            jobs.c.id == job_id, jobs.c.status.in_(statuses)
        ).values(**fields))
        return result.rowcount == 1


def cancel_event_for(job_id: str) -> threading.Event:
    with cancel_events_lock:
        return cancel_events.setdefault(job_id, threading.Event())


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

    def check_canceled() -> None:
        if cancel_event.is_set():
            raise TranslationCanceled("Translation canceled")

    try:
        with connection(engine) as db:
            row = db.execute(select(jobs).where(jobs.c.id == job_id)).first()
        if row is None:
            return
        if row.status == "canceling":
            cancel_event.set()
        check_canceled()
        options = json.loads(row.options)
        folder = JOBS_DIR / job_id
        source = folder / row.stored_name
        if not update_job_if_status(
            job_id, {"queued"}, status="processing", progress=2, stage="Reading subtitle",
        ):
            check_canceled()
            return
        document = load_subtitle(source, options.get("encoding", "utf-8"))
        segments = []
        for cue_i, cue in enumerate(document.cues):
            segments.extend(segment_cue(cue, cue_i))
        check_canceled()
        update_job(job_id, progress=5, stage=f"Parsed {len(document.cues)} cues")

        provider_settings = read_settings(include_secrets=True)
        throttle = Throttle(float(options["rpm"]), cancel_event.is_set)
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
        for index, language in enumerate(targets):
            start_pct = 5 + round(index / len(targets) * 90)
            update_job(job_id, progress=start_pct,
                       stage=f"Translating to {LANGS[language]['name']}")

            def report_progress(done: int, total: int, *, target_index: int = index,
                                target_language: str = language) -> None:
                fraction = done / total if total else 1
                progress = 5 + round((target_index + fraction) / len(targets) * 90)
                update_job(
                    job_id, progress=progress,
                    stage=(f"Translating to {LANGS[target_language]['name']} "
                           f"({done}/{total} segments)"),
                )

            translated = translate_segments(
                segments, provider, language, options["source_language"],
                int(options["batch_size"]), 4, 10, throttle, cache,
                int(options["workers"]), True, report_progress, cancel_event.is_set,
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
            update_job(job_id, outputs=json.dumps(outputs),
                       progress=5 + round((index + 1) / len(targets) * 90))
        if not update_job_if_status(
            job_id, {"processing"}, status="completed", progress=100,
            stage="Ready to download", outputs=json.dumps(outputs), error=None,
        ):
            raise TranslationCanceled("Translation canceled")
    except TranslationCanceled:
        update_job_if_status(
            job_id, {"queued", "processing", "canceling"}, status="canceled",
            stage="Canceled", error=None,
        )
    except Exception as exc:
        if cancel_event.is_set():
            update_job_if_status(
                job_id, {"queued", "processing", "canceling"}, status="canceled",
                stage="Canceled", error=None,
            )
        else:
            update_job_if_status(
                job_id, {"queued", "processing"}, status="failed",
                stage="Translation failed", error=f"{type(exc).__name__}: {exc}",
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
            db.execute(insert(users).values(
                id=user_id, username=username, password_hash=password_hasher.hash(password),
                role="admin", active=True, token_version=0, failed_login_count=0,
                locked_until=None, created_at=timestamp, updated_at=timestamp,
            ))
            db.execute(update(jobs).where(jobs.c.user_id.is_(None)).values(user_id=user_id))
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
    if login_rate_limited(remote_address):
        return jsonify(error=tr("Too many login attempts; try again later")), 429
    payload = json_payload()
    captcha_error = verify_captcha("login", payload.get("captcha_token"))
    if captcha_error:
        return captcha_error
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
    candidate_hash = user.password_hash if user is not None else DUMMY_PASSWORD_HASH
    try:
        verified = password_hasher.verify(candidate_hash, candidate_password)
    except (VerifyMismatchError, VerificationError, InvalidHashError):
        verified = False
    locked_until = parse_timestamp(user.locked_until) if user is not None else None
    locked = bool(locked_until and locked_until > now_datetime())
    if user is None or not valid_input or not verified or not user.active or locked:
        record_login_failure(remote_address)
        if user is not None and user.active and not locked:
            failures = user.failed_login_count + 1
            values: dict[str, Any] = {"failed_login_count": failures, "updated_at": now()}
            if failures >= LOGIN_FAILURE_LIMIT:
                values.update(
                    locked_until=(now_datetime() + timedelta(
                        minutes=LOGIN_LOCK_MINUTES
                    )).isoformat(timespec="seconds"),
                    failed_login_count=0,
                )
            with transaction(engine) as db:
                db.execute(update(users).where(users.c.id == user.id).values(**values))
        return jsonify(error=tr("Invalid username or password")), 401
    values = {"failed_login_count": 0, "locked_until": None, "updated_at": now()}
    if password_hasher.check_needs_rehash(user.password_hash):
        values["password_hash"] = password_hasher.hash(candidate_password)
    with transaction(engine) as db:
        db.execute(update(users).where(users.c.id == user.id).values(**values))
    with connection(engine) as db:
        refreshed_user = db.execute(select(users).where(users.c.id == user.id)).first()
    access_token = issue_token(refreshed_user)
    if payload.get("token_transport") == "header":
        response = jsonify(user=public_user(refreshed_user), access_token=access_token)
    else:
        response = jsonify(user=public_user(refreshed_user))
        set_access_cookies(response, access_token)
    return response


@app.post("/api/auth/register")
def register():
    if request.content_length and request.content_length > 8192:
        return jsonify(error=tr("Authentication request is too large")), 413
    remote_address = request.remote_addr or "unknown"
    if registration_rate_limited(remote_address):
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
            db.execute(insert(users).values(
                id=user_id, username=username, password_hash=password_hasher.hash(password),
                role="user", active=True, token_version=0, failed_login_count=0,
                locked_until=None, created_at=timestamp, updated_at=timestamp,
            ))
    except IntegrityError:
        return jsonify(error=tr("Username already exists")), 409
    with connection(engine) as db:
        user = db.execute(select(users).where(users.c.id == user_id)).first()
    response = jsonify(user=public_user(user))
    set_access_cookies(response, issue_token(user))
    return response, 201


@app.post("/api/auth/logout")
@jwt_required()
def logout():
    claims = get_jwt()
    expires_at = datetime.fromtimestamp(claims["exp"], timezone.utc).isoformat(timespec="seconds")
    with transaction(engine) as db:
        if not db.scalar(select(revoked_tokens.c.jti).where(
            revoked_tokens.c.jti == claims["jti"]
        )):
            db.execute(insert(revoked_tokens).values(
                jti=claims["jti"], expires_at=expires_at, created_at=now()
            ))
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
    if theme not in ACCOUNT_THEMES:
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
    if role not in {"user", "admin"}:
        return jsonify(error=tr("Invalid role")), 400
    user_id = uuid.uuid4().hex
    timestamp = now()
    try:
        with transaction(engine) as db:
            db.execute(insert(users).values(
                id=user_id, username=username, password_hash=password_hasher.hash(password),
                role=role, active=True, token_version=0, failed_login_count=0,
                locked_until=None, created_at=timestamp, updated_at=timestamp,
            ))
    except IntegrityError:
        return jsonify(error=tr("Username already exists")), 409
    with connection(engine) as db:
        user = db.execute(select(users).where(users.c.id == user_id)).first()
    return jsonify(user=public_user(user)), 201


def active_admin_count(db) -> int:
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
        if payload["role"] not in {"user", "admin"}:
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
    if payload.get("unlock") is True:
        values["failed_login_count"] = 0
        values["locked_until"] = None
    if user_id == actor.id and (
        values.get("active") is False or values.get("role") == "user"
    ):
        return jsonify(error=tr("You cannot deactivate or demote your own account")), 409
    with transaction(engine) as db:
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
            rate_limit_buckets.c.scope == f"user:{user_id}"
        ))
        db.execute(delete(users).where(users.c.id == user_id))
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
    if payload.get("default_provider") and payload["default_provider"] not in available_providers():
        return jsonify(error=tr("Invalid default provider")), 400
    if ("captcha_provider" in payload
            and payload["captcha_provider"] not in {"none", *CAPTCHA_PROVIDERS}):
        return jsonify(error=tr("Invalid CAPTCHA provider")), 400
    for key in {"registration_enabled", *CAPTCHA_ACTION_SETTINGS.values()}:
        if key in payload and str(payload[key]).strip() not in {"0", "1"}:
            return jsonify(error=tr("{key} must be enabled or disabled", key=key)), 400
    if "captcha_hostname" in payload:
        hostname = str(payload["captcha_hostname"]).strip().lower().rstrip(".")
        if hostname and not re.fullmatch(
            r"(?=.{1,253}\Z)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*"
            r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?",
            hostname,
        ):
            return jsonify(error=tr("CAPTCHA hostname must be a hostname without a scheme or port")), 400
        payload["captcha_hostname"] = hostname
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
    numeric = {"batch_size": (1, 100), "workers": (1, 16), "rpm": (0, 10000),
               "width": (4, 80), "max_lines": (1, 5)}
    integer_numeric = {
        "rate_limit_window_minutes": (1, 10080),
        "user_job_limit": (0, 100000),
        "admin_job_limit": (0, 100000),
        "panel_job_limit": (0, 1000000),
    }
    try:
        for key, (minimum, maximum) in numeric.items():
            if key in payload and not minimum <= float(payload[key]) <= maximum:
                raise ValueError(tr(
                    "{key} must be between {minimum} and {maximum}",
                    key=key, minimum=minimum, maximum=maximum,
                ))
        for key, (minimum, maximum) in integer_numeric.items():
            if key not in payload:
                continue
            try:
                value = int(str(payload[key]).strip())
            except (TypeError, ValueError) as exc:
                raise ValueError(
                    tr(
                        "{key} must be a whole number between {minimum} and {maximum}",
                        key=key, minimum=minimum, maximum=maximum,
                    )
                ) from exc
            if str(value) != str(payload[key]).strip() or not minimum <= value <= maximum:
                raise ValueError(tr(
                    "{key} must be a whole number between {minimum} and {maximum}",
                    key=key, minimum=minimum, maximum=maximum,
                ))
    except (TypeError, ValueError) as exc:
        return jsonify(error=str(exc)), 400
    timestamp = now()
    with db_lock, transaction(engine) as db:
        if RATE_LIMIT_KEYS & payload.keys():
            db.execute(update(settings_table).where(
                settings_table.c.name == "panel_job_limit"
            ).values(updated_at=settings_table.c.updated_at))
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
        if RATE_LIMIT_KEYS & payload.keys():
            db.execute(delete(rate_limit_buckets))
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
    return jsonify(read_settings())


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
        original = secure_filename(upload.filename or "")
        if not original or Path(original).suffix.lower() not in SUPPORTED_EXTENSIONS:
            return jsonify(error=tr("Unsupported file: {filename}", filename=upload.filename)), 400
        validated_files.append((upload, original))
    current_settings = read_settings()
    provider = request.form.get("provider", current_settings["default_provider"])
    if provider not in available_providers():
        return jsonify(error=tr("Invalid provider")), 400
    targets = [value.strip() for value in request.form.get(
        "target_languages", current_settings["target_languages"]
    ).split(",") if value.strip()]
    if not targets or any(language not in LANGS for language in targets):
        return jsonify(error=tr("Choose one or more valid target languages")), 400
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


@app.get("/api/jobs")
@jwt_required()
def list_jobs():
    user = current_user_row()
    limit = min(max(request.args.get("limit", 50, type=int), 1), 200)
    show_all = user.role == "admin" and request.args.get("all") == "1"
    statement = select(jobs, users.c.username.label("owner")).outerjoin(
        users, jobs.c.user_id == users.c.id
    ).order_by(jobs.c.created_at.desc()).limit(limit)
    if not show_all:
        statement = statement.where(jobs.c.user_id == user.id)
    with connection(engine) as db:
        rows = db.execute(statement).all()
    return jsonify(jobs=[job_dict(row, include_owner=show_all) for row in rows])


@app.get("/api/jobs/<job_id>")
@jwt_required()
def get_job(job_id: str):
    user = current_user_row()
    row = owned_job(job_id, user)
    return jsonify(job_dict(row)) if row else (jsonify(error=tr("Job not found")), 404)


@app.post("/api/jobs/<job_id>/cancel")
@jwt_required()
def cancel_job(job_id: str):
    user = current_user_row()
    with db_lock, transaction(engine) as db:
        statement = select(jobs).where(jobs.c.id == job_id)
        if user.role != "admin":
            statement = statement.where(jobs.c.user_id == user.id)
        row = db.execute(statement).first()
        if row is None:
            return jsonify(error=tr("Job not found")), 404
        if row.status == "canceling":
            cancel_event_for(job_id).set()
            return jsonify(job_dict(row)), 202
        if row.status not in {"queued", "processing"}:
            return jsonify(error=tr("Only an active job can be canceled")), 409
        db.execute(update(jobs).where(jobs.c.id == job_id).values(
            status="canceling", stage="Canceling", updated_at=now()
        ))
        cancel_event_for(job_id).set()
        row = db.execute(select(jobs).where(jobs.c.id == job_id)).first()
    return jsonify(job_dict(row)), 202


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
    archive = JOBS_DIR / job_id / (Path(row.filename).stem + ".translations.zip")
    if not archive.exists():
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as bundle:
            for output in outputs:
                bundle.write(JOBS_DIR / job_id / output["name"], output["name"])
    return send_file(archive, as_attachment=True, download_name=archive.name,
                     mimetype="application/zip")


@app.errorhandler(413)
def too_large(_error):
    return jsonify(error=tr(
        "Upload exceeds the {max_upload_mb} MB limit", max_upload_mb=MAX_UPLOAD_MB,
    )), 413


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", "8000")), debug=app.debug)
