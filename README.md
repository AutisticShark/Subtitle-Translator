# Subtitle Translator

A self-hosted subtitle translation workspace. Go serves the API, runs translation jobs, and embeds the Svelte 5 frontend, built with Vite 8 and TypeScript 7. Python and Node.js are not needed in the production container.

Upload SRT, WebVTT, ASS, or SSA files, choose languages, and download the translated subtitles or a ZIP bundle. Timings, cue settings, speaker dialogue, inline tags, ASS styles, and format-specific metadata stay intact.

## Start with Docker Compose

```sh
cp .env.example .env
# Set JWT_SECRET_KEY in .env. Generate a value with: openssl rand -hex 64
docker compose up --build -d
```

Open <http://localhost:8000>. Create the first administrator, then open **Settings** to configure a translation provider. Set `ADMIN_USERNAME` and `ADMIN_PASSWORD` before the first start for unattended setup. Complete setup before exposing the service to untrusted users.

The `subtitle_data` volume contains the database, uploads, and outputs. `docker compose down` keeps it; **`docker compose down -v` permanently deletes it**.

- `PORT` changes the published port. The container listens on IPv4 and IPv6 where supported.
- `JOB_WORKERS` controls simultaneous files; the translation settings control API concurrency within each file.
- `MAX_UPLOAD_MB` caps each complete upload request, not each individual file.
- `APP_DEBUG=1` exposes the offline Echo provider. Keep it off in production. The previous `FLASK_DEBUG` name is still accepted.
- An optional private Redis service caches settings and job polling. SQL remains authoritative, and Redis outages do not prevent operation.

### Upgrading from the Python application

Back up the SQL database, data volume, and stable encryption key before upgrading. **Keep `JWT_SECRET_KEY`, `API_KEY_ENCRYPTION_KEY`, `DATA_DIR`, and `DATABASE_URL` unchanged.**

The Go application reads the existing schema and performs additive migrations. Existing Argon2id password hashes, Fernet-encrypted provider/CAPTCHA/TOTP secrets, users, MFA recovery codes, settings, job records, and registered output files remain usable. Existing SQLAlchemy-style driver URL prefixes are also accepted. The HTTP API retains the existing paths, cookie names, CSRF header, and bearer-token transport.

Stop the old application before starting the Go application against its database. Do not run both versions, or multiple independent server processes, against the same job storage: startup marks interrupted jobs failed rather than resuming them. A queued cancellation becomes canceled; a running cancellation finishes cooperatively. Graceful shutdown cancels active provider requests.

The container entrypoint adjusts ownership of the default `/app/data` volume for the new app user, then drops to UID/GID 1000. It does not follow symlinks when changing ownership. If you override `DATA_DIR`, or run with a custom container user, prepare that directory's ownership yourself. No Python service, Gunicorn worker setting, or frontend server is needed after the upgrade.

## Translation providers

| Provider | Configuration | Notes |
| --- | --- | --- |
| Anthropic | API key and model | Messages API; the fresh-install default model is `claude-opus-5-5` |
| OpenAI-compatible | API key, model, base URL | Any compatible `/v1/chat/completions` endpoint |
| DeepL | API key | Free endpoint selected by a `:fx` key suffix; requests split at 50 texts |
| Google Cloud Translation | API key | Basic v2; enable Cloud Translation API in the key's project |
| Echo | None | Offline development provider; rejected by the API unless debug is enabled |

