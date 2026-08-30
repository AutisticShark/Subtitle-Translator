# Project memory

## Retrospective scope

Created on 2026-08-29 from the repository history available in this workspace. No earlier chat transcript, agent scratchpad, or pre-existing memory file was present, so this file records every prior-session mistake that can be substantiated from commits and the current test surface. It intentionally does not invent unverified failures.

## Confirmed mistakes and fixes

### Async event target used after `await`

- Evidence: commit `df532a4` (`fix: event.currentTarget becomes null after await`).
- Affected code: `submitTranslation` and `saveSettings` in `static/app.js`.
- Mistake: the handlers accessed `event.currentTarget` after awaiting API work. The event object's `currentTarget` is meaningful only while the listener is being invoked and may be reset to `null` after the handler yields.
- User-visible result: successful translation submission or settings saving could fail during post-request form access/cleanup, despite the API request itself completing.
- Fix: capture `const form = event.currentTarget` before the first `await`, and use `form` for `FormData`, queries, and cleanup.
- Prevention: in every async listener, capture any required event-derived DOM references at entry. During review, search the handler for event-property reads after its first `await`.

### Empty optional Docker image passed to metadata generation

- Evidence: commit `3237715` (`fix(ci): allow empty dockerhub_image metadata`).
- Affected code: `.github/workflows/docker-publish.yml`.
- Mistake: the workflow always supplied a Docker Hub image declaration to `docker/metadata-action`, relying on `enable=false` when Docker Hub configuration was absent. The name itself could be empty and still be parsed/validated by the action.
- User-visible result: the supposedly optional Docker Hub configuration could break the publishing workflow instead of allowing GHCR-only publishing.
- Fix: the image-resolution step now emits one `all` output. It contains GHCR alone when Docker Hub is disabled, or a multiline list of the two complete image names when enabled.
- Prevention: omit absent optional values entirely from third-party action input. Test the absent, partial, complete, and invalid-configuration branches conceptually or in CI.

## Validation gap exposed by those mistakes

The Python test suite does not execute `static/app.js` in a browser and does not evaluate GitHub Actions or `docker/metadata-action`. Both regressions therefore sit outside `pytest` coverage. Passing `python -m pytest -q` remains required, but it must not be treated as sufficient validation for frontend event-lifecycle or publishing-workflow changes.

## Validation errors observed while creating this memory

### Standalone `pytest` launcher used a stale project path

- Evidence: running `pytest -q` on 2026-08-29 failed collection with `ModuleNotFoundError` for `srt_translate` and `webapp`; its traceback referred to the old sibling path `D:\Dev\SRT-Translate`.
- The same suite passed immediately with `python -m pytest -q` from `D:\Dev\Subtitle-Translator` (7 tests passed).
- Prevention: use `python -m pytest -q` in this workspace so the active interpreter starts with the current directory on its import path.

### Imported `webapp` without isolating its runtime data

- Evidence: a diagnostic `python -c` import on 2026-08-29 raised `sqlite3.OperationalError: attempt to write a readonly database`.
- Mistake: `webapp` was imported without first pointing `DATA_DIR` at a temporary writable directory. Module import calls `init_storage()` and therefore is not side-effect free.
- Prevention: set `DATA_DIR` before importing `webapp` in tests or diagnostics. Prefer the existing test harness, which does this correctly, instead of importing against the repository's real `data/` directory.

### Explicit CRLF text was translated twice on Windows

- Evidence: the expanded SRT writer tests on 2026-08-29 found that `write_srt(..., crlf=True)` emitted `\r\r\n` in this Windows workspace.
- Mistake: `write_srt` assembled `\r\n` separators, then used `Path.write_text` with its default platform newline handling, which translated the embedded `\n` a second time.
- User-visible result: the SRT-only CLI could write malformed line endings when preserving a CRLF source on Windows.
- Fix: pass `newline=""` when writing the already-normalized SRT text.
- Prevention: when code chooses the output newline sequence itself, disable platform newline translation and verify the raw output bytes in a focused test.

