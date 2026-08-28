#!/usr/bin/env python3
"""
srt_translate.py — translate SubRip (.srt) subtitles into Chinese (or any target
language) while preserving cue numbering, timing, speaker dashes and inline markup.

Design notes
------------
* zh-TW and zh-CN are translated as INDEPENDENT passes by default. Running OpenCC
  over a simplified translation gives you correct glyphs and wrong vocabulary
  (软件/軟體, 视频/影片, 网络/網路, 打印/列印...). Use --zh-tw-mode=opencc only if
  you explicitly want the cheap path.
* Cues are sent in batches with surrounding context so the model can resolve
  pronouns and continuation lines across cue boundaries.
* Inline markup (<i>, <b>, <font ...>, {\\an8}, ASS overrides) is masked with
  sentinels before translation and restored after, so it can't be "helpfully"
  reworded away.
* Results are cached to a sidecar .json keyed by content hash. Re-runs are free
  and interrupted runs resume.

Usage
-----
    python srt_translate.py input.srt --api-key sk-...     # writes .zh.tw.srt + .zh.cn.srt
    python srt_translate.py input.srt --api-key @~/.anthropic-key
    pass anthropic/key | python srt_translate.py input.srt --api-key -
    export ANTHROPIC_API_KEY=sk-...; python srt_translate.py input.srt
    python srt_translate.py *.srt --workers 8 --langs zh-TW
    python srt_translate.py input.srt --provider deepl --api-key @~/.deepl-key
    python srt_translate.py input.srt --provider google --api-key @~/.google-key
    python srt_translate.py input.srt --provider echo      # offline dry run
"""

from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import html
import json
import os
import random
import re
import sys
import threading
import time
import unicodedata
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable, Iterable, Sequence

# --------------------------------------------------------------------------- #
# Language table
# --------------------------------------------------------------------------- #

LANGS = {
    "zh-TW": {
        "suffix": ".zh.tw",
        "name": "Traditional Chinese (Taiwan)",
        "deepl": "ZH-HANT",
        "style": (
            "Use Taiwan Mandarin vocabulary and idiom, not Mainland vocabulary "
            "mechanically converted to traditional glyphs. Use Taiwan full-width "
            "punctuation conventions."
        ),
    },
    "zh-CN": {
        "suffix": ".zh.cn",
        "name": "Simplified Chinese (Mainland)",
        "deepl": "ZH-HANS",
        "style": "Use Mainland Mandarin vocabulary and standard simplified punctuation.",
    },
    "ja": {"suffix": ".ja", "name": "Japanese", "deepl": "JA", "style": ""},
    "ko": {"suffix": ".ko", "name": "Korean", "deepl": "KO", "style": ""},
    "es": {"suffix": ".es", "name": "Spanish", "deepl": "ES", "style": ""},
    "fr": {"suffix": ".fr", "name": "French", "deepl": "FR", "style": ""},
    "de": {"suffix": ".de", "name": "German", "deepl": "DE", "style": ""},
    "it": {"suffix": ".it", "name": "Italian", "deepl": "IT", "style": ""},
    "pt-BR": {"suffix": ".pt.br", "name": "Brazilian Portuguese", "deepl": "PT-BR", "style": ""},
    "ru": {"suffix": ".ru", "name": "Russian", "deepl": "RU", "style": ""},
    "nl": {"suffix": ".nl", "name": "Dutch", "deepl": "NL", "style": ""},
    "pl": {"suffix": ".pl", "name": "Polish", "deepl": "PL", "style": ""},
    "tr": {"suffix": ".tr", "name": "Turkish", "deepl": "TR", "style": ""},
    "uk": {"suffix": ".uk", "name": "Ukrainian", "deepl": "UK", "style": ""},
    "id": {"suffix": ".id", "name": "Indonesian", "deepl": "ID", "style": ""},
}

CJK_LANGS = {"zh-TW", "zh-CN", "ja"}

# --------------------------------------------------------------------------- #
# SRT parsing / writing
# --------------------------------------------------------------------------- #

TIMING_RE = re.compile(
    r"(?P<start>\d{1,2}:\d{2}:\d{2}[,.]\d{1,3})\s*-->\s*"
    r"(?P<end>\d{1,2}:\d{2}:\d{2}[,.]\d{1,3})(?P<rest>.*)"
)

# <i> <b> <u> <font ...> </...>, ASS overrides {\an8}, and {y:i} legacy tags
TAG_RE = re.compile(r"(</?[a-zA-Z][^>]*>|\{[^}]*\})")


@dataclass
class Cue:
    index: int
    start: str
    end: str
    rest: str  # trailing position data on the timing line, e.g. "  X1:100 X2:500"
    lines: list[str]

    @property
    def text(self) -> str:
        return "\n".join(self.lines)


