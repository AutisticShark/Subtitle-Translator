# Subtitle Translator agent guide

This file applies to the entire repository. Read `MEMORY.md` before making changes; it contains the evidence-based retrospective from earlier work. Keep both files updated when a session reveals a reusable project-specific lesson.

## Project map

- `srt_translate.py` contains the translation pipeline, provider clients, retry/throttling behavior, segmentation, tag masking, cue rebuilding, wrapping, and the SRT-only CLI.
- `subtitle_formats.py` adapts SRT, VTT, ASS, and SSA files to and from the shared cue model. Preserve format-specific headers, timings, settings, dialogue fields, newline style, BOM state, and inline tags.
- `webapp.py` is the Flask API, SQLite-backed settings/job store, upload/download boundary, and background job runner. `DATA_DIR` is resolved at import time.
- `static/app.js` and `templates/index.html` implement the browser UI.
- `tests/` currently covers subtitle-format preservation and an Echo-provider web job. Echo is the preferred offline end-to-end provider.
- `.github/workflows/docker-publish.yml` publishes to GHCR unconditionally and to Docker Hub only when all three Docker Hub settings are present.

## Required invariants

- Never expose saved API-key values through `GET /api/settings`; the UI may receive only configured/not-configured state.
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
