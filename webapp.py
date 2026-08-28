"""Flask application for browser-based subtitle translation."""

from __future__ import annotations

import hmac
import json
import os
import shutil
import sqlite3
import threading
import uuid
import zipfile
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from flask import Flask, Response, jsonify, render_template, request, send_file
from werkzeug.utils import secure_filename

from srt_translate import (
    LANGS,
    FatalTranslationError,
    Throttle,
    TranslationCanceled,
    make_anthropic,
    make_deepl,
    make_echo,
    make_google,
    make_openai,
    rebuild_cues,
    segment_cue,
    translate_segments,
)
from subtitle_formats import (
    SUPPORTED_EXTENSIONS,
    SubtitleFormatError,
    load_subtitle,
    translated_filename,
)


BASE_DIR = Path(__file__).resolve().parent
DATA_DIR = Path(os.environ.get("DATA_DIR", BASE_DIR / "data")).resolve()
JOBS_DIR = DATA_DIR / "jobs"
DB_PATH = DATA_DIR / "app.db"
MAX_UPLOAD_MB = int(os.environ.get("MAX_UPLOAD_MB", "200"))

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
PROVIDERS = {"anthropic", "openai", "deepl", "google", "echo"}

app = Flask(__name__, template_folder="templates", static_folder="static")
app.config.update(
    MAX_CONTENT_LENGTH=MAX_UPLOAD_MB * 1024 * 1024,
    SEND_FILE_MAX_AGE_DEFAULT=0,
)
executor = ThreadPoolExecutor(max_workers=max(1, int(os.environ.get("JOB_WORKERS", "2"))))
db_lock = threading.RLock()
cancel_events_lock = threading.Lock()
cancel_events: dict[str, threading.Event] = {}


def now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def connect_db() -> sqlite3.Connection:
    connection = sqlite3.connect(DB_PATH, timeout=30)
    connection.row_factory = sqlite3.Row
    connection.execute("PRAGMA journal_mode=WAL")
    return connection


def init_storage() -> None:
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    JOBS_DIR.mkdir(parents=True, exist_ok=True)
    with connect_db() as db:
        db.executescript(
            """
            CREATE TABLE IF NOT EXISTS settings (
                name TEXT PRIMARY KEY,
                value TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS jobs (
                id TEXT PRIMARY KEY,
                filename TEXT NOT NULL,
                stored_name TEXT NOT NULL,
                status TEXT NOT NULL,
                progress INTEGER NOT NULL DEFAULT 0,
                stage TEXT NOT NULL DEFAULT '',
                options TEXT NOT NULL,
                outputs TEXT NOT NULL DEFAULT '[]',
                error TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );
            """
        )
        for key, value in DEFAULTS.items():
            db.execute(
                "INSERT OR IGNORE INTO settings(name, value, updated_at) VALUES (?, ?, ?)",
                (key, value, now()),
            )
        db.execute(
            "UPDATE jobs SET status='failed', error='The server restarted before this job finished', "
            "updated_at=? WHERE status IN ('queued', 'processing')", (now(),)
        )
        db.execute(
            "UPDATE jobs SET status='canceled', stage='Canceled', error=NULL, updated_at=? "
            "WHERE status='canceling'", (now(),)
        )


init_storage()


@app.before_request
def authenticate():
    if request.path == "/healthz":
        return None
    password = os.environ.get("APP_PASSWORD", "")
    if password:
        auth = request.authorization
        if not auth or not hmac.compare_digest(auth.password or "", password):
            return Response("Authentication required", 401,
                            {"WWW-Authenticate": 'Basic realm="Subtitle Translator"'})
    return None