def parse_srt(raw: str) -> list[Cue]:
    """Tolerant SRT parser: handles BOM, CRLF, missing/duplicate indices,
    blank lines inside cues, and files that don't end with a newline."""
    raw = raw.lstrip("\ufeff").replace("\r\n", "\n").replace("\r", "\n")
    lines = raw.split("\n")

    cues: list[Cue] = []
    i = 0
    n = len(lines)
    auto_index = 0

    while i < n:
        # Skip blank padding between cues
        while i < n and not lines[i].strip():
            i += 1
        if i >= n:
            break

        # Optional numeric index line
        index = None
        if lines[i].strip().isdigit() and i + 1 < n and TIMING_RE.search(lines[i + 1]):
            index = int(lines[i].strip())
            i += 1

        m = TIMING_RE.search(lines[i]) if i < n else None
        if not m:
            # Junk line we can't make sense of — skip it rather than abort.
            i += 1
            continue
        i += 1

        auto_index += 1
        body: list[str] = []
        # Consume until a blank line that is followed by a new cue header,
        # so blank lines *inside* a cue don't truncate it.
        while i < n:
            if not lines[i].strip():
                j = i + 1
                while j < n and not lines[j].strip():
                    j += 1
                is_next = j < n and (
                    TIMING_RE.search(lines[j])
                    or (
                        lines[j].strip().isdigit()
                        and j + 1 < n
                        and TIMING_RE.search(lines[j + 1])
                    )
                )
                if is_next or j >= n:
                    i = j
                    break
                body.append("")
                i += 1
                continue
            body.append(lines[i])
            i += 1

        while body and not body[-1].strip():
            body.pop()

        cues.append(
            Cue(
                index=index if index is not None else auto_index,
                start=m.group("start"),
                end=m.group("end"),
                rest=m.group("rest").rstrip(),
                lines=body,
            )
        )

    return cues


def write_srt(cues: Sequence[Cue], path: Path, *, bom: bool = False,
              crlf: bool = False, renumber: bool = False) -> None:
    eol = "\r\n" if crlf else "\n"
    out: list[str] = []
    for n, c in enumerate(cues, 1):
        out.append(str(n if renumber else c.index))
        out.append(f"{c.start} --> {c.end}{c.rest}")
        out.extend(c.lines if c.lines else [""])
        out.append("")
    text = eol.join(out)
    if not text.endswith(eol):
        text += eol
    path.write_text(("\ufeff" if bom else "") + text, encoding="utf-8")


# --------------------------------------------------------------------------- #
# Segmentation: split a cue into translatable segments
# --------------------------------------------------------------------------- #

DASH_RE = re.compile(r"^\s*[-–—]\s*(?=\S)")


@dataclass
class Segment:
    """One translatable unit. A cue is one segment, unless it holds a
    two-speaker dialogue pair, in which case each dashed line is its own."""
    cue_i: int
    text: str          # masked, tags replaced by sentinels
    tags: list[str]    # sentinel payloads, in order
    dashed: bool
    trailing_ws: str = ""


def mask_tags(s: str) -> tuple[str, list[str]]:
    tags: list[str] = []

    def repl(m: re.Match) -> str:
        tags.append(m.group(0))
        return f"\u27e6{len(tags) - 1}\u27e7"

    return TAG_RE.sub(repl, s), tags


def unmask_tags(s: str, tags: list[str]) -> str:
    def repl(m: re.Match) -> str:
        k = int(m.group(1))
        return tags[k] if 0 <= k < len(tags) else ""

    # Tolerate the model adding spaces inside the sentinel
    return re.sub(r"\u27e6\s*(\d+)\s*\u27e7", repl, s)


def segment_cue(cue: Cue, cue_i: int) -> list[Segment]:
    stripped = [l for l in cue.lines if l.strip()]
    dash_lines = [l for l in stripped if DASH_RE.match(l)]

    # Two or more dashed lines => speaker pair; keep the lines distinct.
    if len(dash_lines) >= 2:
        segs = []
        for line in stripped:
            body = DASH_RE.sub("", line)
            masked, tags = mask_tags(body.strip())
            segs.append(Segment(cue_i, masked, tags, dashed=True))
        return segs

    # Otherwise the cue is one sentence fragment possibly wrapped over lines.
    joined = " ".join(l.strip() for l in stripped)
    lead = DASH_RE.match(joined)
    dashed = bool(lead)
    if lead:
        joined = DASH_RE.sub("", joined)
    masked, tags = mask_tags(joined)
    return [Segment(cue_i, masked, tags, dashed=dashed)]


# --------------------------------------------------------------------------- #
# CJK-aware line wrapping
# --------------------------------------------------------------------------- #

