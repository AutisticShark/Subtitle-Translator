# Subtitle Translator agent guide

This file applies to the entire repository. Read `MEMORY.md` before making changes; it contains the evidence-based retrospective from earlier work. Keep both files updated when a session reveals a reusable project-specific lesson.

## Project map

- `srt_translate.py` contains the translation pipeline, provider clients (including Google Cloud Translation - Basic v2), retry/throttling behavior, segmentation, tag masking, cue rebuilding, wrapping, and the SRT-only CLI.
- `subtitle_formats.py` adapts SRT, VTT, ASS, and SSA files to and from the shared cue model. Preserve format-specific headers, timings, settings, dialogue fields, newline style, BOM state, and inline tags.
- `webapp.py` is the Flask API, authentication/authorization layer, upload/download boundary, and background job runner. `DATA_DIR` is resolved at import time.
- `database.py` defines the portable SQLAlchemy schema, SQLite legacy migration, and URL normalization for SQLite, PostgreSQL, MariaDB, and MySQL.
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
- Keep `/healthz` unauthenticated so Docker and reverse proxies can probe it.
- Sanitize uploaded filenames, isolate files under random job IDs, and only serve outputs registered to the requested job.
- Preserve subtitle structure and formatting while translating text. Add or change format behavior with focused round-trip tests.
- Keep the web app multi-format, but do not claim that the CLI writes anything except SRT unless that behavior is implemented and tested.
- Treat `data/` as persistent runtime state. Tests must set `DATA_DIR` before importing `webapp` and must not use the repository's real data directory.
- Do not commit secrets, real API keys, job data, translated media, or generated caches.

## Regression-prevention rules from prior work

1. In an async DOM event handler, capture `event.currentTarget` synchronously before the first `await`, then use the captured element afterward. `event.currentTarget` can be `null` after control returns to the event loop. This previously broke translation-form cleanup and settings-form cleanup (commit `df532a4`).
2. Do not pass a blank optional image name to `docker/metadata-action`, even with an `enable=false` fragment. Build the image list so it contains only complete, non-empty names. This previously broke GHCR-only publishing when Docker Hub variables were absent (commit `3237715`).
3. `pytest` alone does not exercise browser event lifetime or GitHub Actions expression/action parsing. For frontend async-handler changes, inspect every post-`await` event access and perform a browser smoke test when available. For publishing changes, reason through both GHCR-only and GHCR-plus-Docker-Hub branches and validate the workflow when tooling is available.
4. Translation progress must be reported from completed translation batches, not only from target-language boundaries. Map each target's batch fraction into its share of the overall job range so multi-language jobs remain monotonic.
5. Job cancellation is cooperative and race-safe: request cancellation by moving an active job to `canceling`, signal its in-memory event, and let only the worker finalize it as `canceled`. Completion and failure updates must be conditional on the current status so they cannot overwrite a concurrent cancellation.
6. Google Cloud Translation - Basic v2 accepts at most 128 strings per request and returns HTML-escaped `translatedText` values. Keep batches within that limit, unescape each value, and reject any response whose translation count differs from the input count so subtitle segments cannot be misaligned.
7. When writing explicitly assembled CRLF subtitle text, disable Python's platform newline translation (`newline=""`). Otherwise Windows converts each `\n` inside `\r\n` again and emits malformed `\r\r\n` line endings.
8. Echo is a development provider: expose and accept it in the web app only when Flask debug mode is enabled. Enforce this on the server as well as filtering the UI; Echo remains available to the CLI and offline tests.
9. JWT cookie authentication requires a CSRF header on `POST`, `PUT`, `PATCH`, and `DELETE`. Browser code reads `csrf_access_token` only for the double-submit header; it must never copy the HttpOnly access JWT into JavaScript storage.
10. Database portability requires SQLAlchemy expressions, not backend-specific placeholders or upsert syntax. Ordinary `postgresql://`, `mariadb://`, and `mysql://` URLs are normalized to installed drivers; compile schema tests for all supported dialects when changing tables.
11. Translation submission limits count jobs, not HTTP requests: every uploaded subtitle consumes one unit. Enforce the per-account role limit and the panel-wide limit in the same database transaction as all job inserts, serialize submissions through the shared settings row, and reject a multi-file upload atomically with HTTP 429 and `Retry-After`.
12. Keep stored job statuses and stages locale-neutral. Localize them while serializing the response so shared jobs can be viewed in different interface languages. Locale selection order is explicit `?lang=`, the non-sensitive `ui_locale` cookie, browser `Accept-Language`, then English fallback.
13. Keep the Docker runtime manifest synchronized with application imports and non-Python runtime assets. Top-level Python modules are copied with `COPY *.py ./`; asset directories such as `locales/`, `templates/`, and `static/` require explicit copies. A missing `i18n.py` previously made Gunicorn workers fail at boot, and omitting `locales/` would fail catalog loading next.
14. CAPTCHA is a server-side security boundary, not a trusted browser flag. Keep CAPTCHA secret keys write-only and encrypted, expose only the active provider's public site key, verify every required token against the provider's fixed Siteverify endpoint, and fail closed on missing configuration or provider errors. Validate the returned hostname and the Turnstile action. Retain rate limits because CAPTCHA does not replace request throttling.
15. Appearance is an account preference, not a global panel setting. Preserve `system`, `light`, and `dark` as the accepted values, render the authenticated preference on the initial HTML response to avoid a theme flash, and recreate theme-sensitive CAPTCHA widgets when the resolved scheme changes.

## Validation

Run the narrowest relevant checks while iterating, then run the full offline suite before handoff:

```text
python -m pytest -q
```

Use the module form in this Windows workspace. The standalone `pytest` launcher has resolved imports through the stale sibling path `D:\Dev\SRT-Translate` and produced false `ModuleNotFoundError` collection failures.

For web-job tests, wait for a terminal job state and include `job["error"]` in failure output. For CI edits, verify these cases separately:

- no Docker Hub configuration: the metadata image list contains only `ghcr.io/<owner>/<repo>`;
- all Docker Hub settings configured: the list contains the normalized Docker Hub name and GHCR name;
- partially configured Docker Hub: publishing is disabled with a warning, without emitting an empty image entry;
- an invalid configured Docker Hub repository name fails early with a useful error.

Do not make live paid-provider calls as routine validation. Use Echo unless the user explicitly requests an integration test and supplies authorization and credentials through a safe channel.

Do not import `webapp` merely as a smoke test without first assigning `DATA_DIR` to a temporary writable directory: importing the module initializes SQLite immediately. The actual web tests already set this environment variable before import.
