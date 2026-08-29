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
import threading
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
from flask import Flask, jsonify, render_template, request, send_file
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
    settings as settings_table, transaction, users,
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
TERMINAL_STATUSES = {"completed", "failed", "canceled"}
ACTIVE_STATUSES = {"queued", "processing", "canceling"}


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
}
SECRET_KEYS = {
    "anthropic_api_key", "openai_api_key", "deepl_api_key", "google_api_key",
}
PUBLIC_KEYS = set(DEFAULTS)
ALL_SETTING_KEYS = PUBLIC_KEYS | SECRET_KEYS
PROVIDER_LABELS = {
    "anthropic": "Anthropic", "openai": "OpenAI-compatible", "deepl": "DeepL",
    "google": "Google Cloud Translation", "echo": "Echo (offline test)",
}
PUBLIC_PROVIDERS = ("anthropic", "openai", "deepl", "google")


DATA_DIR.mkdir(parents=True, exist_ok=True)
JOBS_DIR.mkdir(parents=True, exist_ok=True)
engine = create_database_engine(DB_PATH)
initialize_database(engine, DEFAULTS, now())

configured_jwt_secret = os.environ.get("JWT_SECRET_KEY", "").strip()
jwt_secret = configured_jwt_secret or secrets.token_urlsafe(64)
if not configured_jwt_secret:
    LOGGER.warning(
        "JWT_SECRET_KEY is not configured; generated tokens will become invalid after restart "
        "and API keys cannot be saved"
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


def validate_username(username: str) -> str | None:
    if not USERNAME_PATTERN.fullmatch(username):
        return "Username must be 3-64 lowercase letters, numbers, dots, dashes, or underscores"
    return None


def validate_password(password: Any) -> str | None:
    if not isinstance(password, str) or len(password) < PASSWORD_MIN_LENGTH:
        return f"Password must contain at least {PASSWORD_MIN_LENGTH} characters"
    if len(password) > 256:
        return "Password is too long"
    return None


def public_user(row: Any) -> dict[str, Any]:
    values = row._mapping if hasattr(row, "_mapping") else row
    locked_until = parse_timestamp(values.get("locked_until"))
    result = {
        "id": values["id"], "username": values["username"], "role": values["role"],
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
            "A saved API key cannot be decrypted; restore the configured JWT/encryption key"
        ) from exc


def encrypt_existing_api_keys() -> None:
    """Upgrade values written by versions that stored provider keys as plaintext."""
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
    return jsonify(error="Authentication required", detail=reason), 401


@jwt.invalid_token_loader
def invalid_token(reason: str):
    return jsonify(error="Invalid authentication token", detail=reason), 401


@jwt.expired_token_loader
def expired_token(_header: dict, _payload: dict):
    return jsonify(error="Authentication token expired"), 401


@jwt.revoked_token_loader
def revoked_token(_header: dict, _payload: dict):
    return jsonify(error="Authentication token revoked"), 401


def current_user_row() -> Any | None:
    with connection(engine) as db:
        return db.execute(select(users).where(users.c.id == get_jwt_identity())).first()


def admin_required(function: Callable):
    @wraps(function)
    @jwt_required()
    def wrapped(*args, **kwargs):
        user = current_user_row()
        if user is None or user.role != "admin":
            return jsonify(error="Administrator access required"), 403
        return function(*args, **kwargs)
    return wrapped


@app.after_request
def security_headers(response):
    response.headers.setdefault("X-Content-Type-Options", "nosniff")
    response.headers.setdefault("X-Frame-Options", "DENY")
    response.headers.setdefault("Referrer-Policy", "no-referrer")
    response.headers.setdefault(
        "Content-Security-Policy",
        "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "
        "img-src 'self' data:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'",
    )
    response.headers.setdefault("Cache-Control", "no-store")
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
    result["providers"] = {name: PROVIDER_LABELS[name] for name in providers}
    result["configured"] = {name: configured[name] for name in providers}
    if include_secrets:
        for key in SECRET_KEYS:
            stored = values.get(key, "")
            result[key] = decrypt_secret(stored) if stored else os.environ.get(key.upper(), "")
    return result


def job_dict(row: Any, include_owner: bool = False) -> dict[str, Any]:
    source = dict(row._mapping if hasattr(row, "_mapping") else row)
    result = {key: source.get(key) for key in jobs.c.keys()}
    result["options"] = json.loads(result["options"])
    result["outputs"] = json.loads(result["outputs"])
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
    return render_template("index.html", languages=LANGS, max_upload_mb=MAX_UPLOAD_MB)


@app.get("/healthz")
def health():
    return jsonify(status="ok")


@app.get("/api/auth/setup-status")
def setup_status():
    with connection(engine) as db:
        configured = bool(db.scalar(select(func.count()).select_from(users)))
    return jsonify(configured=configured)


@app.post("/api/auth/setup")
def setup_first_admin():
    if request.content_length and request.content_length > 8192:
        return jsonify(error="Authentication request is too large"), 413
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
                return jsonify(error="Initial setup is already complete"), 409
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
        return jsonify(error="Initial setup is already complete"), 409
    with connection(engine) as db:
        user = db.execute(select(users).where(users.c.id == user_id)).first()
    response = jsonify(user=public_user(user))
    set_access_cookies(response, issue_token(user))
    return response, 201


@app.post("/api/auth/login")
def login():
    if request.content_length and request.content_length > 8192:
        return jsonify(error="Authentication request is too large"), 413
    remote_address = request.remote_addr or "unknown"
    if login_rate_limited(remote_address):
        return jsonify(error="Too many login attempts; try again later"), 429
    payload = json_payload()
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
        return jsonify(error="Invalid username or password"), 401
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
    response = jsonify(message="Logged out")
    unset_jwt_cookies(response)
    return response


@app.get("/api/auth/me")
@jwt_required()
def who_am_i():
    user = current_user_row()
    return jsonify(user=public_user(user)) if user else (jsonify(error="User not found"), 401)


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
        return jsonify(error="Invalid role"), 400
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
        return jsonify(error="Username already exists"), 409
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
        return jsonify(error=f"Unknown fields: {', '.join(sorted(unknown))}"), 400
    values: dict[str, Any] = {"updated_at": now()}
    if "role" in payload:
        if payload["role"] not in {"user", "admin"}:
            return jsonify(error="Invalid role"), 400
        values["role"] = payload["role"]
    if "active" in payload:
        if not isinstance(payload["active"], bool):
            return jsonify(error="active must be a boolean"), 400
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
        return jsonify(error="You cannot deactivate or demote your own account"), 409
    with transaction(engine) as db:
        target = db.execute(select(users).where(users.c.id == user_id)).first()
        if target is None:
            return jsonify(error="User not found"), 404
        removes_admin = target.role == "admin" and target.active and (
            values.get("active") is False or values.get("role") == "user"
        )
        if removes_admin and active_admin_count(db) <= 1:
            return jsonify(error="At least one active administrator is required"), 409
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
        return jsonify(error="You cannot delete your own account"), 409
    with transaction(engine) as db:
        target = db.execute(select(users).where(users.c.id == user_id)).first()
        if target is None:
            return jsonify(error="User not found"), 404
        if target.role == "admin" and target.active and active_admin_count(db) <= 1:
            return jsonify(error="At least one active administrator is required"), 409
        if db.scalar(select(func.count()).select_from(jobs).where(
            jobs.c.user_id == user_id, jobs.c.status.in_(ACTIVE_STATUSES)
        )):
            return jsonify(error="Cancel or finish this user's active jobs first"), 409
        db.execute(update(jobs).where(jobs.c.user_id == user_id).values(user_id=None))
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
        return jsonify(error=f"Unknown settings: {', '.join(sorted(unknown))}"), 400
    if payload.get("default_provider") and payload["default_provider"] not in available_providers():
        return jsonify(error="Invalid default provider"), 400
    if any(key in payload and str(payload[key]).strip() for key in SECRET_KEYS):
        if not configured_jwt_secret and not os.environ.get("API_KEY_ENCRYPTION_KEY"):
            return jsonify(error="Configure JWT_SECRET_KEY before saving API keys"), 503
    numeric = {"batch_size": (1, 100), "workers": (1, 16), "rpm": (0, 10000),
               "width": (4, 80), "max_lines": (1, 5)}
    try:
        for key, (minimum, maximum) in numeric.items():
            if key in payload and not minimum <= float(payload[key]) <= maximum:
                raise ValueError(f"{key} must be between {minimum} and {maximum}")
    except (TypeError, ValueError) as exc:
        return jsonify(error=str(exc)), 400
    timestamp = now()
    with db_lock, transaction(engine) as db:
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
    return jsonify(read_settings())


@app.delete("/api/settings/keys/<provider>")
@admin_required
def delete_key(provider: str):
    key = f"{provider}_api_key"
    if key not in SECRET_KEYS:
        return jsonify(error="Unknown provider"), 404
    with db_lock, transaction(engine) as db:
        db.execute(delete(settings_table).where(settings_table.c.name == key))
    return jsonify(read_settings())


@app.post("/api/jobs")
@jwt_required()
def create_jobs():
    user = current_user_row()
    files = request.files.getlist("files")
    if not files or all(not item.filename for item in files):
        return jsonify(error="Select at least one subtitle file"), 400
    validated_files = []
    for upload in files:
        original = secure_filename(upload.filename or "")
        if not original or Path(original).suffix.lower() not in SUPPORTED_EXTENSIONS:
            return jsonify(error=f"Unsupported file: {upload.filename}"), 400
        validated_files.append((upload, original))
    current_settings = read_settings()
    provider = request.form.get("provider", current_settings["default_provider"])
    if provider not in available_providers():
        return jsonify(error="Invalid provider"), 400
    targets = [value.strip() for value in request.form.get(
        "target_languages", current_settings["target_languages"]
    ).split(",") if value.strip()]
    if not targets or any(language not in LANGS for language in targets):
        return jsonify(error="Choose one or more valid target languages"), 400
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
    created = []
    for upload, original in validated_files:
        job_id = uuid.uuid4().hex
        folder = JOBS_DIR / job_id
        folder.mkdir(parents=True)
        stored = "source" + Path(original).suffix.lower()
        try:
            upload.save(folder / stored)
            timestamp = now()
            with db_lock, transaction(engine) as db:
                db.execute(insert(jobs).values(
                    id=job_id, user_id=user.id, filename=original, stored_name=stored,
                    status="queued", progress=0, stage="", options=json.dumps(options),
                    outputs="[]", error=None, created_at=timestamp, updated_at=timestamp,
                ))
        except Exception:
            shutil.rmtree(folder, ignore_errors=True)
            raise
        created.append(job_id)
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
    return jsonify(job_dict(row)) if row else (jsonify(error="Job not found"), 404)


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
            return jsonify(error="Job not found"), 404
        if row.status == "canceling":
            cancel_event_for(job_id).set()
            return jsonify(job_dict(row)), 202
        if row.status not in {"queued", "processing"}:
            return jsonify(error="Only an active job can be canceled"), 409
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
            return jsonify(error="Job not found"), 404
        if row.status not in TERMINAL_STATUSES:
            return jsonify(error="Wait for the job to finish before deleting it"), 409
        jobs_root = JOBS_DIR.resolve()
        folder = (jobs_root / job_id).resolve()
        if folder.parent != jobs_root:
            return jsonify(error="Invalid job path"), 400
        try:
            if folder.exists():
                shutil.rmtree(folder)
        except OSError:
            return jsonify(error="Could not delete the job files"), 500
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
        return jsonify(error="Job not found"), 404
    allowed = {item["name"] for item in json.loads(row.outputs)}
    if name not in allowed:
        return jsonify(error="Output not found"), 404
    return send_file(JOBS_DIR / job_id / name, as_attachment=True, download_name=name)


@app.get("/api/jobs/<job_id>/download")
@jwt_required()
def download_all(job_id: str):
    user = current_user_row()
    row = owned_job(job_id, user)
    if row is None:
        return jsonify(error="Job not found"), 404
    outputs = json.loads(row.outputs)
    if not outputs:
        return jsonify(error="No outputs are ready"), 404
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
    return jsonify(error=f"Upload exceeds the {MAX_UPLOAD_MB} MB limit"), 413


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", "8000")), debug=app.debug)