CJK_PUNCT_NO_LEAD = "，。、；：？！）」』】》〉,.!?;:)]}"
CJK_PUNCT_NO_TRAIL = "（「『【《〈([{"
HAS_CJK_RE = re.compile(r"[\u3040-\u30ff\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]")


def display_width(s: str) -> float:
    """Full-width chars count 1, half-width count 0.5, so the limit is
    expressed in 'full-width equivalents'."""
    w = 0.0
    for ch in s:
        w += 1.0 if unicodedata.east_asian_width(ch) in ("W", "F") else 0.5
    return w


def wrap_cjk(s: str, limit: float, max_lines: int = 2) -> list[str]:
    """Greedy wrap that won't orphan closing punctuation onto a new line."""
    s = s.strip()
    if not s or display_width(s) <= limit:
        return [s] if s else [""]

    lines: list[str] = []
    cur = ""
    for ch in s:
        if cur and display_width(cur + ch) > limit and ch not in CJK_PUNCT_NO_LEAD:
            if cur and cur[-1] in CJK_PUNCT_NO_TRAIL:
                cur, carry = cur[:-1], cur[-1]
            else:
                carry = ""
            lines.append(cur)
            cur = carry + ch
        else:
            cur += ch
    if cur:
        lines.append(cur)

    # Balanced two-line subtitles read better than a full line plus a stub, so
    # rebalance whenever we wrapped at all (and always if we blew past max_lines).
    n_lines = min(max(len(lines), 2), max_lines) if len(lines) <= max_lines else max_lines
    total = display_width(s)
    target = total / n_lines
    lines, cur = [], ""
    for ch in s:
        if cur and display_width(cur) >= target and len(lines) < n_lines - 1 \
                and ch not in CJK_PUNCT_NO_LEAD:
            if cur[-1] in CJK_PUNCT_NO_TRAIL:
                cur, carry = cur[:-1], cur[-1]
            else:
                carry = ""
            lines.append(cur)
            cur = carry + ch
        else:
            cur += ch
    if cur:
        lines.append(cur)

    return [l.strip() for l in lines if l.strip()]


def wrap_latin(s: str, limit: int, max_lines: int = 2) -> list[str]:
    words, lines, cur = s.split(), [], ""
    for w in words:
        if cur and len(cur) + 1 + len(w) > limit:
            lines.append(cur)
            cur = w
        else:
            cur = f"{cur} {w}".strip()
    if cur:
        lines.append(cur)
    return lines or [""]


# --------------------------------------------------------------------------- #
# Providers
# --------------------------------------------------------------------------- #

class TranslationError(RuntimeError):
    """Transient — worth retrying (5xx, network blips)."""


class FatalTranslationError(TranslationError):
    """Not worth retrying: bad key, forbidden, bad request. Abort the run."""


class RateLimitError(TranslationError):
    """429 / 529. Retried on its own budget, and never fanned out to per-line."""

    def __init__(self, msg: str, retry_after: float | None = None):
        super().__init__(msg)
        self.retry_after = retry_after


class TranslationCanceled(RuntimeError):
    """The caller requested that an in-progress translation stop."""


class Throttle:
    """Shared pacing gate.

    Two jobs. First, when any worker is told to back off, every worker waits —
    otherwise the other threads walk straight back into the limit and the
    backoff accomplishes nothing. Second, optional client-side pacing so you
    can stay under a known RPM without discovering the ceiling by hitting it.
    """

    def __init__(self, rpm: float = 0.0,
                 cancel_callback: Callable[[], bool] | None = None):
        self._lock = threading.Lock()
        self._blocked_until = 0.0
        self._next_slot = 0.0
        self._interval = 60.0 / rpm if rpm > 0 else 0.0
        self._cancel_callback = cancel_callback

    def wait(self) -> None:
        while True:
            if self._cancel_callback and self._cancel_callback():
                raise TranslationCanceled("Translation canceled")
            with self._lock:
                now = time.monotonic()
                target = max(self._blocked_until, self._next_slot)
                if now >= target:
                    self._next_slot = max(now, self._next_slot) + self._interval
                    return
                delay = target - now
            time.sleep(min(delay, 0.25 if self._cancel_callback else 5.0))

    def penalise(self, seconds: float) -> float:
        """Park every worker for `seconds`. Returns the effective wait."""
        with self._lock:
            now = time.monotonic()
            self._blocked_until = max(self._blocked_until, now + seconds)
            return self._blocked_until - now


def _parse_retry_after(value: str | None) -> float | None:
    """`retry-after` is either delta-seconds or an HTTP date."""
    if not value:
        return None
    value = value.strip()
    try:
        return max(0.0, float(value))
    except ValueError:
        pass
    try:
        from email.utils import parsedate_to_datetime
        dt = parsedate_to_datetime(value)
        if dt is None:
            return None
        import datetime as _dt
        now = _dt.datetime.now(dt.tzinfo or _dt.timezone.utc)
        return max(0.0, (dt - now).total_seconds())
    except Exception:
        return None


