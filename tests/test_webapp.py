import io
import json
import os
import shutil
import tempfile
import threading
import time
import unittest
import urllib.error
from unittest.mock import patch


_data_directory = tempfile.TemporaryDirectory()
os.environ["DATA_DIR"] = _data_directory.name
os.environ["JOB_WORKERS"] = "1"
os.environ["JWT_SECRET_KEY"] = "test-jwt-secret-that-is-long-and-stable-for-tests"
os.environ["ADMIN_USERNAME"] = "admin"
os.environ["ADMIN_PASSWORD"] = "correct-horse-battery-staple"

import webapp  # noqa: E402  (environment must be configured before import)


class WebApplicationTests(unittest.TestCase):
    def setUp(self):
        webapp.app.config.update(TESTING=True, DEBUG=True, JWT_COOKIE_CSRF_PROTECT=False)
        self.client = webapp.app.test_client()
        response = self.client.post("/api/auth/login", json={
            "username": "admin",
            "password": "correct-horse-battery-staple",
        })
        self.assertEqual(response.status_code, 200, response.get_json())
        response = self.client.put("/api/settings", json={
            "rate_limit_window_minutes": 60,
            "user_job_limit": 0,
            "admin_job_limit": 0,
            "panel_job_limit": 0,
            "registration_enabled": "1",
            "captcha_provider": "none",
            "captcha_on_login": "1",
            "captcha_on_register": "1",
            "captcha_on_upload": "1",
        })
        self.assertEqual(response.status_code, 200, response.get_json())

    def tearDown(self):
        # CAPTCHA settings persist across tests; leave login open for the next fixture setup.
        self.client.put("/api/settings", json={
            "captcha_provider": "none", "registration_enabled": "1",
        })
        super().tearDown()

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
        page = self.client.get("/")
        self.assertIn(b'id="authView"', page.data)
        self.assertIn("frame-ancestors 'none'", page.headers["Content-Security-Policy"])
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
        with webapp.connect_db() as db:
            stored = db.execute(
                "SELECT value FROM settings WHERE name='anthropic_api_key'"
            ).fetchone()["value"]
        self.assertTrue(stored.startswith("enc:v1:"))
        self.assertNotIn("anthropic-secret", stored)

    def test_public_registration_creates_only_regular_users_and_signs_them_in(self):
        anonymous = webapp.app.test_client()
        status = anonymous.get("/api/auth/setup-status").get_json()
        self.assertTrue(status["configured"])
        self.assertTrue(status["registration_enabled"])
        self.assertEqual(status["captcha"], {
            "provider": "none", "site_key": "", "protected_actions": [],
        })

        response = anonymous.post("/api/auth/register", json={
            "username": "self-service-user",
            "password": "self-service-password",
            "confirm_password": "self-service-password",
        })
        self.assertEqual(response.status_code, 201, response.get_json())
        user = response.get_json()["user"]
        self.addCleanup(self.client.delete, f"/api/users/{user['id']}")
        self.assertEqual(user["role"], "user")
        self.assertEqual(anonymous.get("/api/auth/me").get_json()["user"]["id"], user["id"])
        self.assertEqual(anonymous.post("/api/auth/register", json={
            "username": "another-user",
            "password": "two-different-passwords",
            "confirm_password": "not-the-same-password",
        }).status_code, 400)
        self.assertEqual(anonymous.post("/api/auth/register", json={
            "username": "self-service-user",
            "password": "self-service-password",
            "confirm_password": "self-service-password",
        }).status_code, 409)

    def test_registration_can_be_disabled_by_an_administrator(self):
        disabled = self.client.put("/api/settings", json={"registration_enabled": "0"})
        self.assertEqual(disabled.status_code, 200, disabled.get_json())
        anonymous = webapp.app.test_client()
        self.assertFalse(anonymous.get("/api/auth/setup-status").get_json()[
            "registration_enabled"
        ])
        response = anonymous.post("/api/auth/register", json={
            "username": "blocked-signup",
            "password": "blocked-signup-password",
            "confirm_password": "blocked-signup-password",
        })
        self.assertEqual(response.status_code, 403, response.get_json())

    def test_registration_requires_a_server_verified_captcha_when_enabled(self):
        configured = self.client.put("/api/settings", json={
            "captcha_provider": "recaptcha",
            "captcha_hostname": "localhost",
            "captcha_on_login": "0",
            "captcha_on_register": "1",
            "captcha_on_upload": "0",
            "recaptcha_site_key": "registration-site-key",
            "recaptcha_secret_key": "registration-secret-key",
        })
        self.assertEqual(configured.status_code, 200, configured.get_json())
        payload = {
            "username": "captcha-registration-user",
            "password": "captcha-registration-password",
            "confirm_password": "captcha-registration-password",
        }
        anonymous = webapp.app.test_client()
        self.assertEqual(anonymous.post(
            "/api/auth/register", json=payload,
        ).status_code, 400)
        with patch.object(
            webapp.urllib.request, "urlopen",
            return_value=io.BytesIO(json.dumps({
                "success": True, "hostname": "localhost",
            }).encode()),
        ):
            accepted = anonymous.post("/api/auth/register", json={
                **payload, "captcha_token": "fresh-registration-token",
            })
        self.assertEqual(accepted.status_code, 201, accepted.get_json())
        user_id = accepted.get_json()["user"]["id"]
        self.addCleanup(self.client.delete, f"/api/users/{user_id}")

    def test_all_captcha_providers_verify_login_server_side_and_mask_secrets(self):
        created = self.client.post("/api/users", json={
            "username": "captcha-login-user",
            "password": "captcha-login-password",
            "role": "user",
        }).get_json()["user"]
        self.addCleanup(self.client.delete, f"/api/users/{created['id']}")
        endpoints = {
            "turnstile": "https://challenges.cloudflare.com/turnstile/v0/siteverify",
            "recaptcha": "https://www.google.com/recaptcha/api/siteverify",
            "hcaptcha": "https://api.hcaptcha.com/siteverify",
        }
        for provider, endpoint in endpoints.items():
            with self.subTest(provider=provider):
                saved = self.client.put("/api/settings", json={
                    "captcha_provider": provider,
                    "captcha_hostname": "localhost",
                    "captcha_on_login": "1",
                    "captcha_on_register": "0",
                    "captcha_on_upload": "0",
                    f"{provider}_site_key": f"{provider}-site",
                    f"{provider}_secret_key": f"{provider}-secret",
                })
                self.assertEqual(saved.status_code, 200, saved.get_json())
                settings = self.client.get("/api/settings").get_json()
                self.assertTrue(settings["captcha_configured"][provider])
                self.assertNotIn(f"{provider}_secret_key", settings)
                with webapp.connect_db() as db:
                    encrypted = db.execute(
                        "SELECT value FROM settings WHERE name=?",
                        (f"{provider}_secret_key",),
                    ).fetchone()["value"]
                self.assertTrue(encrypted.startswith("enc:v1:"))
                self.assertNotIn(f"{provider}-secret", encrypted)
                public = webapp.app.test_client().get("/api/auth/setup-status").get_json()
                self.assertEqual(public["captcha"]["site_key"], f"{provider}-site")
                self.assertNotIn("secret", json.dumps(public).lower())

                missing = webapp.app.test_client().post("/api/auth/login", json={
                    "username": "captcha-login-user",
                    "password": "captcha-login-password",
                })
                self.assertEqual(missing.status_code, 400, missing.get_json())

                verification = {"success": True, "hostname": "localhost"}
                if provider == "turnstile":
                    verification["action"] = "login"
                with patch.object(
                    webapp.urllib.request, "urlopen",
                    return_value=io.BytesIO(json.dumps(verification).encode()),
                ) as urlopen:
                    accepted = webapp.app.test_client().post("/api/auth/login", json={
                        "username": "captcha-login-user",
                        "password": "captcha-login-password",
                        "captcha_token": "fresh-token",
                    })
                self.assertEqual(accepted.status_code, 200, accepted.get_json())
                verification_request = urlopen.call_args.args[0]
                self.assertEqual(verification_request.full_url, endpoint)
                form = webapp.urllib.parse.parse_qs(verification_request.data.decode())
                self.assertEqual(form["secret"], [f"{provider}-secret"])
                self.assertEqual(form["response"], ["fresh-token"])
                if provider == "hcaptcha":
                    self.assertEqual(form["sitekey"], ["hcaptcha-site"])

    def test_captcha_fails_closed_for_outages_hostname_and_turnstile_action(self):
        saved = self.client.put("/api/settings", json={
            "captcha_provider": "turnstile",
            "captcha_hostname": "expected.example",
            "captcha_on_login": "1",
            "captcha_on_register": "0",
            "captcha_on_upload": "0",
            "turnstile_site_key": "site-key",
            "turnstile_secret_key": "secret-key",
        })
        self.assertEqual(saved.status_code, 200, saved.get_json())
        payload = {
            "username": "admin", "password": "correct-horse-battery-staple",
            "captcha_token": "token",
        }
        with patch.object(
            webapp.urllib.request, "urlopen", side_effect=urllib.error.URLError("offline"),
        ):
            self.assertEqual(
                webapp.app.test_client().post("/api/auth/login", json=payload).status_code,
                503,
            )
        for result in (
            {"success": True, "hostname": "wrong.example", "action": "login"},
            {"success": True, "hostname": "expected.example", "action": "upload"},
            {"success": False, "error-codes": ["timeout-or-duplicate"]},
        ):
            with self.subTest(result=result), patch.object(
                webapp.urllib.request, "urlopen",
                return_value=io.BytesIO(json.dumps(result).encode()),
            ):
                self.assertEqual(
                    webapp.app.test_client().post("/api/auth/login", json=payload).status_code,
                    400,
                )

    def test_upload_requires_a_fresh_server_verified_captcha(self):
        saved = self.client.put("/api/settings", json={
            "captcha_provider": "turnstile",
            "captcha_hostname": "localhost",
            "captcha_on_login": "0",
            "captcha_on_register": "0",
            "captcha_on_upload": "1",
            "turnstile_site_key": "upload-site-key",
            "turnstile_secret_key": "upload-secret-key",
        })
        self.assertEqual(saved.status_code, 200, saved.get_json())
        source = b"1\n00:00:01,000 --> 00:00:02,000\nHello\n\n"
        missing = self.client.post("/api/jobs", data={
            "provider": "echo",
            "target_languages": "es",
            "files": (io.BytesIO(source), "captcha.srt"),
        }, content_type="multipart/form-data")
        self.assertEqual(missing.status_code, 400, missing.get_json())

        verification = {"success": True, "hostname": "localhost", "action": "upload"}
        with patch.object(
            webapp.urllib.request, "urlopen",
            return_value=io.BytesIO(json.dumps(verification).encode()),
        ), patch.object(webapp.executor, "submit") as submit:
            accepted = self.client.post("/api/jobs", data={
                "provider": "echo",
                "target_languages": "es",
                "captcha_token": "fresh-upload-token",
                "files": (io.BytesIO(source), "captcha.srt"),
            }, content_type="multipart/form-data")
        self.assertEqual(accepted.status_code, 202, accepted.get_json())
        job_id = accepted.get_json()["jobs"][0]
        submit.assert_called_once_with(webapp.run_job, job_id)

        def cleanup_job():
            with webapp.connect_db() as db:
                db.execute("DELETE FROM rate_limit_buckets WHERE scope='panel'")
                db.execute("DELETE FROM rate_limit_buckets WHERE scope=?", (
                    f"user:{self.client.get('/api/auth/me').get_json()['user']['id']}",
                ))
                db.execute("DELETE FROM jobs WHERE id=?", (job_id,))
            shutil.rmtree(webapp.JOBS_DIR / job_id, ignore_errors=True)

        self.addCleanup(cleanup_job)

    def test_signed_in_shell_has_dedicated_role_aware_dashboard_views(self):
        page = self.client.get("/")

        self.assertEqual(page.status_code, 200)
        self.assertIn(b'data-view="dashboard"', page.data)
        self.assertIn(b'data-view="translate" hidden', page.data)
        self.assertIn(b'data-view="jobs" hidden', page.data)
        self.assertIn(b'data-view="admin" id="adminView" hidden', page.data)
        self.assertIn(b'id="adminNavButton"', page.data)
        self.assertIn(b'id="dashboardMetrics"', page.data)
        self.assertIn(b'id="adminMetrics"', page.data)
        self.assertIn(b'id="userList"', page.data)
        self.assertIn(b'id="authSwitchButton"', page.data)
        self.assertIn(b'id="authCaptcha"', page.data)
        self.assertIn(b'id="uploadCaptcha"', page.data)
        self.assertIn(b'name="captcha_provider"', page.data)
        self.assertIn(b'name="registration_enabled"', page.data)
        self.assertIn(b'id="themeSelect"', page.data)
        self.assertIn(b'data-theme="system"', page.data)
        self.assertNotIn(b'id="usersDialog"', page.data)

    def test_theme_preference_is_validated_persisted_and_account_scoped(self):
        self.addCleanup(self.client.patch, "/api/auth/me", json={"theme": "system"})

        invalid = self.client.patch("/api/auth/me", json={"theme": "midnight"})
        self.assertEqual(invalid.status_code, 400, invalid.get_json())
        self.assertIn("Theme must be", invalid.get_json()["error"])

        saved = self.client.patch("/api/auth/me", json={"theme": "dark"})
        self.assertEqual(saved.status_code, 200, saved.get_json())
        self.assertEqual(saved.get_json()["user"]["theme"], "dark")
        self.assertIn(b'data-theme="dark"', self.client.get("/").data)

        fresh_admin = webapp.app.test_client()
        login = fresh_admin.post("/api/auth/login", json={
            "username": "admin", "password": "correct-horse-battery-staple",
        })
        self.assertEqual(login.status_code, 200, login.get_json())
        self.assertEqual(login.get_json()["user"]["theme"], "dark")
        self.assertIn(b'data-theme="dark"', fresh_admin.get("/").data)

        created = self.client.post("/api/users", json={
            "username": "theme-user", "password": "theme-user-password", "role": "user",
        })
        self.assertEqual(created.status_code, 201, created.get_json())
        user_id = created.get_json()["user"]["id"]
        self.addCleanup(self.client.delete, f"/api/users/{user_id}")

        user_client = webapp.app.test_client()
        user_login = user_client.post("/api/auth/login", json={
            "username": "theme-user", "password": "theme-user-password",
        })
        self.assertEqual(user_login.status_code, 200, user_login.get_json())
        self.assertEqual(user_login.get_json()["user"]["theme"], "system")
        user_saved = user_client.patch("/api/auth/me", json={"theme": "light"})
        self.assertEqual(user_saved.status_code, 200, user_saved.get_json())
        self.assertEqual(user_saved.get_json()["user"]["theme"], "light")
        self.assertEqual(
            self.client.get("/api/auth/me").get_json()["user"]["theme"], "dark",
        )
        self.assertIn(b'data-theme="system"', webapp.app.test_client().get("/").data)

    def test_ui_locale_detection_catalog_and_persistence(self):
        anonymous = webapp.app.test_client()
        page = anonymous.get("/", headers={"Accept-Language": "zh-Hant, en;q=0.8"})
        self.assertEqual(page.status_code, 200)
        self.assertIn(b'<html lang="zh-TW" data-theme="system">', page.data)
        self.assertIn("字幕翻譯器".encode(), page.data)
        self.assertEqual(page.headers["Content-Language"], "zh-TW")

        selected = anonymous.get("/?lang=zh-TW")
        self.assertIn("ui_locale=zh-TW", selected.headers["Set-Cookie"])
        persisted = anonymous.get("/")
        self.assertIn(b'<html lang="zh-TW" data-theme="system">', persisted.data)

        catalog = anonymous.get("/api/i18n").get_json()
        self.assertEqual(catalog["locale"], "zh-TW")
        self.assertEqual(catalog["languages"]["ja"], "日文")
        self.assertEqual(catalog["messages"]["Sign in"], "登入")

        simplified_client = webapp.app.test_client()
        simplified_page = simplified_client.get("/?lang=zh-CN")
        self.assertIn("ui_locale=zh-CN", simplified_page.headers["Set-Cookie"])
        self.assertIn(b'<html lang="zh-CN" data-theme="system">', simplified_page.data)
        self.assertIn("字幕翻译器".encode(), simplified_page.data)

        simplified = simplified_client.get("/api/i18n").get_json()
        self.assertEqual(simplified["locale"], "zh-CN")
        self.assertEqual(simplified["languages"]["ja"], "日语")
        self.assertEqual(simplified["messages"]["Sign in"], "登录")

    def test_api_errors_and_job_stages_follow_request_locale(self):
        anonymous = webapp.app.test_client()
        login = anonymous.post(
            "/api/auth/login", json={"username": "missing", "password": "incorrect"},
            headers={"Accept-Language": "zh-TW"},
        )
        self.assertEqual(login.status_code, 401)
        self.assertEqual(login.get_json()["error"], "使用者名稱或密碼無效")

        with webapp.app.test_request_context(headers={"Accept-Language": "zh-TW"}):
            self.assertEqual(webapp.localized_job_stage("Reading subtitle"), "正在讀取字幕")
            self.assertEqual(
                webapp.localized_job_stage("Translating to Japanese (2/5 segments)"),
                "正在翻譯為日文（2/5 個片段）",
            )

    def test_health_is_public_but_api_requires_jwt(self):
        anonymous = webapp.app.test_client()
        self.assertEqual(anonymous.get("/healthz").status_code, 200)
        unauthorized = anonymous.get("/api/settings")
        basic = anonymous.get(
            "/api/settings", headers={"Authorization": "Basic dXNlcjpzZWNyZXQ="},
        )
        self.assertEqual(unauthorized.status_code, 401)
        self.assertEqual(basic.status_code, 401)
        self.assertEqual(self.client.get("/api/settings").status_code, 200)

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
            ({"rate_limit_window_minutes": 0}, "must be a whole number between 1 and 10080"),
            ({"user_job_limit": "1.5"}, "must be a whole number between 0 and 100000"),
            ({"captcha_provider": "robot-checker"}, "Invalid CAPTCHA provider"),
            ({"registration_enabled": "yes"}, "must be enabled or disabled"),
            ({"captcha_hostname": "https://example.com:443"}, "without a scheme or port"),
            ({"captcha_provider": "turnstile", "turnstile_site_key": ""},
             "Configure the selected CAPTCHA site key and secret key"),
        ]
        for payload, error in cases:
            with self.subTest(payload=payload):
                response = self.client.put("/api/settings", json=payload)
                self.assertEqual(response.status_code, 400)
                self.assertIn(error, response.get_json()["error"])

    def test_rate_limit_settings_are_available_in_the_admin_portal(self):
        response = self.client.put("/api/settings", json={
            "rate_limit_window_minutes": 15,
            "user_job_limit": 3,
            "admin_job_limit": 7,
            "panel_job_limit": 20,
        })

        self.assertEqual(response.status_code, 200, response.get_json())
        settings = response.get_json()
        self.assertEqual(settings["rate_limit_window_minutes"], "15")
        self.assertEqual(settings["user_job_limit"], "3")
        self.assertEqual(settings["admin_job_limit"], "7")
        self.assertEqual(settings["panel_job_limit"], "20")
        page = self.client.get("/")
        for field in (
            b'rate_limit_window_minutes', b'user_job_limit',
            b'admin_job_limit', b'panel_job_limit',
        ):
            self.assertIn(field, page.data)

    def test_blank_secret_preserves_saved_value_and_delete_key_removes_it(self):
        self.client.put("/api/settings", json={"openai_api_key": "saved-secret"})

        response = self.client.put("/api/settings", json={"openai_api_key": "  "})
        self.assertTrue(response.get_json()["configured"]["openai"])
        with patch.dict(os.environ, {"OPENAI_API_KEY": ""}):
            deleted = self.client.delete("/api/settings/keys/openai")

        self.assertEqual(deleted.status_code, 200)
        self.assertFalse(deleted.get_json()["configured"]["openai"])
        self.assertEqual(self.client.delete("/api/settings/keys/unknown").status_code, 404)

    def test_active_captcha_secret_cannot_be_removed_until_protection_is_disabled(self):
        configured = self.client.put("/api/settings", json={
            "captcha_provider": "hcaptcha",
            "captcha_on_login": "1",
            "hcaptcha_site_key": "active-site-key",
            "hcaptcha_secret_key": "active-secret-key",
        })
        self.assertEqual(configured.status_code, 200, configured.get_json())
        blocked = self.client.delete("/api/settings/keys/captcha-hcaptcha")
        self.assertEqual(blocked.status_code, 409, blocked.get_json())
        self.assertTrue(self.client.get("/api/settings").get_json()[
            "captcha_configured"
        ]["hcaptcha"])
        self.assertEqual(
            self.client.put("/api/settings", json={"captcha_provider": "none"}).status_code,
            200,
        )
        removed = self.client.delete("/api/settings/keys/captcha-hcaptcha")
        self.assertEqual(removed.status_code, 200, removed.get_json())
        self.assertFalse(removed.get_json()["captcha_configured"]["hcaptcha"])

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

    @staticmethod
    def _remove_rate_limit_test_data(job_ids, user_ids=()):
        with webapp.connect_db() as db:
            for job_id in job_ids:
                db.execute("DELETE FROM jobs WHERE id=?", (job_id,))
            for user_id in user_ids:
                db.execute("DELETE FROM rate_limit_buckets WHERE scope=?", (f"user:{user_id}",))
                db.execute("DELETE FROM users WHERE id=?", (user_id,))
        for job_id in job_ids:
            shutil.rmtree(webapp.JOBS_DIR / job_id, ignore_errors=True)

    @staticmethod
    def _job_payload(*names):
        source = b"1\n00:00:01,000 --> 00:00:02,000\nHello\n\n"
        return {
            "provider": "echo",
            "target_languages": "es",
            "files": [(io.BytesIO(source), name) for name in names],
        }

    def test_regular_users_and_administrators_have_separate_per_account_limits(self):
        created_user = self.client.post("/api/users", json={
            "username": "limited-user",
            "password": "limited-user-password",
            "role": "user",
        }).get_json()["user"]
        user_client = webapp.app.test_client()
        self.assertEqual(user_client.post("/api/auth/login", json={
            "username": "limited-user", "password": "limited-user-password",
        }).status_code, 200)
        configured = self.client.put("/api/settings", json={
            "rate_limit_window_minutes": 60,
            "user_job_limit": 1,
            "admin_job_limit": 2,
            "panel_job_limit": 0,
        })
        self.assertEqual(configured.status_code, 200, configured.get_json())
        job_ids = []
        self.addCleanup(self._remove_rate_limit_test_data, job_ids, (created_user["id"],))

        with patch.object(webapp.executor, "submit") as submit:
            first_user = user_client.post(
                "/api/jobs", data=self._job_payload("user-one.srt"),
                content_type="multipart/form-data",
            )
            second_user = user_client.post(
                "/api/jobs", data=self._job_payload("user-two.srt"),
                content_type="multipart/form-data",
            )
            first_admin = self.client.post(
                "/api/jobs", data=self._job_payload("admin-one.srt"),
                content_type="multipart/form-data",
            )
            second_admin = self.client.post(
                "/api/jobs", data=self._job_payload("admin-two.srt"),
                content_type="multipart/form-data",
            )
            third_admin = self.client.post(
                "/api/jobs", data=self._job_payload("admin-three.srt"),
                content_type="multipart/form-data",
            )

        for response in (first_user, first_admin, second_admin):
            self.assertEqual(response.status_code, 202, response.get_json())
            job_ids.extend(response.get_json()["jobs"])
        self.assertEqual(second_user.status_code, 429, second_user.get_json())
        self.assertEqual(second_user.get_json()["scope"], "user")
        self.assertEqual(second_user.get_json()["limit"], 1)
        self.assertGreater(int(second_user.headers["Retry-After"]), 0)
        self.assertEqual(third_admin.status_code, 429, third_admin.get_json())
        self.assertEqual(third_admin.get_json()["scope"], "admin")
        self.assertEqual(third_admin.get_json()["limit"], 2)
        self.assertEqual(submit.call_count, 3)

    def test_panel_limit_counts_each_file_across_accounts_and_rejects_atomically(self):
        created_user = self.client.post("/api/users", json={
            "username": "panel-user",
            "password": "panel-user-password",
            "role": "user",
        }).get_json()["user"]
        user_client = webapp.app.test_client()
        self.assertEqual(user_client.post("/api/auth/login", json={
            "username": "panel-user", "password": "panel-user-password",
        }).status_code, 200)
        configured = self.client.put("/api/settings", json={
            "rate_limit_window_minutes": 60,
            "user_job_limit": 0,
            "admin_job_limit": 0,
            "panel_job_limit": 2,
        })
        self.assertEqual(configured.status_code, 200, configured.get_json())
        job_ids = []
        self.addCleanup(self._remove_rate_limit_test_data, job_ids, (created_user["id"],))

        with patch.object(webapp.executor, "submit") as submit:
            accepted = user_client.post(
                "/api/jobs", data=self._job_payload("first.srt", "second.srt"),
                content_type="multipart/form-data",
            )
            folders_before_rejection = set(webapp.JOBS_DIR.iterdir())
            rejected = self.client.post(
                "/api/jobs", data=self._job_payload("third.srt"),
                content_type="multipart/form-data",
            )

        self.assertEqual(accepted.status_code, 202, accepted.get_json())
        job_ids.extend(accepted.get_json()["jobs"])
        self.assertEqual(len(job_ids), 2)
        self.assertEqual(rejected.status_code, 429, rejected.get_json())
        self.assertEqual(rejected.get_json()["scope"], "panel")
        self.assertEqual(rejected.get_json()["limit"], 2)
        self.assertEqual(set(webapp.JOBS_DIR.iterdir()), folders_before_rejection)
        self.assertEqual(submit.call_count, 2)

    def test_rate_limit_bucket_reopens_after_the_configured_window(self):
        configured = self.client.put("/api/settings", json={
            "rate_limit_window_minutes": 1,
            "user_job_limit": 0,
            "admin_job_limit": 1,
            "panel_job_limit": 0,
        })
        self.assertEqual(configured.status_code, 200, configured.get_json())
        with webapp.connection(webapp.engine) as db:
            admin = db.execute(webapp.select(webapp.users).where(
                webapp.users.c.username == "admin"
            )).first()
        started = webapp.datetime(2026, 1, 1, tzinfo=webapp.timezone.utc)

        with patch.object(webapp, "now_datetime", return_value=started):
            with webapp.transaction(webapp.engine) as db:
                self.assertIsNone(webapp.consume_job_quota(db, admin, 1))
        with patch.object(webapp, "now_datetime", return_value=started + webapp.timedelta(seconds=30)):
            with webapp.transaction(webapp.engine) as db:
                exceeded = webapp.consume_job_quota(db, admin, 1)
        self.assertEqual(exceeded["scope"], "admin")
        self.assertEqual(exceeded["retry_after"], 30)
        with patch.object(webapp, "now_datetime", return_value=started + webapp.timedelta(seconds=61)):
            with webapp.transaction(webapp.engine) as db:
                self.assertIsNone(webapp.consume_job_quota(db, admin, 1))

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

    def test_queued_job_is_finalized_by_worker_after_cancellation_request(self):
        job_id = "queued-cancel-test"
        timestamp = webapp.now()
        with webapp.connect_db() as db:
            db.execute(
                "INSERT INTO jobs(id, filename, stored_name, status, options, created_at, updated_at) "
                "VALUES (?, 'queued.srt', 'source.srt', 'queued', '{}', ?, ?)",
                (job_id, timestamp, timestamp),
            )

        response = self.client.post(f"/api/jobs/{job_id}/cancel")
        self.assertEqual(response.status_code, 202)
        self.assertEqual(response.get_json()["status"], "canceling")
        webapp.run_job(job_id)
        self.assertEqual(self.client.get(f"/api/jobs/{job_id}").get_json()["status"], "canceled")
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

    def test_cookie_jwt_requires_csrf_and_logout_revokes_it(self):
        webapp.app.config["JWT_COOKIE_CSRF_PROTECT"] = True
        secure_client = webapp.app.test_client()
        try:
            login = secure_client.post("/api/auth/login", json={
                "username": "admin",
                "password": "correct-horse-battery-staple",
            })
            self.assertEqual(login.status_code, 200, login.get_json())
            cookies = login.headers.getlist("Set-Cookie")
            self.assertTrue(any("HttpOnly" in value and "SameSite=Strict" in value
                                for value in cookies))
            self.assertEqual(secure_client.get("/api/auth/me").status_code, 200)

            missing_csrf = secure_client.put("/api/settings", json={"batch_size": "2"})
            self.assertEqual(missing_csrf.status_code, 401)
            csrf = secure_client.get_cookie("csrf_access_token").value
            saved = secure_client.put(
                "/api/settings",
                json={"batch_size": "2"},
                headers={"X-CSRF-TOKEN": csrf},
            )
            self.assertEqual(saved.status_code, 200, saved.get_json())

            csrf = secure_client.get_cookie("csrf_access_token").value
            logged_out = secure_client.post(
                "/api/auth/logout", headers={"X-CSRF-TOKEN": csrf},
            )
            self.assertEqual(logged_out.status_code, 200)
            self.assertEqual(secure_client.get("/api/auth/me").status_code, 401)
        finally:
            webapp.app.config["JWT_COOKIE_CSRF_PROTECT"] = False

    def test_header_bearer_token_does_not_require_browser_csrf(self):
        token_response = webapp.app.test_client().post("/api/auth/login", json={
            "username": "admin",
            "password": "correct-horse-battery-staple",
            "token_transport": "header",
        })
        self.assertEqual(token_response.status_code, 200, token_response.get_json())
        self.assertFalse(token_response.headers.getlist("Set-Cookie"))
        headers = {"Authorization": f"Bearer {token_response.get_json()['access_token']}"}
        self.assertEqual(
            webapp.app.test_client().get("/api/settings", headers=headers).status_code, 200,
        )
        updated = webapp.app.test_client().put(
            "/api/settings", json={"batch_size": "4"}, headers=headers,
        )
        self.assertEqual(updated.status_code, 200, updated.get_json())

    def test_regular_users_are_denied_admin_apis_and_other_users_jobs(self):
        created_ids = []
        for username in ("alice", "bob"):
            response = self.client.post("/api/users", json={
                "username": username,
                "password": f"{username}-password-1234",
                "role": "user",
            })
            self.assertEqual(response.status_code, 201, response.get_json())
            created_ids.append(response.get_json()["user"]["id"])

        alice = webapp.app.test_client()
        bob = webapp.app.test_client()
        self.assertEqual(alice.post("/api/auth/login", json={
            "username": "alice", "password": "alice-password-1234",
        }).status_code, 200)
        self.assertEqual(bob.post("/api/auth/login", json={
            "username": "bob", "password": "bob-password-1234",
        }).status_code, 200)
        self.assertEqual(alice.put("/api/settings", json={"batch_size": "3"}).status_code, 403)
        self.assertEqual(alice.get("/api/users").status_code, 403)

        source = b"1\n00:00:01,000 --> 00:00:02,000\nHello\n\n"
        with patch.object(webapp.executor, "submit") as submit:
            created = alice.post("/api/jobs", data={
                "provider": "echo",
                "target_languages": "es",
                "files": (io.BytesIO(source), "owned.srt"),
            }, content_type="multipart/form-data")
        self.assertEqual(created.status_code, 202, created.get_json())
        job_id = created.get_json()["jobs"][0]
        submit.assert_called_once_with(webapp.run_job, job_id)
        self.assertEqual(bob.get(f"/api/jobs/{job_id}").status_code, 404)
        self.assertFalse(bob.get("/api/jobs").get_json()["jobs"])
        all_jobs = self.client.get("/api/jobs?all=1").get_json()["jobs"]
        self.assertEqual(next(job for job in all_jobs if job["id"] == job_id)["owner"], "alice")

        self.assertEqual(self.client.post(f"/api/jobs/{job_id}/cancel").status_code, 202)
        webapp.run_job(job_id)
        self.assertEqual(self.client.delete(f"/api/jobs/{job_id}").status_code, 200)
        for user_id in created_ids:
            self.assertEqual(self.client.delete(f"/api/users/{user_id}").status_code, 200)

    def test_deactivating_user_revokes_existing_token(self):
        created = self.client.post("/api/users", json={
            "username": "revoked-user",
            "password": "revoked-user-password",
            "role": "user",
        }).get_json()["user"]
        user_client = webapp.app.test_client()
        self.assertEqual(user_client.post("/api/auth/login", json={
            "username": "revoked-user", "password": "revoked-user-password",
        }).status_code, 200)
        self.assertEqual(user_client.get("/api/auth/me").status_code, 200)

        disabled = self.client.patch(
            f"/api/users/{created['id']}", json={"active": False},
        )
        self.assertEqual(disabled.status_code, 200, disabled.get_json())
        self.assertEqual(user_client.get("/api/auth/me").status_code, 401)
        self.assertEqual(self.client.delete(f"/api/users/{created['id']}").status_code, 200)

    def test_initial_setup_cannot_be_repeated(self):
        response = webapp.app.test_client().post("/api/auth/setup", json={
            "username": "attacker",
            "password": "attacker-password-123",
        })
        self.assertEqual(response.status_code, 409)

    def test_current_and_final_administrator_is_protected(self):
        admin = self.client.get("/api/auth/me").get_json()["user"]
        self.assertEqual(
            self.client.patch(f"/api/users/{admin['id']}", json={"active": False}).status_code,
            409,
        )
        self.assertEqual(
            self.client.patch(f"/api/users/{admin['id']}", json={"role": "user"}).status_code,
            409,
        )
        self.assertEqual(self.client.delete(f"/api/users/{admin['id']}").status_code, 409)

    @classmethod
    def tearDownClass(cls):
        webapp.engine.dispose()
        super().tearDownClass()


if __name__ == "__main__":
    unittest.main()
