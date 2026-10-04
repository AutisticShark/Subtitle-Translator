"""Regression tests for job lifecycle, downloads, startup, and database portability."""

import importlib.util
import io
import json
import re
import shutil
import ssl
import subprocess
import sys
import textwrap
import threading
import time
import zipfile
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from sqlalchemy import Text, create_engine, event, insert, select
from sqlalchemy.dialects import mysql
from sqlalchemy.exc import DBAPIError, OperationalError
from sqlalchemy.schema import CreateTable

import database
from database import (
    STARTUP_RECOVERY_ENV, create_database_engine, database_connect_options,
    initialize_database, jobs, metadata, normalize_database_url, retry_database_race,
)
from srt_translate import FatalTranslationError
from test_webapp import webapp

PROJECT_ROOT = Path(__file__).resolve().parents[1]
ADMIN = {"username": "admin", "password": "correct-horse-battery-staple"}
SOURCE = b"".join(
    f"{index}\n00:00:{index:02d},000 --> 00:00:{index:02d},500\nLine {index}\n\n".encode()
    for index in range(1, 13)
)


@pytest.fixture
def client():
    previous = dict(webapp.app.config)
    webapp.app.config.update(TESTING=True, DEBUG=True, JWT_COOKIE_CSRF_PROTECT=False)
    test_client = webapp.app.test_client()
    assert test_client.post("/api/auth/login", json=ADMIN).status_code == 200
    response = test_client.put("/api/settings", json={
        "user_job_limit": 0, "admin_job_limit": 0, "panel_job_limit": 0,
        **{key: 0 for key in webapp.CALENDAR_LIMIT_KEYS},
        "captcha_provider": "none", "batch_size": "1", "workers": "1",
    })
    assert response.status_code == 200, response.get_json()
    yield test_client
    test_client.put("/api/settings", json={"batch_size": "20", "workers": "4"})
    webapp.app.config.clear()
    webapp.app.config.update(previous)


@pytest.fixture
def make_job():
    created = []

    def make(job_id, *, status, outputs=(), files=()):
        folder = webapp.JOBS_DIR / job_id
        folder.mkdir(parents=True, exist_ok=True)
        for name in files:
            (folder / name).write_text(f"content of {name}", "utf-8")
        timestamp = webapp.now()
        with webapp.transaction(webapp.engine) as db:
            db.execute(insert(jobs).values(
                id=job_id, user_id=None, filename="movie.srt", stored_name="source.srt",
                status=status, progress=50, stage="Translating", options="{}",
                outputs=json.dumps(list(outputs)), created_at=timestamp,
                updated_at=timestamp,
            ))
        created.append(job_id)
        return folder

    yield make
    with webapp.transaction(webapp.engine) as db:
        db.execute(jobs.delete().where(jobs.c.id.in_(created)))
    for job_id in created:
        shutil.rmtree(webapp.JOBS_DIR / job_id, ignore_errors=True)


def job_row(job_id):
    with webapp.connection(webapp.engine) as db:
        return db.execute(select(jobs).where(jobs.c.id == job_id)).first()


def set_status(job_id, status, stage):
    """Write a status from a separate connection, as another worker process would."""
    with webapp.transaction(webapp.engine) as db:
        db.execute(jobs.update().where(jobs.c.id == job_id).values(status=status, stage=stage))


def wait_terminal(client, job_id, timeout=15):
    job = None
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        job = client.get(f"/api/jobs/{job_id}").get_json()
        if job["status"] in webapp.TERMINAL_STATUSES:
            return job
        time.sleep(0.02)
    raise AssertionError(f"job did not finish: {job}")


def submit(client, targets="ja"):
    response = client.post("/api/jobs", data={
        "provider": "echo", "target_languages": targets, "source_language": "English",
        "files": (io.BytesIO(SOURCE), "movie.srt"),
    }, content_type="multipart/form-data")
    assert response.status_code == 202, response.get_json()
    return response.get_json()["jobs"][0]


def zip_names(data):
    return sorted(zipfile.ZipFile(io.BytesIO(data)).namelist())


# --- Downloads ------------------------------------------------------------