def _post_json(url: str, headers: dict, payload: dict, timeout: int = 120,
               throttle: Throttle | None = None) -> dict:
    import urllib.error
    import urllib.request

    if throttle is not None:
        throttle.wait()

    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace")[:800]
        hdrs = getattr(e, "headers", None)

        # 429 = rate limited, 529 = provider overloaded. Both mean "come back
        # later", and the response usually says how much later — use that
        # rather than guessing at a backoff curve.
        if e.code in (429, 529):
            after = _parse_retry_after(hdrs.get("retry-after") if hdrs else None)
            if after is None and hdrs:
                for h in ("anthropic-ratelimit-input-tokens-reset",
                          "anthropic-ratelimit-requests-reset",
                          "x-ratelimit-reset-requests"):
                    after = _parse_retry_after(hdrs.get(h))
                    if after is not None:
                        break
            raise RateLimitError(f"HTTP {e.code}: {body[:200]}", after) from None

        # 401/403 = bad or unauthorised key, 400 = malformed request,
        # 404 = wrong model id. Retrying any of these just wastes time and,
        # on a 765-cue file, a lot of it.
        if e.code in (400, 401, 403, 404):
            raise FatalTranslationError(f"HTTP {e.code}: {body}") from None
        raise TranslationError(f"HTTP {e.code}: {body}") from None
    except urllib.error.URLError as e:
        raise TranslationError(f"network error: {e.reason}") from None


SYSTEM_PROMPT = """You are a professional subtitle translator. Translate each \
numbered line from {src} into {tgt}.

{style}

Rules, all mandatory:
1. Output EXACTLY one line per input number, in the form `<number>\u0009<translation>`,
   tab-separated. Same count, same numbers, same order. No preamble, no commentary,
   no blank lines, no markdown.
2. Never merge, split, drop or reorder lines. A line that is a sentence fragment
   stays a fragment — later lines continue it.
3. Copy any \u27e6N\u27e7 sentinel through verbatim and in the same relative position.
   They are markup placeholders, not text.
4. Keep it tight. Subtitles are read in about two seconds; prefer the shorter
   natural phrasing over the literal one.
5. Preserve register, profanity strength, humour and wordplay. Localise idioms
   rather than translating them word for word. If a pun cannot survive, write a
   line that lands the same joke.
6. Do not add honorifics, names or explanations that are not in the source.
7. Leave lines that are purely numbers, timecodes or sound-effect symbols unchanged.
8. Never use full-width Latin letters or digits."""


def make_anthropic(model: str, api_key: str,
                   throttle: Throttle) -> Callable[[list[str], str, str], list[str]]:
    def call(texts: list[str], src: str, tgt_key: str) -> list[str]:
        meta = LANGS[tgt_key]
        numbered = "\n".join(f"{i + 1}\t{t}" for i, t in enumerate(texts))
        resp = _post_json(
            "https://api.anthropic.com/v1/messages",
            {
                "content-type": "application/json",
                "x-api-key": api_key,
                "anthropic-version": "2023-06-01",
            },
            {
                "model": model,
                "max_tokens": 8000,
                "system": SYSTEM_PROMPT.format(
                    src=src, tgt=meta["name"], style=meta["style"]
                ),
                "messages": [{"role": "user", "content": numbered}],
            },
            throttle=throttle,
        )
        out = "".join(
            b.get("text", "") for b in resp.get("content", []) if b.get("type") == "text"
        )
        return parse_numbered(out, len(texts))

    return call


def make_openai(model: str, api_key: str, throttle: Throttle,
                base_url: str = "https://api.openai.com/v1") \
        -> Callable[[list[str], str, str], list[str]]:
    """Create an OpenAI Chat Completions compatible provider.

    ``base_url`` is configurable so the same adapter works with hosted services
    and local servers that expose an OpenAI-compatible API.
    """
    endpoint = base_url.rstrip("/") + "/chat/completions"

    def call(texts: list[str], src: str, tgt_key: str) -> list[str]:
        meta = LANGS[tgt_key]
        numbered = "\n".join(f"{i + 1}\t{t}" for i, t in enumerate(texts))
        resp = _post_json(
            endpoint,
            {
                "content-type": "application/json",
                "authorization": f"Bearer {api_key}",
            },
            {
                "model": model,
                "messages": [
                    {"role": "system", "content": SYSTEM_PROMPT.format(
                        src=src, tgt=meta["name"], style=meta["style"]
                    )},
                    {"role": "user", "content": numbered},
                ],
                "temperature": 0.2,
            },
            throttle=throttle,
        )
        try:
            out = resp["choices"][0]["message"]["content"]
        except (KeyError, IndexError, TypeError) as exc:
            raise TranslationError("OpenAI-compatible API returned an unexpected response") from exc
        return parse_numbered(out, len(texts))

    return call


