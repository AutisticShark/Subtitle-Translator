import io
import os
import tempfile
import time
import unittest
from unittest.mock import patch


_data_directory = tempfile.TemporaryDirectory()
os.environ["DATA_DIR"] = _data_directory.name
os.environ["JOB_WORKERS"] = "1"

import webapp  # noqa: E402  (environment must be configured before import)


class WebApplicationTests(unittest.TestCase):
    def setUp(self):
        webapp.app.config.update(TESTING=True)
        self.client = webapp.app.test_client()

    def test_health_and_secret_masking(self):
        self.assertEqual(self.client.get("/healthz").status_code, 200)
        response = self.client.put("/api/settings", json={"anthropic_api_key": "secret"})
        self.assertEqual(response.status_code, 200)
        settings = self.client.get("/api/settings").get_json()
        self.assertTrue(settings["configured"]["anthropic"])
        self.assertNotIn("anthropic_api_key", settings)

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


if __name__ == "__main__":
    unittest.main()