def read_settings(include_secrets: bool = False) -> dict[str, Any]:
    with db_lock, connect_db() as db:
        values = {row["name"]: row["value"] for row in db.execute("SELECT name, value FROM settings")}
    result: dict[str, Any] = {key: values.get(key, default) for key, default in DEFAULTS.items()}
    result["configured"] = {
        "anthropic": bool(values.get("anthropic_api_key") or os.environ.get("ANTHROPIC_API_KEY")),
        "openai": bool(values.get("openai_api_key") or os.environ.get("OPENAI_API_KEY")),
        "deepl": bool(values.get("deepl_api_key") or os.environ.get("DEEPL_API_KEY")),
        "google": bool(values.get("google_api_key") or os.environ.get("GOOGLE_API_KEY")),
        "echo": True,
    }
    if include_secrets:
        for key in SECRET_KEYS:
            result[key] = values.get(key, "") or os.environ.get(key.upper(), "")
    return result


def job_dict(row: sqlite3.Row) -> dict[str, Any]:
    result = dict(row)
    result["options"] = json.loads(result["options"])
    result["outputs"] = json.loads(result["outputs"])
    result.pop("stored_name", None)
    return result


def update_job(job_id: str, **fields: Any) -> None:
    fields["updated_at"] = now()
    columns = ", ".join(f"{key}=?" for key in fields)
    with db_lock, connect_db() as db:
        db.execute(f"UPDATE jobs SET {columns} WHERE id=?", (*fields.values(), job_id))


def update_job_if_status(job_id: str, statuses: set[str], **fields: Any) -> bool:
    fields["updated_at"] = now()
    columns = ", ".join(f"{key}=?" for key in fields)
    allowed = tuple(statuses)
    placeholders = ", ".join("?" for _ in allowed)
    with db_lock, connect_db() as db:
        cursor = db.execute(
            f"UPDATE jobs SET {columns} WHERE id=? AND status IN ({placeholders})",
            (*fields.values(), job_id, *allowed),
        )
        return cursor.rowcount == 1


def cancel_event_for(job_id: str) -> threading.Event:
    with cancel_events_lock:
        return cancel_events.setdefault(job_id, threading.Event())


def provider_for(name: str, settings: dict[str, Any], throttle: Throttle, model: str | None):
    if name == "echo":
        return make_echo()
    if name == "anthropic":
        key = settings.get("anthropic_api_key")
        if not key:
            raise FatalTranslationError("Anthropic API key is not configured")
        return make_anthropic(model or settings["anthropic_model"], key, throttle)
    if name == "openai":
        key = settings.get("openai_api_key")
        if not key:
            raise FatalTranslationError("OpenAI-compatible API key is not configured")
        return make_openai(model or settings["openai_model"], key, throttle,
                           settings["openai_base_url"])
    if name == "deepl":
        key = settings.get("deepl_api_key")
        if not key:
            raise FatalTranslationError("DeepL API key is not configured")
        return make_deepl(key, throttle)
    if name == "google":
        key = settings.get("google_api_key")
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
        with connect_db() as db:
            row = db.execute("SELECT * FROM jobs WHERE id=?", (job_id,)).fetchone()
        if row is None:
            return
        if row["status"] == "canceling":
            cancel_event.set()
        check_canceled()
        options = json.loads(row["options"])
        folder = JOBS_DIR / job_id
        source = folder / row["stored_name"]
        if not update_job_if_status(
            job_id, {"queued"}, status="processing", progress=2,
            stage="Reading subtitle",
        ):
            check_canceled()
            return

        document = load_subtitle(source, options.get("encoding", "utf-8"))
        segments = []
        for cue_i, cue in enumerate(document.cues):
            segments.extend(segment_cue(cue, cue_i))
        check_canceled()
        update_job(job_id, progress=5, stage=f"Parsed {len(document.cues)} cues")

        settings = read_settings(include_secrets=True)
        throttle = Throttle(float(options["rpm"]), cancel_event.is_set)
        provider = provider_for(options["provider"], settings, throttle, options.get("model"))
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
                    job_id,
                    progress=progress,
                    stage=(f"Translating to {LANGS[target_language]['name']} "
                           f"({done}/{total} segments)"),
                )

            translated = translate_segments(
                segments, provider, language, options["source_language"],
                int(options["batch_size"]), 4, 10, throttle, cache,
                int(options["workers"]), True, report_progress,
                cancel_event.is_set,
            )
            check_canceled()
            cues = rebuild_cues(
                document.cues, segments, translated, language,
                float(options["width"]), int(options["max_lines"]),
            )
            output_name = translated_filename(row["filename"], LANGS[language]["suffix"])
            output_path = folder / output_name
            output_path.write_bytes(document.clone_with_cues(cues).to_bytes())
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