def make_deepl(api_key: str,
               throttle: Throttle) -> Callable[[list[str], str, str], list[str]]:
    host = "api-free.deepl.com" if api_key.endswith(":fx") else "api.deepl.com"

    def call(texts: list[str], src: str, tgt_key: str) -> list[str]:
        resp = _post_json(
            f"https://{host}/v2/translate",
            {
                "content-type": "application/json",
                "authorization": f"DeepL-Auth-Key {api_key}",
            },
            {
                "text": texts,
                "target_lang": LANGS[tgt_key]["deepl"],
                "preserve_formatting": True,
            },
            throttle=throttle,
        )
        return [t["text"] for t in resp["translations"]]

    return call


def make_google(api_key: str,
                throttle: Throttle) -> Callable[[list[str], str, str], list[str]]:
    """Create a Google Cloud Translation - Basic (v2) provider."""
    from urllib.parse import urlencode

    endpoint = (
        "https://translation.googleapis.com/language/translate/v2?"
        + urlencode({"key": api_key})
    )
    source_codes = {meta["name"].casefold(): key for key, meta in LANGS.items()}
    source_codes["english"] = "en"

    def call(texts: list[str], src: str, tgt_key: str) -> list[str]:
        if len(texts) > 128:
            raise FatalTranslationError(
                "Google Cloud Translation accepts at most 128 strings per request"
            )

        payload: dict[str, object] = {
            "q": texts,
            "target": tgt_key,
            "format": "text",
        }
        source = src.strip()
        source_code = source_codes.get(source.casefold())
        if source_code is None and re.fullmatch(
            r"[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*", source
        ):
            source_code = source
        if source_code:
            payload["source"] = source_code

        resp = _post_json(
            endpoint,
            {"content-type": "application/json; charset=utf-8"},
            payload,
            throttle=throttle,
        )
        try:
            translations = resp["data"]["translations"]
            if not isinstance(translations, list) or len(translations) != len(texts):
                raise ValueError
            result = []
            for item in translations:
                translated = item["translatedText"]
                if not isinstance(translated, str):
                    raise ValueError
                result.append(html.unescape(translated))
            return result
        except (KeyError, TypeError, ValueError) as exc:
            raise TranslationError(
                "Google Cloud Translation API returned an unexpected response"
            ) from exc

    return call


def make_echo() -> Callable[[list[str], str, str], list[str]]:
    """Offline provider for testing the pipeline without an API key."""
    def call(texts: list[str], src: str, tgt_key: str) -> list[str]:
        return [f"[{tgt_key}] {t}" for t in texts]

    return call


def parse_numbered(out: str, expected: int) -> list[str]:
    """Pull `N<tab>text` pairs back out, tolerating stray formatting."""
    got: dict[int, str] = {}
    for line in out.splitlines():
        line = line.strip()
        if not line:
            continue
        m = re.match(r"^(\d+)\s*[\t.)\uff1a:\u3001-]\s*(.*)$", line)
        if not m:
            continue
        k = int(m.group(1))
        if 1 <= k <= expected:
            got[k] = m.group(2).strip()
    missing = [i for i in range(1, expected + 1) if i not in got]
    if missing:
        raise TranslationError(
            f"model returned {len(got)}/{expected} lines; missing {missing[:10]}"
        )
    return [got[i] for i in range(1, expected + 1)]


# --------------------------------------------------------------------------- #
# Batch driver
# --------------------------------------------------------------------------- #