def test_concurrent_zip_downloads_never_serve_a_partial_archive(client, make_job):
    outputs = [{"name": "movie.ja.srt", "language": "ja"},
               {"name": "movie.ko.srt", "language": "ko"}]
    make_job("zip-concurrent", status="completed", outputs=outputs,
             files=[item["name"] for item in outputs])
    original_write = zipfile.ZipFile.write
    inner = {}

    def download_completely():
        # Read the body inside the thread: send_file streams lazily, and the
        # race is about what is on disk while the first archive is written.
        response = client.get("/api/jobs/zip-concurrent/download")
        inner["result"] = (response.status_code, response.get_data(),
                           response.headers.get("Content-Disposition", ""))
        response.close()

    def write_then_race(self, *args, **kwargs):
        if not inner:
            inner["started"] = True
            # A second request arrives while the first archive is half written.
            racer = threading.Thread(target=download_completely)
            racer.start()
            racer.join(10)
        return original_write(self, *args, **kwargs)

    with patch.object(zipfile.ZipFile, "write", write_then_race):
        response = client.get("/api/jobs/zip-concurrent/download")
        first = (response.status_code, response.get_data(),
                 response.headers.get("Content-Disposition", ""))
        response.close()
    expected = ["movie.ja.srt", "movie.ko.srt"]
    for status, data, disposition in (first, inner["result"]):
        assert status == 200
        assert zip_names(data) == expected
        assert disposition.endswith("movie.translations.zip")
    assert not list((webapp.JOBS_DIR / "zip-concurrent").glob("*.partial"))


def test_zip_requested_mid_job_is_rebuilt_when_more_outputs_are_registered(client, make_job):
    first_two = [{"name": "movie.ja.srt", "language": "ja"},
                 {"name": "movie.ko.srt", "language": "ko"}]
    third = {"name": "movie.fr.srt", "language": "fr"}
    make_job("zip-partial", status="processing", outputs=first_two,
             files=["movie.ja.srt", "movie.ko.srt", "movie.fr.srt"])
    partial = client.get("/api/jobs/zip-partial/download")
    assert zip_names(partial.data) == ["movie.ja.srt", "movie.ko.srt"]
    partial.close()

    webapp.update_job("zip-partial", status="completed",
                      outputs=json.dumps([*first_two, third]))
    final = client.get("/api/jobs/zip-partial/download")
    assert zip_names(final.data) == ["movie.fr.srt", "movie.ja.srt", "movie.ko.srt"]
    final.close()


# --- Cancellation ---------------------------------------------------------

def test_canceling_a_queued_job_is_immediately_terminal_and_deletable(client, make_job):
    make_job("queued-cancel", status="queued")
    response = client.post("/api/jobs/queued-cancel/cancel")
    assert response.status_code == 202
    assert response.get_json()["status"] == "canceled"
    assert job_row("queued-cancel").stage == "Canceled"
    # A worker that dequeues it later must leave it alone.
    webapp.run_job("queued-cancel")
    assert job_row("queued-cancel").status == "canceled"
    assert client.post("/api/jobs/queued-cancel/cancel").status_code == 409
    assert client.delete("/api/jobs/queued-cancel").status_code == 200


def test_cancel_cannot_overwrite_a_completion_committed_by_another_process(client, make_job):
    make_job("cancel-race", status="processing")
    fired = []

    def other_worker_completes(_conn, _cursor, statement, _params, _context, _many):
        if statement.lstrip().upper().startswith("UPDATE JOBS") and not fired:
            fired.append(True)
            # Commits between the cancel request's lookup and its first write.
            other = create_engine(webapp.engine.url)
            try:
                with other.begin() as db:
                    db.execute(jobs.update().where(
                        jobs.c.id == "cancel-race", jobs.c.status == "processing",
                    ).values(status="completed", stage="Ready to download", progress=100))
            finally:
                other.dispose()

    event.listen(webapp.engine, "before_cursor_execute", other_worker_completes)
    try:
        response = client.post("/api/jobs/cancel-race/cancel")
    finally:
        event.remove(webapp.engine, "before_cursor_execute", other_worker_completes)
    assert fired
    assert response.status_code == 409
    row = job_row("cancel-race")
    assert (row.status, row.stage) == ("completed", "Ready to download")


def test_cancel_for_a_job_running_elsewhere_does_not_leak_an_event(client, make_job):
    make_job("remote-running", status="processing")
    assert client.post("/api/jobs/remote-running/cancel").status_code == 202
    assert client.post("/api/jobs/remote-running/cancel").status_code == 202
    assert "remote-running" not in webapp.cancel_events