## Durable project context

- The application is a self-hosted Flask web UI plus a Python CLI for translating subtitles.
- Supported web formats are SRT, VTT, ASS, and SSA. The CLI currently writes SRT only.
- Providers are Anthropic, OpenAI-compatible APIs, DeepL, Google Cloud Translation - Basic v2, and offline Echo.
- Runtime settings, job records, uploads, outputs, and resumable caches live below `DATA_DIR` (the Docker volume maps it to `/app/data`).
- The settings API treats secrets as write-only. Preserve that security property.
- Echo is the safe, deterministic provider for the CLI and offline tests. The web UI and API expose and accept Echo only while Flask debug mode is enabled; normal instances must reject crafted Echo job requests server-side as well as hiding the option.
- Docker publishing always targets GHCR. Docker Hub is optional and requires `DOCKERHUB_IMAGE`, `DOCKERHUB_USERNAME`, and `DOCKERHUB_TOKEN` together.

## Web interface localization

- UI localization is dependency-free: `i18n.py` discovers `locales/*.json`, negotiates an explicit query selection, the `ui_locale` cookie, and `Accept-Language` in that order, and falls back to English source strings. English, Traditional Chinese, and Simplified Chinese are bundled; `zh-Hant` aliases select `zh-TW`, while generic `zh` and `zh-Hans` aliases select `zh-CN`.
- Catalog coverage tests extract static `tr(...)`/`t(...)` calls from Python, JavaScript, and Jinja sources and require every non-English catalog to cover them. Keep this check generic when adding locales instead of hard-coding one catalog.
- `/api/i18n` is intentionally public because the sign-in and one-time setup screens need dynamic browser messages before authentication. It must expose only static translations and target-language labels.
- Job state is shared data and must not be stored in the submitting user's locale. Workers keep stable English stage values; `job_dict` localizes known stage patterns for each request, allowing different viewers to use different UI locales.
- Technical provider and subtitle-parser failures remain in their original form so diagnostic details are not hidden by incomplete catalog coverage.

### Docker runtime manifest must include localization sources

- Evidence: the 2026-08-30 production log showed every Gunicorn worker exiting with code 3 at `from i18n import ...`, reporting `ModuleNotFoundError: No module named 'i18n'`.
- Cause: the Dockerfile copied an explicit pre-i18n list of Python modules and never added `i18n.py`. The new `locales/` runtime directory was also absent, which would have caused catalog initialization to fail immediately after fixing the module alone.
- Fix and prevention: copy all root Python modules with `COPY *.py ./`, copy `locales/` explicitly, and keep a deployment-manifest regression test. When adding an imported module or runtime asset directory, validate the container manifest as well as the host-side test suite.

## Google Cloud Translation adapter

- The Google provider uses the API-key-compatible Cloud Translation - Basic v2 endpoint and the standard NMT model; it does not require a service-account file.
- Google accepts at most 128 `q` strings in one request. The web batch-size maximum remains 100, while the provider also rejects oversized batches explicitly for CLI callers.
- A recognized source language name or BCP-47 code is sent as `source`; other source labels are omitted so Google can auto-detect them.
- Google returns HTML-escaped `translatedText` values. Unescape every value and require the returned translation count to equal the input count before rebuilding subtitle cues.
- `google_api_key` is write-only through the settings API and may be bootstrapped with `GOOGLE_API_KEY`, matching the existing provider-secret precedence rules.

## Progress and job-lifecycle lessons

### Progress must originate inside the batch driver

- Evidence: the web worker previously set progress to 5% after parsing, then did not update it again until an entire target-language pass completed.
- User-visible result: single-language jobs appeared stuck at 5% before jumping directly to completion.
- Prevention: keep the reusable translation engine UI-agnostic, but expose a completed/total callback at cache discovery and every completed batch. The web worker maps that fraction into the current target language's portion of the 5–95% translation range.