def translate_segments(
    segs: list[Segment],
    provider: Callable[[list[str], str, str], list[str]],
    tgt_key: str,
    src: str,
    batch_size: int,
    retries: int,
    rate_retries: int,
    throttle: Throttle,
    cache: dict,
    workers: int,
    quiet: bool,
    progress_callback: Callable[[int, int], None] | None = None,
    cancel_callback: Callable[[], bool] | None = None,
) -> list[str]:
    results: list[str | None] = [None] * len(segs)

    def check_canceled() -> None:
        if cancel_callback and cancel_callback():
            raise TranslationCanceled("Translation canceled")

    def interruptible_sleep(seconds: float) -> None:
        deadline = time.monotonic() + seconds
        while True:
            check_canceled()
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return
            time.sleep(min(remaining, 0.25))

    # Cache hits first
    check_canceled()
    todo: list[int] = []
    for i, s in enumerate(segs):
        key = hashlib.sha256(f"{tgt_key}\u0000{s.text}".encode()).hexdigest()[:24]
        if key in cache:
            results[i] = cache[key]
        else:
            todo.append(i)

    if not quiet and len(todo) < len(segs):
        print(f"  cache: {len(segs) - len(todo)}/{len(segs)} segments reused",
              file=sys.stderr)

    batches = [todo[i:i + batch_size] for i in range(0, len(todo), batch_size)]
    done = 0
    cached = len(segs) - len(todo)
    if progress_callback:
        progress_callback(cached, len(segs))

    def call_with_retry(texts: list[str], label: str) -> list[str]:
        """Two independent budgets. Rate limits are not failures — they're the
        API telling us to slow down, so they get their own generous allowance
        and don't consume the budget reserved for genuine errors."""
        soft = 0   # 5xx / network
        limited = 0
        while True:
            check_canceled()
            try:
                output = provider(texts, src, tgt_key)
                check_canceled()
                return output
            except FatalTranslationError:
                raise
            except RateLimitError as e:
                limited += 1
                if limited > rate_retries:
                    raise TranslationError(
                        f"rate limited {limited}x, giving up on {label}"
                    ) from None
                # Prefer the server's own number; otherwise exponential with
                # jitter so parallel workers don't resynchronise on retry.
                delay = e.retry_after
                if delay is None:
                    delay = min(2.0 ** limited, 60.0)
                delay += random.uniform(0, min(delay * 0.25, 5.0))
                waited = throttle.penalise(delay)
                if not quiet:
                    print(f"\r  rate limited, all workers pausing "
                          f"{waited:.0f}s (attempt {limited}/{rate_retries})"
                          f"{' ' * 12}", file=sys.stderr, flush=True)
                interruptible_sleep(min(waited, 120.0))
            except TranslationError as e:
                soft += 1
                if soft >= retries:
                    raise
                interruptible_sleep(
                    min(2.0 ** soft, 30.0) + random.uniform(0, 1)
                )

    def run(batch: list[int]) -> tuple[list[int], list[str]]:
        texts = [segs[i].text for i in batch]
        try:
            return batch, call_with_retry(texts, f"batch of {len(texts)}")
        except FatalTranslationError:
            raise
        except RateLimitError:
            raise
        except TranslationError as last:
            # Genuine batch failure — usually one malformed line breaking the
            # numbered-output contract. Retry individually so one bad cue can't
            # sink nineteen good ones. Never do this for rate limits: it turns
            # one rejected request into twenty while already over quota.
            out = []
            for t in texts:
                try:
                    out.append(call_with_retry([t], "single line"))
                except FatalTranslationError:
                    raise
                except TranslationError:
                    out.append(t)  # last resort: source passes through
                else:
                    out[-1] = out[-1][0]
            if not quiet:
                print(f"\r  batch fell back to per-line ({last}){' ' * 12}",
                      file=sys.stderr)
            return batch, out

    ex = concurrent.futures.ThreadPoolExecutor(max_workers=workers)
    completed_normally = False
    try:
        pending = {ex.submit(run, batch) for batch in batches}
        while pending:
            check_canceled()
            finished, pending = concurrent.futures.wait(
                pending, timeout=0.25,
                return_when=concurrent.futures.FIRST_COMPLETED,
            )
            for future in finished:
                batch, out = future.result()
                for i, translated in zip(batch, out):
                    results[i] = translated
                    key = hashlib.sha256(
                        f"{tgt_key}\u0000{segs[i].text}".encode()
                    ).hexdigest()[:24]
                    cache[key] = translated
                done += len(batch)
                if progress_callback:
                    progress_callback(cached + done, len(segs))
                if not quiet:
                    pct = 100 * done / max(len(todo), 1)
                    print(f"\r  {tgt_key}: {done}/{len(todo)} ({pct:.0f}%)",
                          end="", file=sys.stderr, flush=True)
        check_canceled()
        completed_normally = True
    finally:
        ex.shutdown(wait=completed_normally, cancel_futures=not completed_normally)

    if not quiet and todo:
        print(file=sys.stderr)
    return [r if r is not None else "" for r in results]


# --------------------------------------------------------------------------- #
# Reassembly
# --------------------------------------------------------------------------- #

