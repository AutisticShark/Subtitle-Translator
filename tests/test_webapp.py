import io
import os
import tempfile
import time
import unittest


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

    def test_echo_job_end_to_end(self):
        response = self.client.post("/api/jobs", data={
            "provider": "echo",
            "target_languages": "zh-TW,ja",
            "source_language": "English",
            "files": (io.BytesIO(
                b"1\n00:00:01,000 --> 00:00:02,000\nHello there\n\n"
            ), "sample.srt"),
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
        self.assertEqual(len(job["outputs"]), 2)
        download = self.client.get(f"/api/jobs/{job_id}/download")
        self.assertEqual(download.status_code, 200)
        self.assertEqual(download.mimetype, "application/zip")


if __name__ == "__main__":
    unittest.main()