Provider keys can be entered in **Settings** or bootstrapped through `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `DEEPL_API_KEY`, and `GOOGLE_API_KEY`. Saved settings take precedence over environment values. Keys are write-only: settings responses expose configured/not-configured state, never their values.

Batch-level progress updates while translation runs. A shared throttle coordinates request pacing and rate-limit backoff; transient failures are retried, while authentication failures abort. Provider redirects are not followed. A malformed translation, missing line, or altered tag placeholder is retried; exhausted recoverable failures preserve the source segment and produce a warning. Rate-limit delays above five minutes or non-finite values fail rather than parking workers indefinitely.

Input defaults to UTF-8, with selectable UTF-16, GBK, Big5, Shift JIS, Windows-1252, and ISO-8859-1. Legacy single-byte and supported multibyte codecs use Python-compatible mappings and strict invalid-byte checks. Stateful codecs such as UTF-7, HZ, ISO-2022, Johab, and JIS X 0213 are not supported; convert those files to UTF-8 first. The original UTF-8 BOM and newline style are preserved; output is always UTF-8. The interface and API preserve Unicode filenames.

## Accounts and security

The first account is an administrator. Administrators manage provider settings, users, CAPTCHA, and quotas. Regular users can read, cancel, download, and delete only their own jobs. Administrators can explicitly select **All users** in History. The final active administrator cannot be demoted, disabled, or deleted. Users with active jobs cannot be deleted.

### Sessions and secrets

Browser access tokens stay in HttpOnly, `SameSite=Strict` cookies. Mutating requests require the JWT-bound `X-CSRF-TOKEN` header. No access token is kept in browser storage. API clients can request `"token_transport": "header"` during login and use the returned token as `Authorization: Bearer <token>`.

Password resets, role changes, deactivation, and MFA changes invalidate previous tokens. Logout revokes the whole session, including earlier refreshed tokens. Cookie refresh never extends a session past `SESSION_MAX_HOURS` (default 12; 1–720). `JWT_ACCESS_MINUTES` controls token lifetime (default 30; minimum 5); bearer tokens are not refreshed.

Set `JWT_COOKIE_SECURE=1` behind HTTPS. Plain HTTP localhost needs `0`. When using a reverse proxy, set `TRUSTED_PROXY_COUNT` to the **exact** number of trusted forwarding hops; leave it at zero for direct connections. IPv4 and IPv6 client addresses both participate in request limits.

Saved secrets use the original `enc:v1:` Fernet format. A separate `API_KEY_ENCRYPTION_KEY` allows independent JWT-key rotation; without it, the encryption key is derived from `JWT_SECRET_KEY`. Losing or changing that key makes stored secrets unreadable. Without a stable key, local development still works, but secret storage and MFA enrollment are disabled.

### Two-step verification

In **Account**, enroll an authenticator app or an email address. Authenticator enrollment generates its QR code locally; no external QR service is contacted. Standard SHA-1 TOTP uses six digits, 30-second steps, and one adjacent step for clock skew. Accepted steps cannot be replayed.

Email/login challenges expire after five minutes; authenticator enrollment expires after ten. Five failed factor checks lock that flow for 15 minutes. Email sends have a shared per-account 60-second cooldown and a limit of ten per hour. Challenges, send reservations, failure budgets, and single-use recovery consumption live in SQL, not process memory or Redis.

Enrollment shows ten recovery codes once. Store them outside the browser. Replacing codes or disabling verification requires the password and an existing factor or unused recovery code. Administrative password resets never remove MFA. There is no password-only bypass for a lost factor.

SMTP, AWS SES, Aliyun DirectMail, and Resend are supported. Email credentials are deployment-only; see [email delivery](docs/email-delivery.md).

### Registration and CAPTCHA

Registration is enabled by default and creates regular-user accounts only. Administrators can disable it without losing user-management access.

Optional Cloudflare Turnstile, Google reCAPTCHA v2, and hCaptcha protection can apply independently to login, registration, and uploads. Configure the provider, site key, secret key, and expected public hostname. Tokens are verified against fixed provider endpoints; hostname and Turnstile action checks are server-side. Missing configuration or provider failure fails closed. CAPTCHA does not replace login limits or job quotas.

Only an explicitly enabled CAPTCHA loads that provider's third-party widget. The rest of the interface uses local assets and system fonts.

### Job limits

Settings provide per-user, per-administrator, and panel-wide limits for a configurable window plus UTC daily, weekly, and monthly periods. Zero means unlimited. Weeks begin Monday; months reset on the first day, not after 30 days.

Each subtitle file consumes one job, regardless of target count. Multi-file requests are admitted and charged atomically or rejected in full with HTTP 429 and `Retry-After`. Failed, canceled, and deleted jobs still count. Calendar usage is recorded even when limits are unlimited, and changing settings does not reset it. Changing configurable-window settings resets only that window's counters.

### Languages and appearance

English, 正體中文, and 简体中文 are bundled. Locale selection uses `?lang=`, then the `ui_locale` cookie, then `Accept-Language`, then English. Valid catalogs added to `locales/` are discovered automatically. Job states are stored locale-neutral and localized at response time.

System, Light, and Dark are account preferences. The initial HTML includes the saved theme, avoiding a wrong-theme flash. CAPTCHA widgets are recreated when their resolved theme changes. Hidden tabs stop polling so an unattended browser does not keep a session alive indefinitely.

## Databases and Redis

SQLite is the default at `DATA_DIR/app.db`. To use a server database, set:

```dotenv
DATABASE_URL=postgresql://user:password@postgres/subtitles?sslmode=verify-full
# Or:
DATABASE_URL=mariadb://user:password@mariadb/subtitles?charset=utf8mb4
DATABASE_URL=mysql://user:password@mysql/subtitles?charset=utf8mb4
```

URL-encode credentials containing special characters. Uploads and outputs remain in `DATA_DIR` for every backend.

PostgreSQL accepts `sslmode=disable|require|verify-ca|verify-full`; MySQL/MariaDB accept `ssl-mode=DISABLED|REQUIRED|VERIFY_CA|VERIFY_IDENTITY`. `require`/`REQUIRED` encrypt without checking the hostname or certificate, except that a supplied PostgreSQL root CA verifies the chain. Prefer `verify-full`/`VERIFY_IDENTITY` over untrusted networks. Opportunistic modes are rejected rather than silently falling back to plaintext. CA and client-certificate URL parameters remain supported.

Redis is an optional disposable read cache (`REDIS_URL`, `REDIS_CACHE_TTL`, `REDIS_KEY_PREFIX`). Every relevant mutation changes its SQL cache revision in the same transaction. Cache values are encrypted and bound to their keys; outages or invalid entries fall back to SQL. Authentication, quota checks, mutations, and download authorization always use SQL. Do not edit cached tables directly while the server is running without updating revisions or restarting it.

## Build and develop

Use Go **1.27.4** and Node.js **26.13.1**. Build the frontend before the server so its assets are embedded:

```sh
cd web
npm ci
npm run check
npm run build
cd ..
go build -trimpath -o bin/subtitle-translator ./cmd/subtitle-translator
JWT_SECRET_KEY=your-stable-secret APP_DEBUG=1 ./bin/subtitle-translator
```

The direct server binds to `127.0.0.1:8000` by default. `LISTEN_ADDR` overrides the full address; `PORT` selects a different loopback port. The Docker image sets `LISTEN_ADDR=:8000` for network access. The server does not read `.env` itself; Compose reads it and forwards the listed variables.

For frontend development, run `npm run dev` in `web/`. Vite binds to loopback and proxies `/api` to the Go server on port 8000.

The project uses the native **TypeScript 7.0.2** compiler via the `@typescript/native` npm alias. Svelte's checker also requires TypeScript 6.0.3 for its JavaScript AST tooling; `npm run check` explicitly runs `svelte-check --tsgo` and the TypeScript 7 compiler. TypeScript 6 is not the application type checker.

### Tests

```sh
go vet ./...
go test -race ./...
cd web
npm ci
npm run check
npm run build
npx playwright install chromium
npm test
```

Playwright starts a fresh Go server with a temporary SQLite database, uses Echo, and removes its test data afterward. Build `bin/subtitle-translator` first and set `TEST_BINARY=../bin/subtitle-translator` to use that binary instead of `go run`. Browser tests cover setup, provider settings, Unicode uploads, downloads, ownership, polling recovery, themes, localization, narrow screens, logout, and authenticator enrollment. No paid providers are called.

Set `TEST_DATABASE_URL` to a **disposable** PostgreSQL, MariaDB, or MySQL database to include the database integration tests. They create and remove application tables. Never point them at production. The translation engine also replays 4,950 cases generated from the Python implementation to protect format and algorithm compatibility.

`GET /healthz` is public. `/api/i18n` and the setup/login/registration boundaries expose only the data needed before sign-in. All application data APIs require authentication. CI runs native Go race tests, database integration tests, the Svelte/TypeScript checks, and real Chromium tests.

## Image publishing

The publish workflow builds `linux/amd64` and `linux/arm64` images. GHCR publishing is always enabled. Docker Hub is optional and requires all three settings: `DOCKERHUB_IMAGE`, `DOCKERHUB_USERNAME`, and `DOCKERHUB_TOKEN`. Missing or partial configuration never emits an empty metadata image; invalid repository names fail early.

Pushes to `main` publish `dev`; `v*` tags publish release versions. Publication and release tagging remain separate from local development and testing.