def rebuild_cues(
    cues: list[Cue], segs: list[Segment], out: list[str], tgt_key: str,
    width: float, max_lines: int,
) -> list[Cue]:
    by_cue: dict[int, list[tuple[Segment, str]]] = {}
    for s, t in zip(segs, out):
        by_cue.setdefault(s.cue_i, []).append((s, t))

    cjk = tgt_key in CJK_LANGS
    new: list[Cue] = []

    for ci, cue in enumerate(cues):
        items = by_cue.get(ci, [])
        if not items:
            new.append(cue)
            continue

        if len(items) >= 2:
            # Speaker pair: one line each, dash restored.
            lines = []
            for s, t in items:
                t = unmask_tags(t, s.tags).strip()
                lines.append(f"- {t}" if s.dashed else t)
        else:
            s, t = items[0]
            t = unmask_tags(t, s.tags).strip()
            # A "CJK" target can still emit CJK-free lines (names, numbers,
            # untranslated codes). Wrapping those by character splits words in
            # half, so pick the wrapper from the actual output, not the target.
            use_cjk = cjk and HAS_CJK_RE.search(t) is not None
            body = (wrap_cjk(t, width, max_lines) if use_cjk
                    else wrap_latin(t, int(width * 2), max_lines))
            if s.dashed and body:
                body[0] = f"- {body[0]}"
            lines = body

        new.append(Cue(cue.index, cue.start, cue.end, cue.rest, lines))

    return new


# --------------------------------------------------------------------------- #
# Main
# --------------------------------------------------------------------------- #

def output_path(src: Path, tgt_key: str, outdir: Path | None) -> Path:
    stem = src.name
    if stem.lower().endswith(".srt"):
        stem = stem[:-4]
    # Strip an existing language suffix so .en.srt -> .zh.tw.srt, not .en.zh.tw.srt
    stem = re.sub(r"\.(en|eng|english)$", "", stem, flags=re.I)
    name = f"{stem}{LANGS[tgt_key]['suffix']}.srt"
    return (outdir or src.parent) / name


def process(path: Path, args, provider, throttle: Throttle) -> None:
    raw = path.read_bytes()
    bom = raw.startswith(b"\xef\xbb\xbf")
    crlf = b"\r\n" in raw[:4000] if not args.lf else False
    text = raw.decode(args.encoding, errors="replace")

    cues = parse_srt(text)
    if not cues:
        print(f"!! {path.name}: no cues parsed", file=sys.stderr)
        return
    print(f"{path.name}: {len(cues)} cues", file=sys.stderr)

    segs: list[Segment] = []
    for ci, cue in enumerate(cues):
        segs.extend(segment_cue(cue, ci))

    cache_path = path.with_suffix(path.suffix + ".xlate-cache.json")
    cache: dict = {}
    if cache_path.exists() and not args.no_cache:
        try:
            cache = json.loads(cache_path.read_text("utf-8"))
        except Exception:
            cache = {}

    for tgt_key in args.langs:
        dest = output_path(path, tgt_key, args.outdir)
        if dest.exists() and not args.force:
            print(f"  skip {dest.name} (exists; --force to overwrite)", file=sys.stderr)
            continue

        out = translate_segments(
            segs, provider, tgt_key, args.source_lang,
            args.batch_size, args.retries, args.rate_limit_retries,
            throttle, cache, args.workers, args.quiet,
        )
        new_cues = rebuild_cues(cues, segs, out, tgt_key, args.width, args.max_lines)

        if args.outdir:
            args.outdir.mkdir(parents=True, exist_ok=True)
        write_srt(new_cues, dest, bom=bom, crlf=crlf, renumber=args.renumber)
        print(f"  -> {dest}", file=sys.stderr)

        if not args.no_cache:
            cache_path.write_text(json.dumps(cache, ensure_ascii=False), "utf-8")

    if args.zh_tw_mode == "opencc" and "zh-CN" in args.langs and "zh-TW" not in args.langs:
        convert_opencc(output_path(path, "zh-CN", args.outdir),
                       output_path(path, "zh-TW", args.outdir))


def convert_opencc(src: Path, dest: Path) -> None:
    try:
        import opencc  # type: ignore
    except ImportError:
        print("!! --zh-tw-mode=opencc needs: pip install opencc-python-reimplemented",
              file=sys.stderr)
        return
    conv = opencc.OpenCC("s2twp")  # s2twp includes Taiwan phrase substitution
    cues = parse_srt(src.read_text("utf-8"))
    for c in cues:
        c.lines = [conv.convert(l) for l in c.lines]
    write_srt(cues, dest)
    print(f"  -> {dest} (via OpenCC s2twp)", file=sys.stderr)


