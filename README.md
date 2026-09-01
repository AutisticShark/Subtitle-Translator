# Subtitle Translator

A self-hosted web app and command-line tool for translating subtitle files with LLM APIs, DeepL, or Google Cloud Translation while preserving timings, dialogue structure, positioning, and inline styling.

## Highlights

- SRT, WebVTT (`.vtt`), Advanced SubStation Alpha (`.ass`), and SubStation Alpha (`.ssa`)
- Anthropic, OpenAI and OpenAI-compatible endpoints, DeepL, Google Cloud Translation, plus a debug-only offline Echo test provider
- Multi-file uploads and multiple target languages per job
- Background job queue, batch-level progress, manual cancellation, per-language downloads, ZIP bundles, and job deletion
- Multi-user JWT login with administrator and user roles
- Self-service user registration with administrator control
- Server-verified Cloudflare Turnstile, Google reCAPTCHA v2, or hCaptcha protection for login, registration, and uploads
- Localized web interface with browser-language detection and a persistent language selector
- Light, dark, and device-matched themes with a preference saved to each account
- Administrator-only user management and encrypted, write-only provider keys
- Per-user job isolation, with administrator access to cross-user job cleanup
- SQLite by default, plus PostgreSQL and MariaDB/MySQL through SQLAlchemy
- Context-aware batching, shared rate-limit backoff, retries, resumable per-job cache, tag masking, and subtitle-aware line wrapping
- Docker health check, persistent named volume, CSRF-protected HttpOnly auth cookies, and bearer-token API support
- The original CLI remains available

## Start with Docker Compose

```bash
cp .env.example .env
# Generate and set JWT_SECRET_KEY in .env before starting.
docker compose up --build -d
```

Open <http://localhost:8000>. If `ADMIN_PASSWORD` was left blank, the one-time setup screen creates the first administrator. Complete that setup before exposing the service to an untrusted network; for unattended deployment, set `ADMIN_PASSWORD` before the first start instead. Sign in, choose **Settings**, add a provider key and model, then upload subtitles. Application settings, users, job records, sources, outputs, and resumable caches live in the `subtitle_data` Docker volume.

Queued or processing jobs can be canceled from **Recent jobs**. Finished, failed, and canceled jobs can then be deleted; deletion removes the database record, uploaded source, translated outputs, ZIP bundle, and resumable cache.

Useful commands:

```bash
docker compose logs -f
docker compose down
docker compose down -v  # also permanently deletes saved settings and jobs
```

`PORT`, `JOB_WORKERS`, and `MAX_UPLOAD_MB` can be changed in `.env`. API keys can optionally be bootstrapped with environment variables; values saved through the UI take precedence. Set `FLASK_DEBUG=1` only on a development instance to expose the offline Echo provider in the web UI and API. `APP_PASSWORD` remains a deprecated compatibility input: on an empty database it bootstraps the `admin` account, but `ADMIN_PASSWORD` is preferred.

### Authentication and secrets

Browser sessions use short-lived signed JWTs in HttpOnly, `SameSite=Strict` cookies. State-changing browser requests also require the JWT-bound double-submit CSRF token. Non-browser clients can post the normal login fields plus `"token_transport": "header"`; the response returns `access_token`, which protected APIs accept as `Authorization: Bearer <JWT>` without browser CSRF. Logout revokes the current token; disabling a user or changing a password/role invalidates all of that user's existing tokens.

Set a strong, stable `JWT_SECRET_KEY`; Compose refuses to start without it. Provider and CAPTCHA secrets saved through the administrator UI are encrypted with Fernet before database storage and are never returned by `GET /api/settings`. By default the encryption key is derived separately from `JWT_SECRET_KEY`. For independent JWT-key rotation, set a stable `API_KEY_ENCRYPTION_KEY` before saving secrets. Losing or changing the encryption key makes saved secrets unreadable.

