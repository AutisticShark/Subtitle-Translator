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

## Durable project context

- The application is a self-hosted Flask web UI plus a Python CLI for translating subtitles.
- Supported web formats are SRT, VTT, ASS, and SSA. The CLI currently writes SRT only.
- Providers are Anthropic, OpenAI-compatible APIs, DeepL, Google Cloud Translation - Basic v2, and offline Echo.
- Runtime settings, job records, uploads, outputs, and resumable caches live below `DATA_DIR` (the Docker volume maps it to `/app/data`).
- The settings API treats secrets as write-only. Preserve that security property.
- Echo is the safe, deterministic provider for offline tests.
- Docker publishing always targets GHCR. Docker Hub is optional and requires `DOCKERHUB_IMAGE`, `DOCKERHUB_USERNAME`, and `DOCKERHUB_TOKEN` together.

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
