# Subtitle Translator agent guide

This file applies to the entire repository. Use Git history for retired source and record reusable project-specific lessons here. Keep personal agent memory out of this repository.

## Project map

- `cmd/subtitle-translator/` is the HTTP server entrypoint and container health probe. This is a web application, not a subtitle translation CLI.
- `internal/app/` owns the API, authentication, MFA, settings, portable SQL schema/migrations, quotas, CAPTCHA, optional Redis cache, and background jobs. `Config` injects the clock, translator, email sender, and test HTTP client. `New` initializes storage and recovers interrupted jobs; `Close` cancels and finalizes workers.
- `internal/engine/` owns SRT/VTT/ASS/SSA parsing/rendering, encodings, masking, segmentation, batching, wrapping, retries, throttling, and Anthropic/OpenAI-compatible/DeepL/Google/Echo providers. It has no HTTP-server or database dependency.
- `internal/email/` owns SMTP, AWS SES v2, Aliyun DirectMail, and Resend behind `Sender`. Deployment secrets never enter SQL or the settings API.
- `web/src/` is Svelte 5 with TypeScript 7 and plain CSS. `lib/appState.svelte.ts` owns session-scoped UI state; `lib/api.ts` owns requests and CSRF. Components divide translation, job history, settings, users, accounts, authentication, and CAPTCHA.
- `assets.go` embeds the Vite build and `locales/*.json` in the Go binary. `web/dist/.gitkeep` lets Go-only tests compile in a clean checkout before the frontend is built. Production builds must build the frontend first.
- `locales/` contains English fallback, 正體中文, and 简体中文 catalogs. `assets_test.go` checks frontend/backend catalog coverage, placeholder preservation, packaging, and publishing branches.
- `internal/engine/testdata/python_golden.json.gz` preserves 4,950 cases from the Python engine. Its optional generator uses a separate pre-migration checkout; ordinary tests need no Python.
- `Dockerfile` builds Svelte assets, embeds them in a static Go binary, and runs on Alpine. `docker-entrypoint.sh` repairs ownership of the default legacy volume without following symlinks, then drops to UID/GID 1000.

## Required invariants

### Authentication and authorization

1. Never expose saved provider or CAPTCHA secrets through settings responses. Return configured/not-configured state only. Administrators alone may mutate panel/provider settings or keys.
2. Preserve `enc:v1:` Fernet compatibility. The derived key is SHA-256 of `subtitle-api-keys\0` followed by `JWT_SECRET_KEY`; `API_KEY_ENCRYPTION_KEY` may override it. Never silently replace unreadable secrets or log their values. Decrypt only the secrets needed by the current provider/action.
3. Preserve existing Argon2id PHC hashes, JWT signing/claims, cookies, and MFA data. Password length is counted in Unicode code points, not bytes. Validate untrusted hash parameters before allocating Argon2 memory.
4. All application data APIs require access authentication. `/healthz`, `/api/i18n`, setup-status/setup/login/register, narrowly scoped MFA verify/resend, and the initial HTML/assets are intentional public boundaries. Public localization and bootstrap data must contain only static catalogs, labels, and public configuration.
5. Browser access JWTs stay in HttpOnly `SameSite=Strict` cookies. Mutating cookie-authenticated requests require the JWT-bound `X-CSRF-TOKEN` header. Header bearer transport is for API clients; never store access JWTs in browser storage.
6. Every authenticated request checks SQL user activity, token version, JWT revocation, and session revocation. Deactivation, password changes, role changes, and MFA changes invalidate existing tokens. Logout revokes both the token and `sid:<id>`.
7. Refresh only an otherwise valid cookie session, preserving `ver`, `sid`, and `sst`. Never extend expiry past `sst + SESSION_MAX_HOURS`; pre-upgrade tokens use `iat`. Do not refresh login/setup/register/logout/MFA responses or overwrite a newly issued access cookie.
8. First-administrator setup and final-active-administrator protection must be safe under concurrent requests and independent processes. Administrators cannot delete, deactivate, or demote themselves. Recheck the actor inside the serializing transaction.
9. Login guesses reserve a SQL attempt before password hashing. Use conditional updates, not read-modify-write counters. Proxy address extraction trusts exactly `TRUSTED_PROXY_COUNT` hops and supports IPv4 and IPv6.

### MFA and email