def test_worker_stops_when_another_process_records_cancellation(client):
    calls = []

    def provider(texts, _source, target):
        calls.append(texts)
        if len(calls) == 2:
            job_id = calls_job[0]
            set_status(job_id, "canceling", "Canceling")  # no in-memory event
        time.sleep(0.05)  # real provider calls outlast the status poll interval
        return [f"[{target}] {text}" for text in texts]

    calls_job = []
    with patch.object(webapp, "CANCEL_POLL_SECONDS", 0.01), \
            patch.object(webapp, "provider_for", return_value=provider):
        original_run = webapp.run_job

        def run_and_record(job_id):
            calls_job.append(job_id)
            original_run(job_id)

        with patch.object(webapp, "run_job", side_effect=run_and_record):
            job_id = submit(client)
        job = wait_terminal(client, job_id)
    assert job["status"] == "canceled", job.get("error")
    assert job["stage"] == "Canceled"
    assert len(calls) < 12, "the worker kept calling the paid provider after cancellation"
    assert job_id not in webapp.cancel_events
    assert client.delete(f"/api/jobs/{job_id}").status_code == 200


def test_failure_after_remote_cancellation_finalizes_as_canceled(client):
    holder = []

    def provider(_texts, _source, _target):
        # Only the first call: the batch pool may start one more call before it
        # is abandoned, and a test write from it would overwrite the final state.
        if len(holder) == 1:
            holder.append("canceled-remotely")
            set_status(holder[0], "canceling", "Canceling")
        raise FatalTranslationError("provider rejected the request")

    original_run = webapp.run_job

    def run_and_record(job_id):
        holder.append(job_id)
        original_run(job_id)

    with patch.object(webapp, "CANCEL_POLL_SECONDS", 3600), \
            patch.object(webapp, "provider_for", return_value=provider), \
            patch.object(webapp, "run_job", side_effect=run_and_record):
        job_id = submit(client)
        job = wait_terminal(client, job_id)
    assert job["status"] == "canceled", job.get("error")
    assert job["error"] is None
    assert client.delete(f"/api/jobs/{job_id}").status_code == 200


def test_progress_never_overwrites_the_canceling_stage(client):
    holder, observed = [], []

    def provider(texts, _source, target):
        set_status(holder[0], "canceling", "Canceling")
        return [f"[{target}] {text}" for text in texts]

    original_run = webapp.run_job
    original_update = webapp.update_job

    def run_and_record(job_id):
        holder.append(job_id)
        original_run(job_id)

    def update_and_observe(job_id, **fields):
        original_update(job_id, **fields)
        row = job_row(job_id)
        observed.append((row.status, row.stage))

    # Disable status polling so the worker keeps reporting progress while the
    # job is "canceling", as a worker in another process would until it polls.
    with patch.object(webapp, "CANCEL_POLL_SECONDS", 3600), \
            patch.object(webapp, "provider_for", return_value=provider), \
            patch.object(webapp, "update_job", side_effect=update_and_observe), \
            patch.object(webapp, "run_job", side_effect=run_and_record):
        job_id = submit(client, targets="ja,ko")
        job = wait_terminal(client, job_id)
    assert job["status"] == "canceled", job.get("error")
    canceling = [stage for status, stage in observed if status == "canceling"]
    assert canceling, observed
    assert set(canceling) == {"Canceling"}, observed
    assert client.delete(f"/api/jobs/{job_id}").status_code == 200


# --- Startup recovery and concurrent initialization -------------------------

def _seed_jobs(engine):
    timestamp = "2026-01-01T00:00:00+00:00"
    with engine.begin() as db:
        for job_id, status in (("q", "queued"), ("p", "processing"), ("c", "canceling"),
                               ("done", "completed")):
            db.execute(insert(jobs).values(
                id=job_id, filename="a.srt", stored_name="source.srt", status=status,
                stage="Translating to Japanese (3/9 segments)", options="{}",
                created_at=timestamp, updated_at=timestamp,
            ))


def _statuses(engine):
    with engine.connect() as db:
        return {row.id: (row.status, row.stage, row.error)
                for row in db.execute(select(jobs))}


def test_worker_initialization_does_not_fail_jobs_owned_by_live_workers(tmp_path):
    engine = create_database_engine(tmp_path / "app.db")
    try:
        initialize_database(engine, {}, "2026-01-01T00:00:00+00:00")
        _seed_jobs(engine)
        initialize_database(engine, {"x": "1"}, "2026-01-02T00:00:00+00:00",
                            recover_jobs=False)
        assert {key: value[0] for key, value in _statuses(engine).items()} == {
            "q": "queued", "p": "processing", "c": "canceling", "done": "completed",
        }
        initialize_database(engine, {}, "2026-01-03T00:00:00+00:00")
        recovered = _statuses(engine)
        assert recovered["q"] == ("failed", "Translation failed",
                                  database.RECOVERED_JOB_ERROR)
        assert recovered["p"] == recovered["q"]
        assert recovered["c"] == ("canceled", "Canceled", None)
        assert recovered["done"][0] == "completed"
    finally:
        engine.dispose()