@app.get("/")
def index():
    return render_template("index.html", languages=LANGS, max_upload_mb=MAX_UPLOAD_MB)


@app.get("/healthz")
def health():
    return jsonify(status="ok")


@app.get("/api/settings")
def get_settings():
    return jsonify(read_settings())


@app.put("/api/settings")
def save_settings():
    payload = request.get_json(silent=True) or {}
    unknown = set(payload) - ALL_SETTING_KEYS
    if unknown:
        return jsonify(error=f"Unknown settings: {', '.join(sorted(unknown))}"), 400
    if payload.get("default_provider") and payload["default_provider"] not in PROVIDERS:
        return jsonify(error="Invalid default provider"), 400
    numeric = {"batch_size": (1, 100), "workers": (1, 16), "rpm": (0, 10000),
               "width": (4, 80), "max_lines": (1, 5)}
    try:
        for key, (minimum, maximum) in numeric.items():
            if key in payload and not minimum <= float(payload[key]) <= maximum:
                raise ValueError(f"{key} must be between {minimum} and {maximum}")
    except (TypeError, ValueError) as exc:
        return jsonify(error=str(exc)), 400

    with db_lock, connect_db() as db:
        for key, value in payload.items():
            # Blank secret fields deliberately leave the existing value intact.
            if key in SECRET_KEYS and not str(value).strip():
                continue
            db.execute(
                "INSERT INTO settings(name, value, updated_at) VALUES (?, ?, ?) "
                "ON CONFLICT(name) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at",
                (key, str(value).strip(), now()),
            )
    return jsonify(read_settings())


@app.delete("/api/settings/keys/<provider>")
def delete_key(provider: str):
    key = f"{provider}_api_key"
    if key not in SECRET_KEYS:
        return jsonify(error="Unknown provider"), 404
    with db_lock, connect_db() as db:
        db.execute("DELETE FROM settings WHERE name=?", (key,))
    return jsonify(read_settings())


@app.post("/api/jobs")
def create_jobs():
    files = request.files.getlist("files")
    if not files or all(not item.filename for item in files):
        return jsonify(error="Select at least one subtitle file"), 400
    validated_files = []
    for upload in files:
        original = secure_filename(upload.filename or "")
        if not original or Path(original).suffix.lower() not in SUPPORTED_EXTENSIONS:
            return jsonify(error=f"Unsupported file: {upload.filename}"), 400
        validated_files.append((upload, original))
    settings = read_settings()
    provider = request.form.get("provider", settings["default_provider"])
    if provider not in PROVIDERS:
        return jsonify(error="Invalid provider"), 400
    targets = [value.strip() for value in request.form.get(
        "target_languages", settings["target_languages"]).split(",") if value.strip()]
    if not targets or any(language not in LANGS for language in targets):
        return jsonify(error="Choose one or more valid target languages"), 400
    options = {
        "provider": provider,
        "model": request.form.get("model", "").strip() or None,
        "source_language": request.form.get("source_language", settings["source_language"]).strip(),
        "target_languages": targets,
        "encoding": request.form.get("encoding", "utf-8").strip(),
        "batch_size": settings["batch_size"],
        "workers": settings["workers"],
        "rpm": settings["rpm"],
        "width": settings["width"],
        "max_lines": settings["max_lines"],
    }
    created = []
    for upload, original in validated_files:
        job_id = uuid.uuid4().hex
        folder = JOBS_DIR / job_id
        folder.mkdir(parents=True)
        stored = "source" + Path(original).suffix.lower()
        upload.save(folder / stored)
        timestamp = now()
        with db_lock, connect_db() as db:
            db.execute(
                "INSERT INTO jobs(id, filename, stored_name, status, options, created_at, updated_at) "
                "VALUES (?, ?, ?, 'queued', ?, ?, ?)",
                (job_id, original, stored, json.dumps(options), timestamp, timestamp),
            )
        created.append(job_id)
        executor.submit(run_job, job_id)
    return jsonify(jobs=created), 202


