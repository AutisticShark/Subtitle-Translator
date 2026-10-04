# Subtitle Translator agent guide

This file applies to the entire repository. Read `MEMORY.md` before making changes; it contains the evidence-based retrospective from earlier work. Keep both files updated when a session reveals a reusable project-specific lesson.

## Project map

- `srt_translate.py` contains the translation pipeline, provider clients (including Google Cloud Translation - Basic v2), retry/throttling behavior, segmentation, tag masking, cue rebuilding, and wrapping. It is a library for the web app; there is no CLI.
- `subtitle_formats.py` adapts SRT, VTT, ASS, and SSA files to and from the shared cue model. Preserve format-specific headers, timings, settings, dialogue fields, newline style, BOM state, and inline tags.
- `webapp.py` is the Flask API, authentication/authorization layer, upload/download boundary, and background job runner. `DATA_DIR` is resolved at import time. Logic that does not need Flask or module-level state lives beside it: `settings_schema.py` (declarative setting bounds and validation), `quotas.py` (transactional job-quota accounting; clock and defaults injected), `captcha.py` (Siteverify protocol), and `timeutil.py`. Tests patch `webapp.now_datetime`, `webapp.read_cache`, `webapp.update_job`, `webapp.provider_for`, and similar module globals, so keep those orchestration functions in `webapp.py` and inject their dependencies into extracted modules.
- `email_delivery.py` owns the shared email provider registry and SMTP, SES, Aliyun DirectMail, and Resend transports. `mfa.py` composes verification messages and owns their authorization, SQL challenges, and send limits.
- `database.py` defines the portable SQLAlchemy schema, SQLite legacy migration, URL normalization (including `sslmode`/`ssl-mode` translation for pg8000 and PyMySQL) for SQLite, PostgreSQL, MariaDB, and MySQL, and the one-time pre-fork initialization called from `gunicorn.conf.py`.
- `static/app.js` and `templates/index.html` implement the browser UI.
- `i18n.py` and `locales/*.json` provide request-locale negotiation and web/API message catalogs. English source strings are the fallback.
- `tests/` currently covers subtitle-format preservation and an Echo-provider web job. Echo is the preferred offline end-to-end provider.
- `.github/workflows/docker-publish.yml` publishes to GHCR unconditionally and to Docker Hub only when all three Docker Hub settings are present.

## Required invariants

- Never expose saved API-key values through `GET /api/settings`; the UI may receive only configured/not-configured state.
- Keep provider settings and API-key mutation administrator-only. Saved API keys use `enc:v1:` Fernet encryption; never silently replace an unreadable key or log its value.
- Keep all application data APIs JWT-protected. Browser JWTs stay in HttpOnly `SameSite=Strict` cookies with CSRF enabled; `/healthz`, `/api/i18n`, the setup-status/setup/login/registration endpoints, and the initial HTML page are the intentional public boundaries. `/api/i18n` may expose only static catalog and language-label data; setup-status may additionally expose only registration state and the active CAPTCHA provider's public widget configuration.
- Scope every non-admin job read, mutation, and download by `user_id`. Use a not-found response for another user's job so its existence is not disclosed.
- User deactivation, password changes, and role changes must invalidate existing tokens. Never allow deletion, deactivation, or demotion of the final active administrator.
- MFA challenge JWTs must never authorize application APIs. Keep challenge purpose, user, token version, expiry, failure budgets, email send limits, and single-use OTP/recovery consumption in SQL. Lock the user row before MFA reads/mutations across all backends; a process-local lock cannot prevent replay across workers. Require password plus an existing factor to disable MFA or replace recovery codes, and never remove MFA as a side effect of an administrator password reset.
- Keep `/healthz` unauthenticated so Docker and reverse proxies can probe it.
- Sanitize uploaded filenames, isolate files under random job IDs, and only serve outputs registered to the requested job.
- Preserve subtitle structure and formatting while translating text. Add or change format behavior with focused round-trip tests.
- The project is web-app only; the command-line interface was removed. Do not reintroduce a CLI entry point without an explicit request.
- Treat `data/` as persistent runtime state. Tests must set `DATA_DIR` before importing `webapp` and must not use the repository's real data directory.
- Do not commit secrets, real API keys, job data, translated media, or generated caches.

## Regression-prevention rules from prior work