def test_gunicorn_master_recovers_once_and_marks_workers_to_skip(tmp_path, monkeypatch):
    monkeypatch.setenv("DATA_DIR", str(tmp_path / "data"))
    monkeypatch.delenv("DATABASE_URL", raising=False)
    # Recorded so the flag the hook sets is removed again after the test.
    monkeypatch.setenv(STARTUP_RECOVERY_ENV, "0")
    path = tmp_path / "data" / "app.db"
    path.parent.mkdir()
    engine = create_database_engine(path)
    try:
        initialize_database(engine, {}, "2026-01-01T00:00:00+00:00")
        _seed_jobs(engine)
    finally:
        engine.dispose()

    spec = importlib.util.spec_from_file_location("gunicorn_conf", PROJECT_ROOT / "gunicorn.conf.py")
    config = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(config)
    messages = []
    config.on_starting(SimpleNamespace(log=SimpleNamespace(info=messages.append)))

    assert database.startup_recovery_completed()
    assert (tmp_path / "data" / "jobs").is_dir()
    engine = create_database_engine(path)
    try:
        assert _statuses(engine)["p"][0] == "failed"
    finally:
        engine.dispose()
    dockerfile = (PROJECT_ROOT / "Dockerfile").read_text("utf-8")
    command = re.search(r'(?m)^CMD (\[.*\])\s*$', dockerfile).group(1)
    assert json.loads(command)[:3] == ["gunicorn", "--config", "gunicorn.conf.py"]
    for option in ('"--bind", "0.0.0.0:8000"', '"--threads", "8"', '"--timeout", "300"'):
        assert option in command


def test_webapp_skips_recovery_when_the_master_already_recovered():
    source = (PROJECT_ROOT / "webapp.py").read_text("utf-8")
    assert re.search(
        r"(?m)^initialize_database\(engine, DEFAULTS, now\(\), "
        r"recover_jobs=not startup_recovery_completed\(\)\)$", source,
    )


def test_concurrent_workers_initialize_a_fresh_database(tmp_path):
    database_path = tmp_path / "fresh.db"
    go = tmp_path / "go"
    script = textwrap.dedent(f"""
        import sys, time
        from pathlib import Path
        sys.path.insert(0, {str(PROJECT_ROOT)!r})
        from database import create_database_engine, initialize_database
        go = Path({str(go)!r})
        while not go.exists():
            time.sleep(0.005)
        engine = create_database_engine(Path({str(database_path)!r}))
        initialize_database(engine, {{"a": "1", "b": "2", "c": "3"}},
                            "2026-01-01T00:00:00+00:00")
        engine.dispose()
    """)
    processes = [
        subprocess.Popen([sys.executable, "-c", script], stderr=subprocess.PIPE, text=True)
        for _ in range(6)
    ]
    time.sleep(1.5)  # let every interpreter finish importing SQLAlchemy
    go.write_text("1")
    errors = []
    for process in processes:
        _, stderr = process.communicate(timeout=120)
        if process.returncode:
            errors.append(stderr)
    assert not errors, errors[0]
    engine = create_database_engine(database_path)
    try:
        with engine.connect() as db:
            names = set(db.execute(select(database.settings.c.name)).scalars())
            revisions = set(db.execute(select(database.cache_revisions.c.name)).scalars())
        assert names == {"a", "b", "c"}
        assert revisions == {"settings", "jobs"}
    finally:
        engine.dispose()


def test_startup_steps_retry_transient_races():
    attempts = []

    def flaky():
        attempts.append(1)
        if len(attempts) < 3:
            raise OperationalError("INSERT", {}, Exception("database is locked"))
        return "ok"

    assert retry_database_race(flaky) == "ok"
    assert len(attempts) == 3
    permanent = []

    def always_fails():
        permanent.append(1)
        raise OperationalError("CREATE", {}, Exception("permanent"))

    with pytest.raises(OperationalError):
        retry_database_race(always_fails, attempts=2)
    assert len(permanent) == 2


# --- Schema portability ---------------------------------------------------

