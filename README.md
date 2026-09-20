# Subtitle Translator

A self-hosted web app and command-line tool for translating subtitle files with LLM APIs, DeepL, or Google Cloud Translation while preserving timings, dialogue structure, positioning, and inline styling.

## Highlights

- SRT, WebVTT (`.vtt`), Advanced SubStation Alpha (`.ass`), and SubStation Alpha (`.ssa`)
- Anthropic, OpenAI and OpenAI-compatible endpoints, DeepL, Google Cloud Translation, plus a debug-only offline Echo test provider
- Multi-file uploads and multiple target languages per job
- Background job queue, batch-level progress, manual cancellation, per-language downloads, ZIP bundles, and job deletion
- Multi-user JWT login with administrator and user roles
- Optional per-account MFA with authenticator apps, email codes, and single-use recovery codes
- Extensible transactional email delivery through SMTP, AWS SES, Aliyun DirectMail, or Resend
- Self-service user registration with administrator control
- Server-verified Cloudflare Turnstile, Google reCAPTCHA v2, or hCaptcha protection for login, registration, and uploads
- Localized web interface with browser-language detection and a persistent language selector
- Light, dark, and device-matched themes with a preference saved to each account
- Administrator-only user management and encrypted, write-only provider keys
- Per-user job isolation, with administrator access to cross-user job cleanup
- SQLite by default, plus PostgreSQL and MariaDB/MySQL through SQLAlchemy
- Optional Redis cache for settings and frequently polled job lists/details, with SQL fallback
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

Browser sessions use short-lived signed JWTs in HttpOnly, `SameSite=Strict` cookies. State-changing browser requests also require the JWT-bound double-submit CSRF token. Non-browser clients can post the normal login fields plus `"token_transport": "header"`; after all required authentication steps succeed, the response returns `access_token`, which protected APIs accept as `Authorization: Bearer <JWT>` without browser CSRF. Logout revokes the current token; disabling a user or changing a password/role invalidates all of that user's existing tokens.

Set a strong, stable `JWT_SECRET_KEY`; Compose refuses to start without it. Provider and CAPTCHA secrets saved through the administrator UI are encrypted with Fernet before database storage and are never returned by `GET /api/settings`. By default the encryption key is derived separately from `JWT_SECRET_KEY`. For independent JWT-key rotation, set a stable `API_KEY_ENCRYPTION_KEY` before saving secrets. Losing or changing the encryption key makes saved secrets unreadable.

Set `JWT_COOKIE_SECURE=1` whenever the app is served over HTTPS. Plain `http://localhost` needs `0`. For any network deployment, terminate TLS at the app or a trusted reverse proxy; secure cookies and JWT authentication do not encrypt HTTP traffic.

Administrators can manage provider settings, create/disable/promote/delete users, reset passwords, unlock accounts, and select **All users** when inspecting or deleting jobs. Regular users can operate only their own jobs. The final active administrator cannot be deleted, disabled, or demoted.

Self-registration is enabled by default and creates regular-user accounts only. An administrator can disable it under **Settings → Registration and CAPTCHA**. The initial setup flow remains separate: the first account must be the administrator created by the one-time setup screen or `ADMIN_PASSWORD` bootstrap.