1. In an async DOM event handler, capture `event.currentTarget` synchronously before the first `await`, then use the captured element afterward. `event.currentTarget` can be `null` after control returns to the event loop. This previously broke translation-form cleanup and settings-form cleanup (commit `df532a4`).
2. Do not pass a blank optional image name to `docker/metadata-action`, even with an `enable=false` fragment. Build the image list so it contains only complete, non-empty names. This previously broke GHCR-only publishing when Docker Hub variables were absent (commit `3237715`).
3. `pytest` alone does not exercise browser event lifetime or GitHub Actions expression/action parsing. For frontend async-handler changes, inspect every post-`await` event access and perform a browser smoke test when available. For publishing changes, reason through both GHCR-only and GHCR-plus-Docker-Hub branches and validate the workflow when tooling is available.
4. Translation progress must be reported from completed translation batches, not only from target-language boundaries. Map each target's batch fraction into its share of the overall job range so multi-language jobs remain monotonic.
5. Job cancellation is cooperative and race-safe. A `queued` job has no worker, so cancel it straight to `canceled`; move a `processing` job to `canceling` and let only its worker finalize it. Both transitions are conditional `UPDATE ... WHERE status = ...` statements with a rowcount check. The in-memory event reaches only the same process, so running jobs also poll their own SQL status (throttled by `CANCEL_POLL_SECONDS`). Completion, failure, and progress updates must be conditional on the current status so they cannot overwrite a concurrent cancellation; a failure after a recorded cancellation finalizes as `canceled`.
6. Google Cloud Translation - Basic v2 accepts at most 128 strings per request and returns HTML-escaped `translatedText` values. Keep batches within that limit, unescape each value, and reject any response whose translation count differs from the input count so subtitle segments cannot be misaligned.
7. When writing explicitly assembled CRLF subtitle text, disable Python's platform newline translation (`newline=""`). Otherwise Windows converts each `\n` inside `\r\n` again and emits malformed `\r\r\n` line endings.
8. Echo is a development provider: expose and accept it in the web app only when Flask debug mode is enabled. Enforce this on the server as well as filtering the UI; Echo remains available to offline tests.
9. JWT cookie authentication requires a CSRF header on `POST`, `PUT`, `PATCH`, and `DELETE`. Browser code reads `csrf_access_token` only for the double-submit header; it must never copy the HttpOnly access JWT into JavaScript storage.
10. Database portability requires SQLAlchemy expressions, not backend-specific placeholders or upsert syntax. Ordinary `postgresql://`, `mariadb://`, and `mysql://` URLs are normalized to installed drivers; compile schema tests for all supported dialects when changing tables.
11. Translation submission limits count jobs, not HTTP requests: every uploaded subtitle consumes one unit. Enforce the per-account role limit and the panel-wide limit in the same database transaction as all job inserts, serialize submissions through the shared settings row, and reject a multi-file upload atomically with HTTP 429 and `Retry-After`.
12. Keep stored job statuses and stages locale-neutral. Localize them while serializing the response so shared jobs can be viewed in different interface languages. Locale selection order is explicit `?lang=`, the non-sensitive `ui_locale` cookie, browser `Accept-Language`, then English fallback.
13. Keep the Docker runtime manifest synchronized with application imports and non-Python runtime assets. Top-level Python modules are copied with `COPY *.py ./`; asset directories such as `locales/`, `templates/`, and `static/` require explicit copies. A missing `i18n.py` previously made Gunicorn workers fail at boot, and omitting `locales/` would fail catalog loading next.
14. CAPTCHA is a server-side security boundary, not a trusted browser flag. Keep CAPTCHA secret keys write-only and encrypted, expose only the active provider's public site key, verify every required token against the provider's fixed Siteverify endpoint, and fail closed on missing configuration or provider errors. Validate the returned hostname and the Turnstile action. Never derive the expected hostname from `Host`, `X-Forwarded-Host`, or `request.url_root`, which the client controls: compare against the configured `captcha_hostname`. Saving settings that enable a provider without a hostname is rejected; legacy configurations without one skip only the hostname comparison and log a warning. In the browser, wait until the provider's `render` function exists, not just its global (reCAPTCHA creates a stub first). Retain rate limits because CAPTCHA does not replace request throttling.
15. Appearance is an account preference, not a global panel setting. Preserve `system`, `light`, and `dark` as the accepted values, render the authenticated preference on the initial HTML response to avoid a theme flash, and recreate theme-sensitive CAPTCHA widgets when the resolved scheme changes.
16. Translation caches are keyed by 24-character lowercase hexadecimal content hashes with string values. If a file-backed cache is ever reintroduced, treat it as untrusted input: load it through a file handle, validate keys and values, and stream JSON to the already-selected file handle rather than passing serialized content to `Path.write_text` (Sonar rule `pythonsecurity:S2083`).
17. Keep the direct `python webapp.py` development server bound to a loopback address. Network-facing container access belongs to the production Gunicorn command in `Dockerfile`; do not expose Flask's development server on every interface.
18. Redis is a disposable read cache. Every settings or job mutation must bump its `cache_revisions` marker in the same SQL transaction, including ownership changes and startup recovery. Cache raw locale-neutral rows, scope job keys by account/admin view, and keep authentication, quotas, mutations, and download authorization in SQL. Never make Redis invalidation or availability a prerequisite for correctness.
19. Send transactional email through `email_delivery.send_email`, keeping credentials deployment-only and provider responses private. Preserve legacy SMTP selection when `EMAIL_PROVIDER` is blank, require TLS, and never automatically retry or switch providers after an ambiguous send failure. Provider additions need offline transport tests plus `.env.example`, Compose, and documentation updates. Keep MFA limits and challenge consumption in SQL regardless of the selected transport.
20. Per-address limits and CAPTCHA `remoteip` use `request.remote_addr`. Behind a reverse proxy that is the proxy's address unless `TRUSTED_PROXY_COUNT` is set to the exact proxy count (Werkzeug `ProxyFix`). Increment failed-login counters with a single SQL UPDATE, never read-modify-write, so parallel guesses cannot under-count.
21. `db_lock` is a `threading.Lock` and protects nothing across Gunicorn workers, and SQLite's deferred transactions take no lock until the first write. Every check-then-act on shared state (final-admin count, login attempt budget, job status transitions) must either be one conditional `UPDATE` whose rowcount is checked, or run after a serializing write such as the shared settings-row update used by quotas and user administration. Reserve a login attempt atomically before the slow password hash, and make the success reset conditional on the lock state observed before verification.
22. The cookie-refresh hook in `security_headers` may re-issue a token only when the user is active, the presented `ver` equals the current `token_version`, and neither the `jti` nor the session id (`sid`) is revoked. Re-issued tokens keep their `ver` and `sid`. Logout revokes the `sid` (stored as `sid:<id>` in `revoked_tokens`) so earlier refreshed tokens of that session die too. Skip refresh on login, registration, setup, and any response that already sets the access cookie.
23. Schema creation, migrations, and interrupted-job recovery run once in the Gunicorn master through the `gunicorn.conf.py` `on_starting` hook, before workers fork; workers inherit `SUBTITLE_TRANSLATOR_STARTUP_RECOVERED=1` and skip recovery. Recovery in a worker would fail jobs that sibling workers are still running. Startup steps must still be idempotent under concurrent boots (`retry_database_race`).
24. Translate masked text, not raw markup: wrap lines while tags are still single zero-width sentinel tokens, detect speaker dashes after leading sentinels, never send segments with no text besides tags, and reject replies whose placeholder multiset differs from the source so the existing retry and fallback path handles them. Provider transport errors (timeouts, `http.client.HTTPException`, non-JSON bodies) must become retryable `TranslationError`; only `RateLimitError` and `FatalTranslationError` may escape the per-line fallback.
25. Validate an upload's extension on the original filename and build its display name with `upload_display_name`, which keeps Unicode. `secure_filename` strips all non-ASCII characters and rejected CJK and Cyrillic names. Files on disk keep the fixed `source.<ext>` name.
26. MySQL rejects literal defaults on `TEXT`/`BLOB`/`JSON` columns; use parenthesized expression defaults such as `text("('')")` or Python-side defaults. A schema test enforces this for the MySQL dialect.