def test_text_columns_have_no_literal_server_default_on_mysql():
    dialect = mysql.dialect()
    for table in metadata.sorted_tables:
        ddl = str(CreateTable(table).compile(dialect=dialect))
        assert not re.search(r"\b(?:TINY|MEDIUM|LONG)?(?:TEXT|BLOB)\b[^,\n]*DEFAULT '",
                             ddl, re.IGNORECASE), ddl
        assert not re.search(r"\bJSON\b[^,\n]*DEFAULT '", ddl, re.IGNORECASE), ddl
        for column in table.columns:
            if isinstance(column.type, Text) and column.server_default is not None:
                clause = str(column.server_default.arg)
                assert clause.startswith("(") and clause.endswith(")"), (table.name, clause)


def test_text_expression_defaults_apply_to_raw_inserts(tmp_path):
    engine = create_database_engine(tmp_path / "defaults.db")
    try:
        initialize_database(engine, {}, "2026-01-01T00:00:00+00:00")
        with engine.begin() as db:
            db.exec_driver_sql(
                "INSERT INTO jobs (id, filename, stored_name, status, options, created_at, "
                "updated_at) VALUES ('raw', 'a.srt', 's.srt', 'queued', '{}', 't', 't')"
            )
        with engine.connect() as db:
            row = db.execute(select(jobs.c.stage, jobs.c.outputs)).one()
        assert tuple(row) == ("", "[]")
    finally:
        engine.dispose()


# --- TLS URL parameters ---------------------------------------------------

def _options(raw):
    return database_connect_options(normalize_database_url(raw, Path("unused.db")))


@pytest.mark.parametrize("raw, verify, hostname", [
    ("postgres://u:p@db.example/app?sslmode=require", ssl.CERT_NONE, False),
    ("postgresql://u:p@db.example/app?sslmode=verify-ca", ssl.CERT_REQUIRED, False),
    ("postgresql://u:p@db.example/app?sslmode=verify-full", ssl.CERT_REQUIRED, True),
    ("postgresql+pg8000://u:p@db.example/app?sslmode=VERIFY-FULL",
     ssl.CERT_REQUIRED, True),
])
def test_postgres_sslmode_becomes_a_pg8000_ssl_context(raw, verify, hostname):
    url, connect_args = _options(raw)
    assert "sslmode" not in url.query
    context = connect_args["ssl_context"]
    assert isinstance(context, ssl.SSLContext)
    assert context.verify_mode == verify
    assert context.check_hostname is hostname


@pytest.mark.parametrize("raw, verify, hostname", [
    ("mysql://u:p@db.example/app?charset=utf8mb4&ssl-mode=REQUIRED", ssl.CERT_NONE, False),
    ("mariadb://u:p@db.example/app?ssl_mode=verify_ca", ssl.CERT_REQUIRED, False),
    ("mysql://u:p@db.example/app?ssl-mode=VERIFY_IDENTITY", ssl.CERT_REQUIRED, True),
])
def test_mysql_ssl_mode_becomes_a_pymysql_ssl_context(raw, verify, hostname):
    url, connect_args = _options(raw)
    assert not {"ssl-mode", "ssl_mode"} & set(url.query)
    context = connect_args["ssl"]
    assert context.verify_mode == verify
    assert context.check_hostname is hostname
    if "charset" in raw:
        assert url.query["charset"] == "utf8mb4"


def test_disabled_tls_and_unsupported_modes():
    url, connect_args = _options("postgres://u:p@db.example/app?sslmode=disable")
    assert connect_args == {} and "sslmode" not in url.query
    url, connect_args = _options("mysql://u:p@db.example/app?ssl-mode=DISABLED")
    assert connect_args == {} and "ssl-mode" not in url.query
    for raw in ("postgres://u:p@db.example/app?sslmode=prefer",
                "postgres://u:p@db.example/app?sslmode=bogus",
                "mysql://u:p@db.example/app?ssl-mode=PREFERRED"):
        with pytest.raises(RuntimeError, match="not supported"):
            _options(raw)
    # Other drivers understand their own parameters; leave them untouched.
    url, connect_args = _options("postgresql+psycopg://u:p@db.example/app?sslmode=require")
    assert url.query["sslmode"] == "require" and connect_args == {}


@pytest.mark.parametrize("raw", [
    "postgres://u:p@127.0.0.1:1/app?sslmode=require",
    "mysql://u:p@127.0.0.1:1/app?ssl-mode=REQUIRED",
])
def test_drivers_accept_the_translated_tls_arguments(raw, monkeypatch):
    monkeypatch.setenv("DATABASE_URL", raw)
    engine = create_database_engine(Path("unused.db"))
    try:
        # Port 1 refuses the connection; the old code failed earlier with a
        # TypeError for the unknown sslmode/ssl-mode keyword.
        with pytest.raises(DBAPIError):
            with engine.connect():
                pass
    finally:
        engine.dispose()