1. MFA challenge JWTs use a separate key/audience and must never authorize application APIs. Bind purpose, account, version, transport, expiration, and challenge identity to SQL state. Lock that account before reading or consuming factor state.
2. TOTP steps, email OTPs, and recovery codes are single-use. Enrollment, verification, management failure budgets, cooldowns, and send reservations persist in SQL. An unreadable factor still spends its failure budget and returns a safe error.
3. Disabling MFA or replacing recovery codes requires the password plus an existing factor. Administrative password reset never disables MFA. Login and management failure budgets are separate.
4. While holding one user's row lock, touch only that user's MFA rows. Purge expired challenges afterward in a separate best-effort transaction by primary key; cross-user range deletes can deadlock on MySQL/MariaDB.
5. A stored email `code_hash` means sending was attempted and is not known to have failed. Do not retry or fail over after an ambiguous delivery failure. Treat SMTP DATA acceptance as success even if QUIT fails.
6. Transactional email goes through `email.Sender`, with TLS, fixed provider endpoints, no redirects, bounded reads, and sanitized errors. Preserve blank-provider SMTP fallback. Bad email configuration disables email enrollment, not the whole application or recovery access. Adapter additions need offline tests, `.env.example`, Compose, and documentation updates.

### Jobs, storage, and databases

1. Scope every non-admin job list/read/cancel/delete/download by `user_id`. Another user's job returns 404, never an existence-revealing authorization error. Administrator cross-owner history is explicit.
2. Validate extensions on original upload names; preserve sanitized Unicode display names. Store sources under random job IDs with the fixed `source.<ext>` name. Authorize downloads against registered output names, not arbitrary paths. Keep path and symlink guards.
3. Store errors verbatim for diagnosis, but serialize provider errors to administrators only. Submitters may see their own format/encoding problems; other failures get a localized generic error. Provider errors can contain account or key fragments.
4. Job progress originates in completed translation batches and maps monotonically across target languages. Store statuses/stages locale-neutral; localize on serialization.
5. Cancellation is cooperative and race-safe. Queued jobs go directly to canceled; processing jobs move to canceling and only their worker finalizes them. Completion, failure, and progress updates are conditional on current status. Never delete active job files. Cancellation must interrupt request contexts and retry/throttle waits.
6. Recover interrupted jobs once when the server starts, not on an individual worker's restart. Jobs do not resume. Stop the old application before starting another process against the same storage. Call HTTP `Shutdown` before application `Close`.
7. Job quotas count files, not HTTP requests or targets. Enforce per-role, per-account, and panel limits in the same transaction as all job inserts, serialized through the settings row. A multi-file rejection is atomic and returns 429 plus `Retry-After`. Calendar periods are UTC; changing window settings does not erase calendar usage.
8. SQLite, PostgreSQL, MySQL, and MariaDB share the legacy schema and string timestamp formats. Migrations are additive and idempotent. Keep MySQL TEXT defaults as parenthesized expressions. Retry known transaction serialization/deadlock conflicts, including MariaDB 13's error 1020; do not retry external side effects.
9. Preserve documented database TLS semantics, including encrypt-only `require`/`REQUIRED`, certificate-only `verify-ca`/`VERIFY_CA`, and hostname-verifying modes. Reject opportunistic plaintext fallback. Never leak credentials from a database URL in returned or logged errors.
10. Redis is disposable. Every relevant mutation changes its revision in the same SQL transaction. Cache raw locale-neutral data, scope entries to account/admin view, encrypt and bind payloads to keys, and fall back to SQL on outages. Authentication, authorization, quotas, and mutations always use SQL. Keep the Go cache namespace distinct from Python's old cache entries.
11. Publish ZIPs without replacing an archive another request has open; concurrent downloads must work on Windows as well as Linux.

### Subtitle engine

1. Preserve format-specific headers, timing/cue settings, ASS/SSA fields, BOM, newline style, drawings, inline tags, and speaker structure. Output is UTF-8. Add focused round-trip tests for every format change, and retain the Python golden corpus.
2. Translate masked text, not raw markup. Keep tags as zero-width sentinel tokens while wrapping, recognize speaker dashes after leading tags, skip markup-only segments, and validate the placeholder multiset in every reply. VTT entities are decoded only between tags; escape translated text, never restored tags. SRT/ASS do not entity-decode.
3. Malformed replies, empty text translations, count mismatches, and transport/non-JSON failures are retryable translation errors. Fatal errors and exhausted rate limits abort rather than fan out to per-line fallback. Never cache pass-through segments. Stop sibling workers after fatal failure or cancellation.
4. Google v2 requests contain at most 128 texts and decode its returned escaped strings. DeepL requests contain at most 50 texts; split larger configured batches in order. Reject non-finite or over-five-minute rate-limit waits. Provider requests must not follow credential-bearing redirects.
5. Echo is always available to offline engine tests, but the web API and UI expose it only in debug mode. Tests must never call paid providers without explicit authorization and credentials through a safe channel.