Set `JWT_COOKIE_SECURE=1` whenever the app is served over HTTPS. Plain `http://localhost` needs `0`. For any network deployment, terminate TLS at the app or a trusted reverse proxy; secure cookies and JWT authentication do not encrypt HTTP traffic.

Administrators can manage provider settings, create/disable/promote/delete users, reset passwords, unlock accounts, and select **All users** when inspecting or deleting jobs. Regular users can operate only their own jobs. The final active administrator cannot be deleted, disabled, or demoted.

Self-registration is enabled by default and creates regular-user accounts only. An administrator can disable it under **Settings → Registration and CAPTCHA**. The initial setup flow remains separate: the first account must be the administrator created by the one-time setup screen or `ADMIN_PASSWORD` bootstrap.

The same settings section can enable one CAPTCHA provider and independently protect login, registration, and upload submissions. Supported providers are [Cloudflare Turnstile](https://developers.cloudflare.com/turnstile/), [Google reCAPTCHA v2](https://developers.google.com/recaptcha/docs/display), and [hCaptcha](https://docs.hcaptcha.com/). Enter the provider's public site key and private secret key, then set the expected public hostname if the request hostname seen by Flask is not the hostname registered with the CAPTCHA provider. Secret keys are write-only and encrypted like translation-provider keys. Every challenge token is checked server-side, including its hostname and (for Turnstile) action; provider errors fail closed. Keep the existing request limits enabled too, because CAPTCHA complements rather than replaces rate limiting.

CAPTCHA can instead be bootstrapped with `CAPTCHA_PROVIDER`, `CAPTCHA_HOSTNAME`, the three `CAPTCHA_ON_*` switches, and the matching `*_SITE_KEY` / `*_SECRET_KEY` variables shown in `.env.example`. `CAPTCHA_PROVIDER=none` disables all CAPTCHA checks. Use HTTPS in production and restrict each widget key to the deployment's real hostname in its provider dashboard.

### Interface languages

The web interface supports English, Traditional Chinese, and Simplified Chinese. It chooses a language from an explicit `?lang=` selection, the saved `ui_locale` cookie, or the browser's `Accept-Language` header, in that order. `zh-Hant` variants select Traditional Chinese, while `zh`, `zh-Hans`, `zh-CN`, and `zh-SG` select Simplified Chinese. Use the language selector in the lower-right corner to save a preference. Unsupported or missing strings fall back to English.

Localization catalogs live in `locales/*.json`. Each catalog declares its locale code, display name, aliases, and translated messages; adding a valid catalog makes that locale available automatically. Stored job states remain locale-neutral so the same job can be rendered in each viewer's selected language. Technical provider and subtitle-parser errors remain unchanged to preserve their diagnostic details.

### Appearance

Signed-in users can choose **System theme**, **Light**, or **Dark** from the appearance selector in the top bar. The choice is stored on the user account, so it follows that account to other browsers and devices. **System theme** follows the device's `prefers-color-scheme` setting; signed-out and first-time views also use the system theme.

The Settings portal also controls translation-submission rate limits. Regular-user and administrator limits apply independently to each account, while the panel-wide limit covers all accounts. The shared window is configurable from 1 minute to 7 days; `0` disables an individual limit. Each uploaded subtitle counts as one job, and a multi-file request is accepted or rejected as a unit. Counters are stored in the application database so limits remain effective across restarts and multiple web workers.

### Database backends

SQLite remains the zero-configuration default at `/app/data/app.db`. Set `DATABASE_URL` for another backend:

```text
postgresql://subtitle:password@postgres/subtitle
mariadb://subtitle:password@mariadb/subtitle?charset=utf8mb4
mysql://subtitle:password@mysql/subtitle?charset=utf8mb4
```

Ordinary PostgreSQL, MariaDB, and MySQL URLs are normalized to the bundled `pg8000` and `PyMySQL` drivers. URL-encode special characters in credentials. The job files still live below `DATA_DIR`; changing the relational database does not move uploads or translated outputs to object storage.

## Run without Docker

Python 3.11 or newer is recommended.

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate
# macOS/Linux: source .venv/bin/activate
pip install -r requirements.txt
python webapp.py
```

The app listens on `http://localhost:8000` and creates `data/` on first start. Set `JWT_SECRET_KEY` in the environment before saving provider keys. The first browser visit offers one-time administrator setup unless `ADMIN_PASSWORD` bootstrapped the account.

## Providers

| Provider | Web settings | Notes |
|---|---|---|
| Anthropic | API key, model | Uses the Messages API |
| OpenAI-compatible | API key, model, base URL | Works with OpenAI and compatible `/v1/chat/completions` servers |
| DeepL | API key | Automatically selects free or paid API by the `:fx` key suffix |
| Google Cloud Translation | API key | Uses Cloud Translation - Basic (v2) with the standard NMT model |
| Echo | None | Offline pipeline test; available in the web app only with `FLASK_DEBUG=1` |

The language list is shared by the CLI and web app. DeepL and Google Cloud Translation must support the selected target; the LLM providers can use every target shown in the UI. For Google, enter a supported source language code (such as `en`) or a known language name; otherwise the API automatically detects the source language. Echo remains available to the CLI for offline pipeline checks without enabling web debug mode.

For Google Cloud Translation, enable the Cloud Translation API in a Google Cloud project and create an API key. Save it in the web Settings screen or set `GOOGLE_API_KEY`. The integration uses the API-key-compatible Basic v2 endpoint; it does not require a service-account credential file.

## CLI

Existing usage is preserved, with OpenAI-compatible support added:

```bash
python srt_translate.py episode.srt --provider anthropic --langs zh-TW,ja
python srt_translate.py episode.srt --provider deepl --langs de
python srt_translate.py episode.srt --provider google --langs zh-TW --api-key @~/.google-key
python srt_translate.py episode.srt --provider openai --model gpt-5-mini \
  --base-url https://api.openai.com/v1
python srt_translate.py episode.srt --provider echo
```

The CLI currently writes SRT. Use the web app for VTT, ASS, and SSA.

## Development and tests

```bash
pip install -r requirements-dev.txt
python -m pytest -q
```

The `/healthz` endpoint is unauthenticated for container and reverse-proxy health checks. `/api/i18n` and the authentication setup-status, setup, login, and registration routes are the intentional public API boundaries; setup-status exposes only registration state and the active CAPTCHA provider's public widget configuration. All application data APIs are authenticated. Uploaded filenames are sanitized, stored under random job IDs, associated with an owning user, and downloads are restricted to both the owner/administrator and registered output files.

## Publishing Docker images

The [Docker publishing workflow](.github/workflows/docker-publish.yml) publishes the same multi-architecture (`linux/amd64` and `linux/arm64`) image to Docker Hub and GitHub Container Registry:

- Every push to `main` updates the `dev` tag.
- Every Git tag matching `v*` publishes that exact tag, such as `v1.0.0`.
- The workflow can also be run manually; running it from `main` publishes `dev`.

Docker Hub publishing is optional. To enable it, configure all three of these under **Repository Settings → Secrets and variables → Actions**:

| Type | Name | Value |
|---|---|---|
| Variable | `DOCKERHUB_USERNAME` | Docker Hub account or organization name |
| Variable | `DOCKERHUB_IMAGE` | Full Docker Hub repository, for example `username/srt-translate` |
| Secret | `DOCKERHUB_TOKEN` | Docker Hub access token with write permission |

GHCR authentication uses the automatic `GITHUB_TOKEN`; the workflow grants it `packages: write`. The GHCR image name is derived from the GitHub repository and normalized to lowercase, for example `ghcr.io/owner/srt-translate:dev`.

If any Docker Hub setting is missing, the workflow reports a warning, skips Docker Hub, and continues publishing to GHCR.

Create a release image by pushing a matching tag:

```bash
git tag v1.0.0
git push origin v1.0.0
```