@app.get("/api/jobs")
def list_jobs():
    limit = min(max(request.args.get("limit", 50, type=int), 1), 200)
    with connect_db() as db:
        rows = db.execute("SELECT * FROM jobs ORDER BY created_at DESC LIMIT ?", (limit,)).fetchall()
    return jsonify(jobs=[job_dict(row) for row in rows])


@app.get("/api/jobs/<job_id>")
def get_job(job_id: str):
    with connect_db() as db:
        row = db.execute("SELECT * FROM jobs WHERE id=?", (job_id,)).fetchone()
    if row is None:
        return jsonify(error="Job not found"), 404
    return jsonify(job_dict(row))


@app.post("/api/jobs/<job_id>/cancel")
def cancel_job(job_id: str):
    with db_lock, connect_db() as db:
        row = db.execute("SELECT * FROM jobs WHERE id=?", (job_id,)).fetchone()
        if row is None:
            return jsonify(error="Job not found"), 404
        if row["status"] == "canceling":
            cancel_event_for(job_id).set()
            return jsonify(job_dict(row)), 202
        if row["status"] == "queued":
            cancel_event_for(job_id).set()
            db.execute(
                "UPDATE jobs SET status='canceled', stage='Canceled', error=NULL, "
                "updated_at=? WHERE id=?",
                (now(), job_id),
            )
            row = db.execute("SELECT * FROM jobs WHERE id=?", (job_id,)).fetchone()
            return jsonify(job_dict(row))
        if row["status"] != "processing":
            return jsonify(error="Only an active job can be canceled"), 409

        db.execute(
            "UPDATE jobs SET status='canceling', stage='Canceling', updated_at=? WHERE id=?",
            (now(), job_id),
        )
        cancel_event_for(job_id).set()
        row = db.execute("SELECT * FROM jobs WHERE id=?", (job_id,)).fetchone()
    return jsonify(job_dict(row)), 202


@app.delete("/api/jobs/<job_id>")
def delete_job(job_id: str):
    with db_lock, connect_db() as db:
        row = db.execute("SELECT status FROM jobs WHERE id=?", (job_id,)).fetchone()
        if row is None:
            return jsonify(error="Job not found"), 404
        if row["status"] not in {"completed", "failed", "canceled"}:
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
        db.execute("DELETE FROM jobs WHERE id=?", (job_id,))
        with cancel_events_lock:
            cancel_events.pop(job_id, None)
    return jsonify(deleted=job_id)


@app.get("/api/jobs/<job_id>/download/<path:name>")
def download_output(job_id: str, name: str):
    with connect_db() as db:
        row = db.execute("SELECT outputs FROM jobs WHERE id=?", (job_id,)).fetchone()
    if row is None:
        return jsonify(error="Job not found"), 404
    allowed = {item["name"] for item in json.loads(row["outputs"])}
    if name not in allowed:
        return jsonify(error="Output not found"), 404
    path = JOBS_DIR / job_id / name
    return send_file(path, as_attachment=True, download_name=name)


@app.get("/api/jobs/<job_id>/download")
def download_all(job_id: str):
    with connect_db() as db:
        row = db.execute("SELECT filename, outputs FROM jobs WHERE id=?", (job_id,)).fetchone()
    if row is None:
        return jsonify(error="Job not found"), 404
    outputs = json.loads(row["outputs"])
    if not outputs:
        return jsonify(error="No outputs are ready"), 404
    if len(outputs) == 1:
        return download_output(job_id, outputs[0]["name"])
    archive = JOBS_DIR / job_id / (Path(row["filename"]).stem + ".translations.zip")
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
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", "8000")), debug=False)