### Frontend

1. Preserve the established interface during stack migrations: Dashboard, New translation, Translation history, Account security, Administration, and the panel-settings dialog are existing workflows, not disposable decoration. Keep the original layout, navigation, visual identity, and interactions; a framework rewrite is not permission to redesign or remove screens. Do not add new decorative metrics, redundant labels, marketing copy, or external fonts/CDNs.
2. Use Svelte 5 runes. Do not import or declare a binding named `state` in a component using `$state`; it conflicts with Svelte store-subscription syntax. Use `appState`.
3. Literal regex quantifiers in Svelte attributes need a string expression: `pattern={'[0-9]{6}'}`. `pattern="[0-9]{6}"` interpolates the braces into `[0-9]6` and silently blocks valid form submission. Browser tests must inspect the actual pattern and submit the form.
4. Capture `event.currentTarget` and `event.submitter` synchronously before an `await`. Guard all asynchronous account/session responses against session-generation changes; a late theme response must not restore a signed-out user. Stop polling in hidden tabs and after unmount/sign-out. Confirm polling 401s against `/api/auth/me` because MFA can rotate cookies mid-flight.
5. Preserve system/light/dark account preferences in initial HTML to avoid theme flash. Recreate CAPTCHA widgets when the resolved scheme changes. Wait for the provider's actual `render` function, not just its global stub.
6. Use semantic controls, visible keyboard focus, native dialogs, labeled inputs, reduced-motion support, and usable touch targets. Test mobile widths and long Unicode filenames. No JWTs, factor secrets, recovery codes, or typed provider keys persist in browser storage.
7. `npm run check` must use TypeScript 7's native compiler (`@typescript/native` alias and `svelte-check --tsgo`). TypeScript 6 is only Svelte's required AST bridge. Do not silently downgrade the application checker.

## Validation

Build in this order:

```sh
cd web
npm ci
npm run check
npm run build
cd ..
go vet ./...
go test -race ./...
go build -trimpath -o bin/subtitle-translator ./cmd/subtitle-translator
cd web
npx playwright install chromium
TEST_BINARY=../bin/subtitle-translator npm test
```

The browser suite creates isolated temporary data and uses Echo. Never initialize a server or test against the real `data/` directory. `TEST_DATABASE_URL` must point to a disposable private database: tests drop and recreate application tables. Run SQLite plus PostgreSQL, MariaDB, and MySQL integration on the shared Linux dev VM before handoff. Set `TEST_REDIS_URL` to exercise the encrypted read cache against a private Redis. Keep VM tools/caches within `/srv/subtitle-translator`, register ports, and clean up all processes/artifacts started by the session.

For authentication, quotas, jobs, or startup changes, include concurrent HTTP requests through the real handler. A local Windows run cannot replace the Linux race detector. Frontend changes require real-browser tests and visual inspection in light/dark modes and a narrow viewport; type checking alone misses browser validation and event timing.

Publishing tests must cover GHCR-only, complete Docker Hub configuration, partial configuration, and invalid repository names. Never pass an empty image name to `docker/metadata-action`, even with `enable=false`. Run actionlint when workflows change. Test the production container's health probe, non-root process, default volume ownership upgrade, and both IP stacks.

## Commits

Commit directly on `main`, never a feature branch; push only when the user asks. Use Conventional Commits with an imperative summary and a body explaining why. Split independent work by kind: dependency-only refreshes in `chore(deps)`, fixes per area with their tests/locales, features with their tests/configuration/documentation, and retrospectives/agent rules in a docs commit. An atomic runtime migration must keep its new source, dependency manifests, frontend, container, and CI together so every commit is usable.

Stage explicit paths and only this task's changes, including the user's repository editor configuration. Validate each commit in a separate temporary worktree, not only the final working tree. Clean up temporary worktrees and build artifacts before reporting.