## Validation

Run the narrowest relevant checks while iterating, then run the full offline suite before handoff:

```text
python -m pytest -q
```

CI also runs `python -m ruff check .` (configuration in `pyproject.toml`, version pinned in `requirements-dev.txt`); run it before handoff.

Use the module form in this Windows workspace. The standalone `pytest` launcher has resolved imports through the stale sibling path `D:\Dev\SRT-Translate` and produced false `ModuleNotFoundError` collection failures. The local Python lacks `boto3` and `ruff`; run the suite with `PYTHONPATH=.test-deps` (an ignored local dependency folder) and run ruff where it is installed.

Race conditions are invisible to the single-process test client. For changes to job transitions, authentication counters, or startup, also reproduce with concurrent requests against a multi-worker Gunicorn (`--workers 4`, bound to loopback) on a fresh SQLite `DATA_DIR`.

For web-job tests, wait for a terminal job state and include `job["error"]` in failure output. For CI edits, verify these cases separately:

- no Docker Hub configuration: the metadata image list contains only `ghcr.io/<owner>/<repo>`;
- all Docker Hub settings configured: the list contains the normalized Docker Hub name and GHCR name;
- partially configured Docker Hub: publishing is disabled with a warning, without emitting an empty image entry;
- an invalid configured Docker Hub repository name fails early with a useful error.

Do not make live paid-provider calls as routine validation. Use Echo unless the user explicitly requests an integration test and supplies authorization and credentials through a safe channel.

Do not import `webapp` merely as a smoke test without first assigning `DATA_DIR` to a temporary writable directory: importing the module initializes SQLite immediately. The actual web tests already set this environment variable before import.