def resolve_key(cli_value: str | None, env_name: str, label: str) -> str:
    """Resolve an API key from --api-key, then $ENV.

    --api-key accepts:
      sk-xxxx          literal value
      @/path/to/file   read from a file (first non-empty line)
      -                read from stdin
    A literal on the command line is visible in shell history and to anyone who
    can run `ps` on the box, so @file or the env var is safer for anything
    long-lived. Nothing stops you using the literal for a one-off.
    """
    v = cli_value
    if v:
        v = v.strip()
        if v == "-":
            v = ""
            for line in sys.stdin:
                if line.strip():
                    v = line.strip()
                    break
            if not v:
                raise SystemExit(f"error: no {label} key read from stdin")
        elif v.startswith("@"):
            path = Path(os.path.expanduser(v[1:]))
            if not path.is_file():
                raise SystemExit(f"error: key file not found: {path}")
            v = next((l.strip() for l in path.read_text("utf-8").splitlines()
                      if l.strip()), "")
            if not v:
                raise SystemExit(f"error: key file is empty: {path}")
        return v

    v = os.environ.get(env_name, "").strip()
    if not v:
        raise SystemExit(
            f"error: no {label} key. Pass --api-key (value, @file, or - for "
            f"stdin) or set ${env_name}."
        )
    return v


def main(argv: Sequence[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        description="Translate .srt subtitles, preserving timing and structure.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("files", nargs="+", type=Path)
    p.add_argument("--langs", default="zh-TW,zh-CN",
                   help="comma-separated targets (default: zh-TW,zh-CN)")
    p.add_argument("--source-lang", default="English")
    p.add_argument("--provider",
                   choices=["anthropic", "openai", "deepl", "google", "echo"],
                   default="anthropic")
    p.add_argument("--model", default="claude-sonnet-4-6",
                   help="model id for LLM providers")
    p.add_argument("--base-url", default="https://api.openai.com/v1",
                   help="base URL for --provider=openai")
    p.add_argument("--zh-tw-mode", choices=["native", "opencc"], default="native",
                   help="native = separate translation pass (default, better); "
                        "opencc = convert from the zh-CN output (cheap, worse)")
    p.add_argument("--batch-size", type=int, default=20)
    p.add_argument("--workers", type=int, default=4)
    p.add_argument("--retries", type=int, default=4,
                   help="attempts for genuine errors (5xx, network)")
    p.add_argument("--rate-limit-retries", type=int, default=10,
                   help="separate, larger budget for 429/529 backoff")
    p.add_argument("--rpm", type=float, default=0,
                   help="client-side cap on requests/min across all workers "
                        "(0 = unthrottled until the API pushes back)")
    p.add_argument("--width", type=float, default=16,
                   help="max line width in full-width chars (default 16)")
    p.add_argument("--max-lines", type=int, default=2)
    p.add_argument("--encoding", default="utf-8")
    p.add_argument("--outdir", type=Path)
    p.add_argument("--renumber", action="store_true",
                   help="renumber cues 1..N instead of keeping source indices")
    p.add_argument("--lf", action="store_true", help="force LF endings")
    p.add_argument("--force", action="store_true", help="overwrite existing outputs")
    p.add_argument("--no-cache", action="store_true")
    p.add_argument("--api-key", metavar="KEY",
                   help="API key for the chosen provider. Accepts a literal "
                        "value, @/path/to/keyfile, or - to read from stdin. "
                        "Falls back to $ANTHROPIC_API_KEY / $OPENAI_API_KEY / "
                        "$DEEPL_API_KEY / $GOOGLE_API_KEY.")
    p.add_argument("--quiet", action="store_true")
    args = p.parse_args(argv)

    args.langs = [l.strip() for l in args.langs.split(",") if l.strip()]
    for l in args.langs:
        if l not in LANGS:
            p.error(f"unknown language {l!r}; known: {', '.join(LANGS)}")

    throttle = Throttle(rpm=args.rpm)

    if args.provider == "anthropic":
        provider = make_anthropic(
            args.model,
            resolve_key(args.api_key, "ANTHROPIC_API_KEY", "Anthropic"),
            throttle,
        )
    elif args.provider == "openai":
        provider = make_openai(
            args.model,
            resolve_key(args.api_key, "OPENAI_API_KEY", "OpenAI-compatible"),
            throttle,
            args.base_url,
        )
    elif args.provider == "deepl":
        provider = make_deepl(
            resolve_key(args.api_key, "DEEPL_API_KEY", "DeepL"), throttle
        )
    elif args.provider == "google":
        provider = make_google(
            resolve_key(args.api_key, "GOOGLE_API_KEY", "Google Cloud Translation"),
            throttle,
        )
    else:
        provider = make_echo()

    files = [f for f in args.files if f.is_file()]
    if not files:
        p.error("no readable input files")

    for f in files:
        try:
            process(f, args, provider, throttle)
        except KeyboardInterrupt:
            print("\ninterrupted (cache saved; re-run to resume)", file=sys.stderr)
            return 130
        except FatalTranslationError as e:
            print(f"\n!! aborting: {e}", file=sys.stderr)
            print("   check the provider settings, then re-run "
                  "(finished work is cached).", file=sys.stderr)
            return 2
        except Exception as e:
            print(f"!! {f.name}: {type(e).__name__}: {e}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    sys.exit(main())
