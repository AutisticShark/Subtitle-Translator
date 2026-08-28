# Subtitle Translator

A self-hosted web app and command-line tool for translating subtitle files with LLM APIs or DeepL while preserving timings, dialogue structure, positioning, and inline styling.

## Highlights

- SRT, WebVTT (`.vtt`), Advanced SubStation Alpha (`.ass`), and SubStation Alpha (`.ssa`)
- Anthropic, OpenAI and OpenAI-compatible endpoints, DeepL, plus an offline Echo test provider
- Multi-file uploads and multiple target languages per job
- Background job queue, batch-level progress, manual cancellation, per-language downloads, ZIP bundles, and job deletion
- Persistent, write-only API-key fields and translation defaults in the web UI
- Context-aware batching, shared rate-limit backoff, retries, resumable per-job cache, tag masking, and subtitle-aware line wrapping
- Docker health check, persistent named volume, and optional HTTP Basic protection
- The original CLI remains available

## Start with Docker Compose

```bash
cp .env.example .env
# Set APP_PASSWORD in .env if the app is accessible beyond your own machine.
docker compose up --build -d
```

Open <http://localhost:8000>, choose **Settings**, add a provider key and model, then upload subtitles. Application settings, job records, sources, outputs, and resumable caches live in the `subtitle_data` Docker volume.

Queued or processing jobs can be canceled from **Recent jobs**. Finished, failed, and canceled jobs can then be deleted; deletion removes the database record, uploaded source, translated outputs, ZIP bundle, and resumable cache.

Useful commands:

```bash
docker compose logs -f
docker compose down
docker compose down -v  # also permanently deletes saved settings and jobs
```

`PORT`, `APP_PASSWORD`, `JOB_WORKERS`, and `MAX_UPLOAD_MB` can be changed in `.env`. API keys can optionally be bootstrapped with environment variables; values saved through the UI take precedence. When `APP_PASSWORD` is set, the browser prompts for any username and that password.

> API keys saved through the UI are stored in the private Docker volume. The UI never reads them back, but the database itself is not encrypted. Use host permissions, `APP_PASSWORD`, TLS at your reverse proxy, and Docker secrets/environment variables as appropriate for your threat model.

## Run without Docker

Python 3.11 or newer is recommended.

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate
# macOS/Linux: source .venv/bin/activate
pip install -r requirements.txt
python webapp.py
```

The app listens on `http://localhost:8000` and creates `data/` on first start.

## Providers

| Provider | Web settings | Notes |
|---|---|---|
| Anthropic | API key, model | Uses the Messages API |
| OpenAI-compatible | API key, model, base URL | Works with OpenAI and compatible `/v1/chat/completions` servers |
| DeepL | API key | Automatically selects free or paid API by the `:fx` key suffix |
| Echo | None | Offline pipeline test; prefixes text with the target code |

The language list is shared by the CLI and web app. DeepL must support the selected target; the LLM providers can use every target shown in the UI.

## CLI

Existing usage is preserved, with OpenAI-compatible support added:

```bash
python srt_translate.py episode.srt --provider anthropic --langs zh-TW,ja
python srt_translate.py episode.srt --provider deepl --langs de
python srt_translate.py episode.srt --provider openai --model gpt-5-mini \
  --base-url https://api.openai.com/v1
python srt_translate.py episode.srt --provider echo
```

The CLI currently writes SRT. Use the web app for VTT, ASS, and SSA.

## Development and tests

```bash
pip install -r requirements-dev.txt
pytest -q
```

The `/healthz` endpoint is unauthenticated for container and reverse-proxy health checks. Uploaded filenames are sanitized, stored under random job IDs, and downloads are restricted to registered output files.

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