### Delete only terminal jobs

- Job deletion removes the isolated job directory and its SQLite record.
- Reject deletion for queued, processing, or canceling jobs so file removal cannot race a background worker that is reading sources, writing caches, or producing outputs. Completed, failed, and canceled jobs are safe to delete.

### Cancellation needs a state transition and a worker signal

- A UI-only `canceled` status is unsafe because an active worker can later overwrite it with `completed` or `failed`, and immediate deletion can race file access.
- Move active jobs atomically to `canceling`, signal a per-job in-memory event, and let the worker transition to terminal `canceled` after it stops coordinating work.
- Translation batch waits and retry/throttle sleeps must check cancellation. The batch executor abandons pending work without waiting for an in-flight HTTP request, whose provider thread may finish later but no longer writes job files or state.
- Final completion and failure updates must include a current-status condition so a concurrent cancellation wins the race.

## Authentication and database portability

- Browser authentication uses short-lived JWTs in HttpOnly `SameSite=Strict` cookies with double-submit CSRF protection. Header bearer JWTs remain available for API clients; browser code never stores an access JWT in local or session storage.
- Users own jobs. Every regular-user list, lookup, cancellation, deletion, and download query must include `user_id`; administrators may explicitly inspect and operate across owners.
- Administrators alone manage global translation settings and provider keys. Saved provider keys are write-only at the API boundary and Fernet-encrypted with the `enc:v1:` prefix in the database. A stable `JWT_SECRET_KEY` or separate `API_KEY_ENCRYPTION_KEY` is required before saving them.
- Tokens carry a user token version and are rejected when the database user is missing, disabled, or has a different version. Password, role, and activation changes increment the version; logout also records the individual JWT ID as revoked.
- The first administrator is created either by the one-time setup endpoint or `ADMIN_PASSWORD` bootstrap. The final active administrator cannot be deleted, disabled, or demoted, and setup uses a unique database marker to prevent concurrent first-admin creation.
- `database.py` uses SQLAlchemy Core and URL normalization: SQLite is the default, PostgreSQL uses `pg8000`, and MariaDB/MySQL use `PyMySQL`. The legacy SQLite migration adds nullable ownership and the first administrator claims old jobs.
- The local Python 3.15 RC has no compatible `psycopg-binary` wheel. Using the pure-Python `pg8000` driver avoids a platform-specific install failure and avoids adding `libpq` to the slim container.

## Translation submission rate limits

- Regular-user and administrator limits apply independently per account; the panel-wide limit covers all accounts. All limits share a configurable fixed window, and `0` means unlimited.
- Each uploaded subtitle consumes one job unit. A multi-file submission is admitted and inserted as one database transaction or rejected in full with HTTP 429 and a `Retry-After` header.
- Rate counters live in `rate_limit_buckets`. Job submissions and rate-setting changes first lock the `panel_job_limit` settings row, keeping counters and configuration race-safe across web workers and supported database backends. Changing any rate-limit setting starts fresh windows.

## Self-registration and CAPTCHA

- Self-registration creates active regular-user accounts only; it never participates in the one-time first-administrator setup. Administrators can disable public registration without removing their ability to create users.
- Cloudflare Turnstile, Google reCAPTCHA v2, and hCaptcha share one server-side verification boundary. Only the selected provider's site key and protected-action list are public. CAPTCHA secret keys use the existing `enc:v1:` encrypted, write-only settings path.
- Login, registration, and upload protection can be selected independently. A required token is posted only to a fixed provider verification URL with a bounded timeout. Missing keys, network failures, invalid JSON, rejected or replayed tokens, hostname mismatches, and Turnstile action mismatches all fail closed.
- CAPTCHA complements the existing login/registration request throttles and persistent translation-job quotas; it does not replace them. Browser widget rendering and third-party connectivity still require a live browser/deployment smoke test beyond the mocked offline provider tests.
