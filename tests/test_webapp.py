import io
import json
import os
import shutil
import tempfile
import threading
import time
import unittest
from unittest.mock import patch


_data_directory = tempfile.TemporaryDirectory()
os.environ["DATA_DIR"] = _data_directory.name
os.environ["JOB_WORKERS"] = "1"

import webapp  # noqa: E402  (environment must be configured before import)


class WebApplicationTests(unittest.TestCase):
    def setUp(self):
        webapp.app.config.update(TESTING=True, DEBUG=True)
        self.client = webapp.app.test_client()

    def insert_job(self, job_id, *, status="completed", outputs=None, filename="sample.srt"):
        outputs = outputs or []
        folder = webapp.JOBS_DIR / job_id
        folder.mkdir(parents=True, exist_ok=True)
        timestamp = webapp.now()
        with webapp.connect_db() as db:
            db.execute(
                "INSERT INTO jobs(id, filename, stored_name, status, options, outputs, "
                "created_at, updated_at) VALUES (?, ?, 'source.srt', ?, '{}', ?, ?, ?)",
                (job_id, filename, status, json.dumps(outputs), timestamp, timestamp),
            )

        def cleanup():
            with webapp.connect_db() as db:
                db.execute("DELETE FROM jobs WHERE id=?", (job_id,))
            if folder.exists():
                shutil.rmtree(folder)

        self.addCleanup(cleanup)
        return folder

    def test_health_and_secret_masking(self):
        self.assertEqual(self.client.get("/healthz").status_code, 200)
        response = self.client.put("/api/settings", json={
            "anthropic_api_key": "anthropic-secret",
            "google_api_key": "google-secret",
        })
        self.assertEqual(response.status_code, 200)
        settings = self.client.get("/api/settings").get_json()
        self.assertTrue(settings["configured"]["anthropic"])
        self.assertTrue(settings["configured"]["google"])
        self.assertNotIn("anthropic_api_key", settings)
        self.assertNotIn("google_api_key", settings)

    def test_health_bypasses_basic_auth_but_api_does_not(self):
        with patch.dict(os.environ, {"APP_PASSWORD": "secret"}):
            self.assertEqual(self.client.get("/healthz").status_code, 200)
            unauthorized = self.client.get("/api/settings")
            authorized = self.client.get(
                "/api/settings",
                headers={"Authorization": "Basic dXNlcjpzZWNyZXQ="},
            )

        self.assertEqual(unauthorized.status_code, 401)
        self.assertIn("Basic", unauthorized.headers["WWW-Authenticate"])
        self.assertEqual(authorized.status_code, 200)

    def test_echo_is_available_when_debug_is_enabled(self):
        settings = self.client.get("/api/settings").get_json()

        self.assertEqual(settings["providers"]["echo"], "Echo (offline test)")
        self.assertTrue(settings["configured"]["echo"])
        provider = webapp.provider_for("echo", {}, webapp.Throttle(), None)
        self.assertEqual(provider(["Hello"], "English", "es"), ["[es] Hello"])

    def test_echo_is_hidden_and_rejected_when_debug_is_disabled(self):
        webapp.app.config["DEBUG"] = False
        settings = self.client.get("/api/settings").get_json()

        self.assertNotIn("echo", settings["providers"])
        self.assertNotIn("echo", settings["configured"])
        with self.assertRaisesRegex(webapp.FatalTranslationError, "only in debug mode"):
            webapp.provider_for("echo", {}, webapp.Throttle(), None)

        save_default = self.client.put("/api/settings", json={"default_provider": "echo"})
        self.assertEqual(save_default.status_code, 400)

        with patch.object(webapp.executor, "submit") as submit:
            create = self.client.post("/api/jobs", data={
                "provider": "echo",
                "target_languages": "es",
                "files": (io.BytesIO(
                    b"1\n00:00:01,000 --> 00:00:02,000\nHello\n\n"
                ), "sample.srt"),
            }, content_type="multipart/form-data")

        self.assertEqual(create.status_code, 400)
        self.assertEqual(create.get_json()["error"], "Invalid provider")
        submit.assert_not_called()

    def test_hidden_echo_default_falls_back_without_overwriting_saved_setting(self):
        with webapp.connect_db() as db:
            db.execute(
                "UPDATE settings SET value='echo', updated_at=? WHERE name='default_provider'",
                (webapp.now(),),
            )
        self.addCleanup(self._set_default_provider, "anthropic")

        webapp.app.config["DEBUG"] = False
        self.assertEqual(
            self.client.get("/api/settings").get_json()["default_provider"],
            "anthropic",
        )
        webapp.app.config["DEBUG"] = True
        self.assertEqual(
            self.client.get("/api/settings").get_json()["default_provider"],
            "echo",
        )

    def test_settings_reject_unknown_invalid_provider_and_out_of_range_values(self):
        cases = [
            ({"surprise": "value"}, "Unknown settings"),
            ({"default_provider": "missing"}, "Invalid default provider"),
            ({"batch_size": 0}, "batch_size must be between 1 and 100"),
            ({"workers": "many"}, "could not convert string to float"),
        ]
        for payload, error in cases:
            with self.subTest(payload=payload):
                response = self.client.put("/api/settings", json=payload)
                self.assertEqual(response.status_code, 400)
                self.assertIn(error, response.get_json()["error"])

    def test_blank_secret_preserves_saved_value_and_delete_key_removes_it(self):
        self.client.put("/api/settings", json={"openai_api_key": "saved-secret"})

        response = self.client.put("/api/settings", json={"openai_api_key": "  "})
        self.assertTrue(response.get_json()["configured"]["openai"])
        with patch.dict(os.environ, {"OPENAI_API_KEY": ""}):
            deleted = self.client.delete("/api/settings/keys/openai")

        self.assertEqual(deleted.status_code, 200)
        self.assertFalse(deleted.get_json()["configured"]["openai"])
        self.assertEqual(self.client.delete("/api/settings/keys/unknown").status_code, 404)

    def test_google_provider_uses_the_saved_key(self):
        throttle = webapp.Throttle()
        expected_provider = object()
        with patch.object(webapp, "make_google", return_value=expected_provider) as factory:
            provider = webapp.provider_for(
                "google", {"google_api_key": "google-secret"}, throttle, None
            )

        self.assertIs(provider, expected_provider)
        factory.assert_called_once_with("google-secret", throttle)

    def test_provider_requires_credentials_and_rejects_unknown_name(self):
        with self.assertRaisesRegex(webapp.FatalTranslationError, "Anthropic API key"):
            webapp.provider_for("anthropic", {}, webapp.Throttle(), None)
        with self.assertRaisesRegex(webapp.FatalTranslationError, "Unknown provider"):
            webapp.provider_for("not-real", {}, webapp.Throttle(), None)

    def test_job_creation_validates_files_provider_and_targets_before_queueing(self):
        cases = [
            ({}, "Select at least one"),
            ({"files": (io.BytesIO(b"text"), "notes.txt")}, "Unsupported file"),
            ({
                "provider": "not-real",
                "files": (io.BytesIO(b"text"), "sample.srt"),
            }, "Invalid provider"),
            ({
                "provider": "echo",
                "target_languages": "xx",
                "files": (io.BytesIO(b"text"), "sample.srt"),
            }, "valid target languages"),
        ]

        with patch.object(webapp.executor, "submit") as submit:
            for data, error in cases:
                with self.subTest(error=error):
                    response = self.client.post(
                        "/api/jobs", data=data, content_type="multipart/form-data",
                    )
                    self.assertEqual(response.status_code, 400)
                    self.assertIn(error, response.get_json()["error"])

        submit.assert_not_called()

    def test_job_creation_sanitizes_filename_and_isolates_source(self):
        source = b"1\n00:00:01,000 --> 00:00:02,000\nHello\n\n"
        with patch.object(webapp.executor, "submit") as submit:
            response = self.client.post("/api/jobs", data={
                "provider": "echo",
                "target_languages": "es",
                "files": (io.BytesIO(source), "../../Episode.EN.SRT"),
            }, content_type="multipart/form-data")

        self.assertEqual(response.status_code, 202)
        job_id = response.get_json()["jobs"][0]
        self.addCleanup(lambda: shutil.rmtree(webapp.JOBS_DIR / job_id, ignore_errors=True))
        self.addCleanup(self._delete_job_record, job_id)
        submit.assert_called_once_with(webapp.run_job, job_id)
        with webapp.connect_db() as db:
            row = db.execute("SELECT filename, stored_name FROM jobs WHERE id=?", (job_id,)).fetchone()
        self.assertEqual(row["filename"], "Episode.EN.SRT")
        self.assertEqual(row["stored_name"], "source.srt")
        self.assertEqual((webapp.JOBS_DIR / job_id / "source.srt").read_bytes(), source)
        self.assertNotIn("stored_name", self.client.get(f"/api/jobs/{job_id}").get_json())

    @staticmethod
    def _delete_job_record(job_id):
        with webapp.connect_db() as db:
            db.execute("DELETE FROM jobs WHERE id=?", (job_id,))

    @staticmethod
    def _set_default_provider(provider):
        with webapp.connect_db() as db:
            db.execute(
                "UPDATE settings SET value=?, updated_at=? WHERE name='default_provider'",
                (provider, webapp.now()),
            )

    def test_active_job_cannot_be_deleted(self):
        job_id = "active-delete-test"
        timestamp = webapp.now()
        with webapp.connect_db() as db:
            db.execute(
                "INSERT INTO jobs(id, filename, stored_name, status, options, created_at, updated_at) "
                "VALUES (?, 'active.srt', 'source.srt', 'processing', '{}', ?, ?)",
                (job_id, timestamp, timestamp),
            )
        try:
            response = self.client.delete(f"/api/jobs/{job_id}")
            self.assertEqual(response.status_code, 409)
            self.assertEqual(self.client.get(f"/api/jobs/{job_id}").status_code, 200)
        finally:
            with webapp.connect_db() as db:
                db.execute("DELETE FROM jobs WHERE id=?", (job_id,))

    def test_unknown_and_terminal_jobs_cannot_be_canceled(self):
        self.assertEqual(self.client.post("/api/jobs/missing/cancel").status_code, 404)
        self.insert_job("completed-cancel-test")

        response = self.client.post("/api/jobs/completed-cancel-test/cancel")

        self.assertEqual(response.status_code, 409)

    def test_processing_job_can_be_canceled_and_deleted(self):
        provider_entered = threading.Event()
        release_provider = threading.Event()
        provider_exited = threading.Event()

        def blocking_provider(texts, _source, target):
            provider_entered.set()
            try:
                release_provider.wait(2)
                return [f"[{target}] {text}" for text in texts]
            finally:
                provider_exited.set()

        with patch.object(webapp, "provider_for", return_value=blocking_provider):
            response = self.client.post("/api/jobs", data={
                "provider": "echo",
                "target_languages": "zh-TW",
                "source_language": "English",
                "files": (io.BytesIO(
                    b"1\n00:00:01,000 --> 00:00:02,000\nHello there\n\n"
                ), "cancel.srt"),
            }, content_type="multipart/form-data")
            self.assertEqual(response.status_code, 202)
            job_id = response.get_json()["jobs"][0]
            self.assertTrue(provider_entered.wait(1))

            cancel = self.client.post(f"/api/jobs/{job_id}/cancel")
            self.assertEqual(cancel.status_code, 202)
            self.assertEqual(cancel.get_json()["status"], "canceling")

            job = None
            for _ in range(100):
                job = self.client.get(f"/api/jobs/{job_id}").get_json()
                if job["status"] in {"canceled", "completed", "failed"}:
                    break
                time.sleep(0.02)

            self.assertEqual(job["status"], "canceled", job.get("error"))
            self.assertFalse(provider_exited.is_set())
            deleted = self.client.delete(f"/api/jobs/{job_id}")
            self.assertEqual(deleted.status_code, 200)

        release_provider.set()
        self.assertTrue(provider_exited.wait(1))

    def test_queued_job_is_canceled_immediately(self):
        job_id = "queued-cancel-test"
        timestamp = webapp.now()
        with webapp.connect_db() as db:
            db.execute(
                "INSERT INTO jobs(id, filename, stored_name, status, options, created_at, updated_at) "
                "VALUES (?, 'queued.srt', 'source.srt', 'queued', '{}', ?, ?)",
                (job_id, timestamp, timestamp),
            )

        response = self.client.post(f"/api/jobs/{job_id}/cancel")
        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.get_json()["status"], "canceled")
        self.assertEqual(self.client.delete(f"/api/jobs/{job_id}").status_code, 200)

    def test_echo_job_end_to_end(self):
        self.client.put("/api/settings", json={"batch_size": "1", "workers": "2"})
        progress_updates = []
        original_update_job = webapp.update_job

        def capture_progress(job_id, **fields):
            if "progress" in fields:
                progress_updates.append(fields["progress"])
            original_update_job(job_id, **fields)

        source = b"".join(
            f"{index}\n00:00:0{index},000 --> 00:00:0{index + 1},000\nLine {index}\n\n".encode()
            for index in range(1, 5)
        )
        with patch.object(webapp, "update_job", side_effect=capture_progress):
            response = self.client.post("/api/jobs", data={
                "provider": "echo",
                "target_languages": "zh-TW,ja",
                "source_language": "English",
                "files": (io.BytesIO(source), "sample.srt"),
            }, content_type="multipart/form-data")
            self.assertEqual(response.status_code, 202)
            job_id = response.get_json()["jobs"][0]
            job = None
            for _ in range(100):
                job = self.client.get(f"/api/jobs/{job_id}").get_json()
                if job["status"] in {"completed", "failed"}:
                    break
                time.sleep(0.02)
        self.assertEqual(job["status"], "completed", job.get("error"))
        self.assertEqual(progress_updates, sorted(progress_updates))
        self.assertTrue(any(5 < progress < 95 for progress in progress_updates))
        self.assertEqual(len(job["outputs"]), 2)
        download = self.client.get(f"/api/jobs/{job_id}/download")
        self.assertEqual(download.status_code, 200)
        self.assertEqual(download.mimetype, "application/zip")
        download.close()

        job_folder = webapp.JOBS_DIR / job_id
        self.assertTrue(job_folder.exists())
        deleted = self.client.delete(f"/api/jobs/{job_id}")
        self.assertEqual(deleted.status_code, 200)
        self.assertEqual(deleted.get_json(), {"deleted": job_id})
        self.assertFalse(job_folder.exists())
        self.assertEqual(self.client.get(f"/api/jobs/{job_id}").status_code, 404)

    def test_malformed_subtitle_job_fails_with_useful_error(self):
        response = self.client.post("/api/jobs", data={
            "provider": "echo",
            "target_languages": "es",
            "files": (io.BytesIO(b"not a subtitle"), "broken.srt"),
        }, content_type="multipart/form-data")
        self.assertEqual(response.status_code, 202)
        job_id = response.get_json()["jobs"][0]

        job = None
        for _ in range(100):
            job = self.client.get(f"/api/jobs/{job_id}").get_json()
            if job["status"] in {"completed", "failed", "canceled"}:
                break
            time.sleep(0.02)

        self.assertEqual(job["status"], "failed", job.get("error"))
        self.assertIn("No subtitle cues", job["error"])
        self.assertEqual(self.client.delete(f"/api/jobs/{job_id}").status_code, 200)

    def test_download_serves_only_registered_outputs(self):
        outputs = [{"name": "sample.es.srt", "language": "es"}]
        folder = self.insert_job("download-boundary-test", outputs=outputs)
        (folder / "sample.es.srt").write_text("translated", "utf-8")
        (folder / "private.txt").write_text("private", "utf-8")

        allowed = self.client.get(
            "/api/jobs/download-boundary-test/download/sample.es.srt"
        )
        blocked = self.client.get(
            "/api/jobs/download-boundary-test/download/private.txt"
        )

        self.assertEqual(allowed.status_code, 200)
        self.assertEqual(allowed.data, b"translated")
        self.assertEqual(blocked.status_code, 404)

    def test_download_all_rejects_jobs_without_outputs(self):
        self.insert_job("no-output-test", outputs=[])

        response = self.client.get("/api/jobs/no-output-test/download")

        self.assertEqual(response.status_code, 404)
        self.assertIn("No outputs", response.get_json()["error"])


if __name__ == "__main__":
    unittest.main()