The same settings section can enable one CAPTCHA provider and independently protect login, registration, and upload submissions. Supported providers are [Cloudflare Turnstile](https://developers.cloudflare.com/turnstile/), [Google reCAPTCHA v2](https://developers.google.com/recaptcha/docs/display), and [hCaptcha](https://docs.hcaptcha.com/). Enter the provider's public site key and private secret key, then set the expected public hostname if the request hostname seen by Flask is not the hostname registered with the CAPTCHA provider. Secret keys are write-only and encrypted like translation-provider keys. Every challenge token is checked server-side, including its hostname and (for Turnstile) action; provider errors fail closed. Keep the existing request limits enabled too, because CAPTCHA complements rather than replaces rate limiting.

CAPTCHA can instead be bootstrapped with `CAPTCHA_PROVIDER`, `CAPTCHA_HOSTNAME`, the three `CAPTCHA_ON_*` switches, and the matching `*_SITE_KEY` / `*_SECRET_KEY` variables shown in `.env.example`. `CAPTCHA_PROVIDER=none` disables all CAPTCHA checks. Use HTTPS in production and restrict each widget key to the deployment's real hostname in its provider dashboard.

### Multi-factor authentication (MFA)

Open **Account security** after signing in. Each account, including administrators, can opt into one second-factor method at a time:

- **Authenticator app (recommended):** enter your current password, scan the locally generated QR code with Google Authenticator, Microsoft Authenticator, or another TOTP app, and enter its six-digit code. Manual setup keys are also provided. Uses standard SHA-1 TOTP with six digits and a 30-second period; the server accepts one adjacent time step for clock skew and rejects already-used steps. Keep the server and phone clocks synchronized.
- **Email verification:** enter your password and an email address, then verify the six-digit code sent to that address. Email MFA becomes available when an email delivery provider is configured. Its protection depends on the security of the mailbox; use a separate mailbox password and enable MFA there too.

MFA remains disabled until enrollment is confirmed. Email codes and login challenges expire after **5 minutes**; pending authenticator enrollment expires after **10 minutes**. Codes are single-use. Email sends have a **60-second per-account cooldown** and a **10-per-hour per-account limit**, shared by enrollment, login, and account-security actions. Five failed password/code checks in MFA flows lock those flows for **15 minutes** and invalidate outstanding challenges. This failure budget persists across new login challenges, processes, and restarts. Existing password-login and CAPTCHA protections still apply.

Enabling MFA shows **10 recovery codes once**, with a download button. Keep them outside the browser in a safe place. Each recovery code replaces the second factor once; it does not replace your password. Recovery works even if email delivery is down. You can replace recovery codes or disable MFA under **Account security**, using your password plus an unused current-factor or recovery code. To switch methods or change the MFA email address, disable the existing method with both factors, then enroll again. Changes invalidate other access JWTs and all outstanding challenges, and refresh the current session. Password resets by administrators do not remove MFA. There is no password-only or administrator API bypass for a lost second factor: retain recovery codes, especially for the final administrator.

Email delivery is selected with `EMAIL_PROVIDER` in the deployment environment (`.env` for Compose). Set `EMAIL_FROM` to your sender address, configure the selected provider, then recreate the application container. Credentials remain deployment-only and never appear in the settings API or browser.

| `EMAIL_PROVIDER` | Required provider configuration |
| --- | --- |
| `smtp` | `SMTP_HOST`; optional authentication with `SMTP_USERNAME` / `SMTP_PASSWORD` |
| `ses` | `AWS_SES_REGION` and AWS SDK credentials or an IAM role |
| `aliyun` | `ALIYUN_DM_REGION`, `ALIYUN_ACCESS_KEY_ID`, `ALIYUN_ACCESS_KEY_SECRET` |
| `resend` | `RESEND_API_KEY` |
| `none` | Explicitly disables email delivery |

Existing `SMTP_HOST` / `SMTP_FROM` deployments continue to work with a blank or unset `EMAIL_PROVIDER`. `EMAIL_FROM` takes precedence over `SMTP_FROM` for SMTP. SMTP requires certificate-verified `starttls` or `ssl`; a blank `SMTP_PORT` selects 587 or 465 respectively. The other providers use their HTTPS APIs. See [Email delivery configuration and provider extensions](docs/email-delivery.md) for examples, cloud permissions, and the adapter contract.

Authenticator MFA needs no email provider or external QR service. Existing deployments gain the MFA tables automatically; existing accounts keep password-only login until they enroll. MFA reads, locking, and counters use SQL directly, independently of Redis. Back up the database and stable encryption key together: authenticator secrets use the existing `enc:v1:` Fernet encryption. OTP codes use keyed hashes and recovery codes use hashes; neither is stored in plaintext.

**API login with MFA:** `POST /api/auth/login` returns `mfa_required: true`, `method`, `challenge_token`, and `expires_in` after a correct password, with no access token or new login cookie. Submit `{"challenge_token": "...", "code": "..."}` to `POST /api/auth/mfa/verify`. Successful verification returns the normal user response and access cookie, or `access_token` if the original login requested header transport. Challenge JWTs use a separate signing key and audience and cannot authorize application APIs. `POST /api/auth/mfa/resend` accepts an email challenge and returns a replacement challenge token; use the new token because the old token/code pair is invalidated. A delivery failure returns `email_sent: false` and a warning while allowing recovery-code verification. A new password login supersedes the previous login challenge, so use resend while remaining on the verification screen.

Authenticated MFA endpoints are `GET /api/auth/mfa`, `POST /api/auth/mfa/setup` (`password`, `method`, and `email` for email enrollment), `/confirm` (`challenge_token`, `code`), `/email` (`password`, for a management email code), `/disable`, and `/recovery` (`password`, `code`, and a management `challenge_token` for email codes). Cookie-authenticated mutations require the usual CSRF header. Enrollment, disabling, and recovery-code replacement return a fresh session; bearer clients must replace their old access token. Only `/verify` and `/resend` operate without an access JWT, and both require a valid, unexpired, narrowly scoped challenge JWT.

### Interface languages

The web interface supports English, Traditional Chinese, and Simplified Chinese. It chooses a language from an explicit `?lang=` selection, the saved `ui_locale` cookie, or the browser's `Accept-Language` header, in that order. `zh-Hant` variants select Traditional Chinese, while `zh`, `zh-Hans`, `zh-CN`, and `zh-SG` select Simplified Chinese. Use the language selector in the lower-right corner to save a preference. Unsupported or missing strings fall back to English.

Localization catalogs live in `locales/*.json`. Each catalog declares its locale code, display name, aliases, and translated messages; adding a valid catalog makes that locale available automatically. Stored job states remain locale-neutral so the same job can be rendered in each viewer's selected language. Technical provider and subtitle-parser errors remain unchanged to preserve their diagnostic details.

### Appearance

Signed-in users can choose **System theme**, **Light**, or **Dark** from the appearance selector in the top bar. The choice is stored on the user account, so it follows that account to other browsers and devices. **System theme** follows the device's `prefers-color-scheme` setting; signed-out and first-time views also use the system theme.

The Settings portal also controls translation-submission rate limits. Regular-user and administrator limits apply independently to each account, while the panel-wide limit covers all accounts. The shared window is configurable from 1 minute to 7 days; `0` disables an individual limit. Each uploaded subtitle counts as one job, and a multi-file request is accepted or rejected as a unit. Counters are stored in the application database so limits remain effective across restarts and multiple web workers.

Administrators can also set **daily, weekly, and monthly translation limits** in **Settings → Submission rate limits**, separately for each regular-user account, each administrator account, and the whole panel. These limits apply together with the configurable submission window; every applicable limit must allow the complete upload. All new limits default to `0` (unlimited).

Calendar periods use **UTC**: daily limits reset at midnight, weekly limits reset on Monday at midnight, and monthly limits reset on the first day of the next calendar month (not after 30 days). Each accepted subtitle file consumes one job regardless of its number of target languages. Failed, canceled, and deleted jobs still count. Rejected uploads consume no quota. Counters begin when this version is installed; older jobs are not backfilled. Usage is recorded even while a limit is unlimited, and saving or changing settings never clears calendar usage. Changing the existing configurable-window settings starts fresh counters for that window only.

The settings API exposes `user_daily_job_limit`, `user_weekly_job_limit`, `user_monthly_job_limit`, and the equivalent `admin_` and `panel_` keys. Values must be whole numbers from 0 to 100,000 for account limits or 0 to 1,000,000 for panel limits. Exceeded limits return HTTP 429 with `scope`, `period`, `limit`, `used`, `requested`, `reset_at`, and a `Retry-After` header. If several limits are exceeded, the response describes the one with the latest reset. A batch larger than the limit must be reduced before retrying.

### Database backends

SQLite remains the zero-configuration default at `/app/data/app.db`. Set `DATABASE_URL` for another backend:

```text
postgresql://subtitle:password@postgres/subtitle
mariadb://subtitle:password@mariadb/subtitle?charset=utf8mb4
mysql://subtitle:password@mysql/subtitle?charset=utf8mb4
```

Ordinary PostgreSQL, MariaDB, and MySQL URLs are normalized to the bundled `pg8000` and `PyMySQL` drivers. URL-encode special characters in credentials. The job files still live below `DATA_DIR`; changing the relational database does not move uploads or translated outputs to object storage.

### Redis caching

Docker Compose includes a private Redis service and enables caching by default. It has no published port, uses an internal network, and caps cached data at 128 MB with LRU eviction. Persistence is disabled because every cached value can be reconstructed from SQL. Redis startup or downtime does not block application startup; reads fall back to SQL with 250 ms connection/command timeouts and a five-second retry cooldown.

| Data | Storage and cache behavior |
|---|---|
| Panel settings, provider configuration, registration/CAPTCHA configuration | SQL remains authoritative; Redis caches the stored settings snapshot. Responses still mask secrets and apply the current request's language. |
| Job lists and detail polling | Redis caches raw rows, scoped to the requesting account or explicit administrator view and list limit. Stages are localized after retrieval. |
| Accounts, roles, token versions, logout revocations | Read directly from SQL so authentication changes take effect immediately. |
| Job submission quotas, cancellation, deletion, download authorization | Enforced directly in SQL; quota counters and job inserts remain in one transaction. |
| Uploads, translated outputs, resumable translation caches | Remain in the persistent data volume. |

Settings and job writes change a random revision in the new `cache_revisions` SQL table **in the same transaction**. Cache reads check that revision before looking up Redis. This replaces full result queries with a small primary-key lookup on cache hits, and prevents an old cache value from being reused after a committed change, even if Redis was unavailable during the write. An in-flight reader can finish with its pre-change snapshot. All job changes invalidate the job cache; active workloads with frequent progress updates may therefore have fewer cache hits. No user/job/settings data is migrated out of SQL.

Cache payloads are encrypted and authenticated using the application's existing Fernet key, and bound to their cache key. Plaintext secrets and subtitle metadata are not stored in Redis. Keep the existing JWT/encryption keys stable. Entries expire after `REDIS_CACHE_TTL` seconds (default `60`, accepted range `1`–`3600`); eviction, expiry, invalid payloads, and Redis failures all cause database reads. Do not edit cached SQL tables directly while the app is running: maintenance scripts must also update the relevant revision in the same transaction, or restart the app afterward.

Set these variables in `.env` for Compose, or export them for a non-Docker process:

- `REDIS_URL`: Compose defaults to `redis://redis:6379/0`. An explicitly blank value disables caching; outside Compose, unset also disables it. For an external service, use its authenticated `redis://` or TLS `rediss://` URL.
- `REDIS_CACHE_TTL`: expiration in seconds.
- `REDIS_KEY_PREFIX`: default `subtitle-translator`; choose a distinct value for independent deployments sharing Redis. Keys also include a hash of the database URL.

Apply an upgrade with `docker compose up -d --build`; startup creates the revision table and retains existing records. Redis needs no backup; continue backing up the SQL database, data volume, and encryption key. To run only the app with caching disabled, set `REDIS_URL=` and use `docker compose up -d --build subtitle-translator` (stop an existing Redis container with `docker compose stop redis` if desired). This cache does not change the in-process job executor: retain the Dockerfile's single Gunicorn worker.

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
