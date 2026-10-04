"""
Subtitle translation engine used by the web app: SubRip (.srt) parsing, tag masking,
segmentation, provider clients, batched translation, cue rebuilding, and wrapping.

Design notes
------------
* Cues are sent in batches with surrounding context so the model can resolve
  pronouns and continuation lines across cue boundaries.
* Inline markup (<i>, <b>, <font ...>, {\\an8}, ASS overrides) is masked with
  sentinels before translation and restored after, so it can't be "helpfully"
  reworded away.
* Translations are cached by content hash so interrupted jobs resume without
  repeating completed batches.
"""

from __future__ import annotations

import concurrent.futures
import datetime
import hashlib
import html
import http.client
import json
import random
import re
import sys
import threading
import time
import unicodedata
from collections import Counter
from dataclasses import dataclass
from typing import Callable

# --------------------------------------------------------------------------- #
# Language table
# --------------------------------------------------------------------------- #

LANGS = {
    "zh-TW": {
        "suffix": ".zh.tw",
        "name": "Traditional Chinese (Taiwan)",
        "deepl": "ZH-HANT",
        "style": (
            "Use Taiwan Mandarin vocabulary and idiom, not Chinese vocabulary "
            "mechanically converted to traditional glyphs. Use Taiwan full-width "
            "punctuation conventions."
        ),
    },
    "zh-CN": {
        "suffix": ".zh.cn",
        "name": "Simplified Chinese (China)",
        "deepl": "ZH-HANS",
        "style": "Use Chinese Mandarin vocabulary and standard simplified punctuation.",
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

# <i> <b> <u> <font ...> </...>, WebVTT karaoke timestamps <00:00:01.000>,
# ASS overrides {\an8}, and {y:i} legacy tags
TAG_RE = re.compile(
    r"(</?[a-zA-Z][^>]*>|<(?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3}>|\{[^}]*\})"
)


@dataclass
class Cue:
    index: int
    start: str
    end: str
    rest: str  # trailing position data on the timing line, e.g. "  X1:100 X2:500"
    lines: list[str]
    # Markup dialect of the text. "ass" additionally masks the \h hard space
    # and \n soft break escapes, which are plain text in the other formats.
    dialect: str = ""

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

    def starts_cue(j: int) -> bool:
        """A timing line, or an index line directly followed by one."""
        return bool(
            TIMING_RE.search(lines[j])
            or (
                lines[j].strip().isdigit()
                and j + 1 < n
                and TIMING_RE.search(lines[j + 1])
            )
        )

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
        # so blank lines *inside* a cue don't truncate it. A header directly
        # after text (a missing blank separator) also ends the cue.
        while i < n:
            if not lines[i].strip():
                j = i + 1
                while j < n and not lines[j].strip():
                    j += 1
                if j >= n or starts_cue(j):
                    i = j
                    break
                body.append("")
                i += 1
                continue
            if starts_cue(i):
                break
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


# --------------------------------------------------------------------------- #
# Segmentation: split a cue into translatable segments
# --------------------------------------------------------------------------- #

# A speaker dash, optionally preceded by masked tags (`<i>- Hi`, `{\an8}- Hi`).
# A hyphen-minus directly followed by a digit is a minus sign (`-10 degrees`).
DASH_RE = re.compile(
    r"^(?P<lead>\s*(?:\u27e6\d+\u27e7\s*)*)(?:-(?!\d)|[\u2013\u2014])\s*(?=\S)"
)

SENTINEL_RE = re.compile(r"\u27e6\s*(\d+)\s*\u27e7")

# Wrapping works on text whose placeholders are single characters from the
# Supplementary Private Use Area-A: zero-width and impossible to split.
_PH_BASE = 0xF0000
_PH_CLASS = "\U000f0000-\U000ffffd"
_PH_RE = re.compile(f"[{_PH_CLASS}]")
_LEAD_PH_RE = re.compile(f"^[\\s{_PH_CLASS}]*")

# Characters that must never reach a provider as text: the sentinel brackets
# themselves and the internal placeholder range. They are masked as tags so
# they round-trip byte-for-byte instead of being mistaken for placeholders.
_RESERVED = f"[\u27e6\u27e7{_PH_CLASS}]"
_MASK_RE = re.compile(TAG_RE.pattern[:-1] + "|" + _RESERVED + ")")
# ASS additionally has the \h (hard space) and \n (soft line break) escapes.
_MASK_ASS_RE = re.compile(TAG_RE.pattern[:-1] + r"|\\[hn]|" + _RESERVED + ")")
_DRAWING_RE = re.compile(r"\\p(\d+)")


@dataclass
class Segment:
    """One translatable unit. A cue is one segment, unless it holds a
    two-speaker dialogue pair, in which case each speaker is its own."""
    cue_i: int
    text: str          # masked, tags replaced by sentinels
    tags: list[str]    # sentinel payloads, in order
    dashed: bool
    trailing_ws: str = ""
    # The dash followed leading tags (`<i>- Hi`) rather than preceding them.
    dash_inside: bool = False


def mask_tags(s: str, ass: bool = False) -> tuple[str, list[str]]:
    """Replace markup with numbered sentinels.

    ASS vector drawings (the text after a ``\\p1`` override, up to and
    including the ``\\p0`` override that ends it) are folded into one
    placeholder: drawing commands are neither translatable nor wrappable.
    """
    pattern = _MASK_ASS_RE if ass else _MASK_RE
    pieces: list[list] = []  # [is_tag, text]
    drawing = False
    pos = 0
    for m in [*pattern.finditer(s), None]:
        end = m.start() if m is not None else len(s)
        if end > pos:
            if drawing:
                pieces[-1][1] += s[pos:end]
            else:
                pieces.append([False, s[pos:end]])
        if m is None:
            break
        tag = m.group(0)
        if drawing:
            pieces[-1][1] += tag
        else:
            pieces.append([True, tag])
        if tag.startswith("{"):
            modes = _DRAWING_RE.findall(tag)
            if modes:
                drawing = int(modes[-1]) > 0
        pos = m.end()

    tags: list[str] = []
    out: list[str] = []
    for is_tag, text in pieces:
        if is_tag:
            out.append(f"\u27e6{len(tags)}\u27e7")
            tags.append(text)
        else:
            out.append(text)
    return "".join(out), tags


def unmask_tags(s: str, tags: list[str]) -> str:
    def repl(m: re.Match) -> str:
        k = int(m.group(1))
        return tags[k] if 0 <= k < len(tags) else ""

    # Tolerate the model adding spaces inside the sentinel
    return SENTINEL_RE.sub(repl, s)


def placeholders_match(source: str, translated: str) -> bool:
    """True when ``translated`` holds exactly the source's placeholders.

    Order may change (translation reorders words), but every placeholder must
    appear as often as in the source and no stray sentinel bracket may remain;
    otherwise restoring the tags would drop, duplicate, or invent markup.
    """
    expected = Counter(int(k) for k in SENTINEL_RE.findall(source))
    got = Counter(int(k) for k in SENTINEL_RE.findall(translated))
    if expected != got:
        return False
    rest = SENTINEL_RE.sub("", translated)
    return "\u27e6" not in rest and "\u27e7" not in rest


def needs_translation(seg: Segment) -> bool:
    """Segments with no text besides markup are passed through unchanged."""
    return bool(SENTINEL_RE.sub("", seg.text).strip())


def _split_dash(line: str, ass: bool) -> tuple[bool, bool, str]:
    """Detect a speaker dash on the masked line, so leading tags can't hide
    it. Returns (dashed, dash_after_tags, raw line without the dash)."""
    masked, tags = mask_tags(line, ass)
    m = DASH_RE.match(masked)
    if not m:
        return False, False, line
    lead = m.group("lead").strip()
    return True, bool(lead), unmask_tags(lead + masked[m.end():], tags)


def segment_cue(cue: Cue, cue_i: int) -> list[Segment]:
    ass = cue.dialect == "ass"
    stripped = [line.strip() for line in cue.lines if line.strip()]
    parsed = [_split_dash(line, ass) for line in stripped]

    # Two or more dashed lines => speaker pair; keep the speakers distinct.
    # An undashed line continues the preceding speaker's sentence.
    if sum(1 for dashed, _inside, _body in parsed if dashed) >= 2:
        groups: list[tuple[bool, bool, list[str]]] = []
        for dashed, inside, body in parsed:
            if dashed or not groups:
                groups.append((dashed, inside, [body.strip()]))
            else:
                groups[-1][2].append(body.strip())
        segs = []
        for dashed, inside, bodies in groups:
            masked, tags = mask_tags(" ".join(bodies), ass)
            segs.append(Segment(cue_i, masked, tags, dashed=dashed, dash_inside=inside))
        return segs

    # Otherwise the cue is one sentence fragment possibly wrapped over lines.
    dashed, inside, joined = _split_dash(" ".join(stripped), ass)
    masked, tags = mask_tags(joined.strip(), ass)
    return [Segment(cue_i, masked, tags, dashed=dashed, dash_inside=inside)]


# --------------------------------------------------------------------------- #
# CJK-aware line wrapping
# --------------------------------------------------------------------------- #

CJK_PUNCT_NO_LEAD = "，。、；：？！）」』】》〉,.!?;:)]}"
CJK_PUNCT_NO_TRAIL = "（「『【《〈([{"
HAS_CJK_RE = re.compile(r"[\u3040-\u30ff\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]")


def display_width(s: str) -> float:
    """Full-width chars count 1, half-width count 0.5, so the limit is
    expressed in 'full-width equivalents'. Internal tag placeholders are
    zero-width."""
    w = 0.0
    for ch in s:
        if _is_placeholder(ch):
            continue
        w += 1.0 if unicodedata.east_asian_width(ch) in ("W", "F") else 0.5
    return w


def _is_placeholder(ch: str) -> bool:
    return _PH_BASE <= ord(ch) <= 0xFFFFD


def _visible_len(s: str) -> int:
    return sum(1 for ch in s if not _is_placeholder(ch))


def wrap_cjk(s: str, limit: float, max_lines: int = 2) -> list[str]:
    """Greedy wrap that won't orphan closing punctuation onto a new line.
    Tag placeholders stay attached to the text before them."""
    s = s.strip()
    if not s or display_width(s) <= limit:
        return [s] if s else [""]

    lines: list[str] = []
    cur = ""
    for ch in s:
        if cur and display_width(cur + ch) > limit and ch not in CJK_PUNCT_NO_LEAD \
                and not _is_placeholder(ch):
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
                and ch not in CJK_PUNCT_NO_LEAD and not _is_placeholder(ch):
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

    return [line.strip() for line in lines if line.strip()]


def _latin_words(s: str) -> list[str]:
    """Split on whitespace, gluing placeholder-only words (a tag between two
    spaces) to the next word, or to the previous one at the end, so a line
    never consists of markup alone."""
    words: list[str] = []
    pending = ""
    for w in s.split():
        if not _visible_len(w):
            pending = f"{pending} {w}" if pending else w
            continue
        words.append(f"{pending} {w}" if pending else w)
        pending = ""
    if pending:
        if words:
            words[-1] += " " + pending
        else:
            words.append(pending)
    return words


def wrap_latin(s: str, limit: int, max_lines: int = 2) -> list[str]:
    words, lines, cur = _latin_words(s), [], ""
    for w in words:
        if cur and _visible_len(cur) + 1 + _visible_len(w) > limit:
            lines.append(cur)
            cur = w
        else:
            cur = f"{cur} {w}" if cur else w
    if cur:
        lines.append(cur)
    if not lines:
        return [""]

    max_lines = max(1, max_lines)
    if len(lines) <= max_lines:
        return lines

    # The width is a preference, while max_lines is a hard subtitle-layout
    # constraint. Rebalance all words when the greedy pass needs too many lines.
    # Long words can use up the words early; stop rather than emit empty lines.
    remaining = list(words)
    balanced: list[str] = []
    while remaining and len(balanced) < max_lines - 1:
        slots = max_lines - len(balanced)
        remaining_width = sum(_visible_len(word) for word in remaining) + len(remaining) - 1
        target = (remaining_width + slots - 1) // slots
        current = remaining.pop(0)
        while remaining and _visible_len(current) + 1 + _visible_len(remaining[0]) <= target:
            current += " " + remaining.pop(0)
        balanced.append(current)
    if remaining:
        balanced.append(" ".join(remaining))
    return balanced


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
        now = datetime.datetime.now(dt.tzinfo or datetime.timezone.utc)
        return max(0.0, (dt - now).total_seconds())
    except Exception:
        return None


_GO_DURATION_RE = re.compile(r"(?:\d+(?:\.\d+)?(?:h|ms|us|µs|μs|ns|m|s))+")
_GO_DURATION_PART_RE = re.compile(r"(\d+(?:\.\d+)?)(h|ms|us|µs|μs|ns|m|s)")
_GO_UNITS = {"h": 3600.0, "m": 60.0, "s": 1.0, "ms": 1e-3,
             "us": 1e-6, "µs": 1e-6, "μs": 1e-6, "ns": 1e-9}


def _parse_rate_limit_reset(value: str | None) -> float | None:
    """Seconds until a rate-limit reset header's moment.

    Accepts what ``retry-after`` accepts, RFC 3339 timestamps
    (``anthropic-ratelimit-*-reset``), and Go durations such as ``6m0s`` or
    ``250ms`` (``x-ratelimit-reset-*``). Past moments clamp to zero.
    """
    after = _parse_retry_after(value)
    if after is not None or not value:
        return after
    value = value.strip()
    if _GO_DURATION_RE.fullmatch(value):
        return max(0.0, sum(float(amount) * _GO_UNITS[unit]
                            for amount, unit in _GO_DURATION_PART_RE.findall(value)))
    iso = value[:-1] + "+00:00" if value[-1:] in ("Z", "z") else value
    try:
        moment = datetime.datetime.fromisoformat(iso)
    except ValueError:
        return None
    if moment.tzinfo is None:
        moment = moment.replace(tzinfo=datetime.timezone.utc)
    now = datetime.datetime.now(datetime.timezone.utc)
    return max(0.0, (moment - now).total_seconds())


def _post_json(url: str, headers: dict, payload: dict, timeout: float = 120,
               throttle: Throttle | None = None) -> dict:
    import urllib.error
    import urllib.request

    if throttle is not None:
        throttle.wait()

    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
    except urllib.error.HTTPError as e:
        try:
            body = e.read().decode("utf-8", "replace")[:800]
        except (OSError, http.client.HTTPException):
            body = ""
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
                    after = _parse_rate_limit_reset(hdrs.get(h))
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
    except (OSError, http.client.HTTPException) as e:
        # Timeouts while waiting for or reading the response, dropped
        # connections (RemoteDisconnected), truncated bodies (IncompleteRead),
        # and TLS failures are transient: let the retry policy handle them.
        raise TranslationError(
            f"network error: {type(e).__name__}: {e}".rstrip(": ")
        ) from None

    try:
        result = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, ValueError):
        # Proxies and gateways sometimes answer 200 with an HTML error page.
        raise TranslationError("provider returned a response that is not JSON") from None
    if not isinstance(result, dict):
        raise TranslationError("provider returned an unexpected JSON response")
    return result


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
        try:
            translations = resp["translations"]
            if not isinstance(translations, list) or len(translations) != len(texts):
                raise ValueError
            result = []
            for item in translations:
                translated = item["text"]
                if not isinstance(translated, str):
                    raise ValueError
                result.append(translated)
            return result
        except (KeyError, TypeError, ValueError) as exc:
            raise TranslationError("DeepL API returned an unexpected response") from exc

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


_NUMBERED_RE = re.compile(r"^(\d+)\s*[\t.)\uff1a:\u3001-]\s*(.*)$")
_NUMBER_ONLY_RE = re.compile(r"^(\d+)$")


def _join_continuation(head: str, tail: str) -> str:
    if not head:
        return tail
    # CJK text has no inter-word spaces; don't invent one at the line break.
    if HAS_CJK_RE.match(head[-1]) and HAS_CJK_RE.match(tail[0]):
        return head + tail
    return f"{head} {tail}"


def parse_numbered(out: str, expected: int) -> list[str]:
    """Pull `N<tab>text` pairs back out, tolerating stray formatting.

    A bare `N` (the tab and empty translation trimmed away) is an empty line,
    and an unnumbered line continues the entry before it. Text before the
    first numbered line is preamble and ignored.
    """
    got: dict[int, str] = {}
    last: int | None = None
    for line in out.splitlines():
        line = line.strip()
        if not line or line.startswith("```"):
            continue
        m = _NUMBERED_RE.match(line)
        if m and 1 <= int(m.group(1)) <= expected:
            last = int(m.group(1))
            got[last] = m.group(2).strip()
            continue
        m = _NUMBER_ONLY_RE.match(line)
        if m and 1 <= int(m.group(1)) <= expected and int(m.group(1)) not in got:
            last = int(m.group(1))
            got[last] = ""
            continue
        if last is not None:
            got[last] = _join_continuation(got[last], line)
    missing = [i for i in range(1, expected + 1) if i not in got]
    if missing:
        raise TranslationError(
            f"model returned {len(got)}/{expected} lines; missing {missing[:10]}"
        )
    return [got[i] for i in range(1, expected + 1)]


# --------------------------------------------------------------------------- #
# Batch driver
# --------------------------------------------------------------------------- #

def _validate_output(texts: list[str], output: object) -> None:
    """Reject provider output that can't be mapped back onto the input:
    the wrong number of strings, or translations whose tag placeholders were
    dropped, duplicated, or invented. Raised as a retryable format error so
    the retry and per-line fallback path handles it."""
    if not isinstance(output, list) or len(output) != len(texts):
        count = len(output) if isinstance(output, list) else "no"
        raise TranslationError(
            f"provider returned {count} translations for {len(texts)} lines"
        )
    for position, (source, translated) in enumerate(zip(texts, output, strict=True)):
        if not isinstance(translated, str):
            raise TranslationError(
                f"provider returned a non-text translation for line {position + 1}"
            )
        if not placeholders_match(source, translated):
            raise TranslationError(
                f"line {position + 1} lost or altered its markup placeholders"
            )


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
    fallback_callback: Callable[[int], None] | None = None,
) -> list[str]:
    """Translate segments in batches.

    ``fallback_callback`` receives the number of segments in each finished batch
    that could not be translated and were passed through as source text.
    """
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
        if not needs_translation(s):
            # Empty text or markup only (e.g. an ASS drawing): nothing to send.
            results[i] = s.text
            continue
        key = hashlib.sha256(f"{tgt_key}\u0000{s.text}".encode()).hexdigest()[:24]
        hit = cache.get(key)
        if isinstance(hit, str) and placeholders_match(s.text, hit):
            results[i] = hit
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
                _validate_output(texts, output)
                return output
            except FatalTranslationError:
                raise
            except RateLimitError as e:
                limited += 1
                if limited > rate_retries:
                    # Stay a RateLimitError so run() aborts instead of retrying
                    # every line individually while already over quota.
                    raise RateLimitError(
                        f"rate limited {limited}x, giving up on {label}",
                        e.retry_after,
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
            except TranslationError:
                soft += 1
                if soft >= retries:
                    raise
                interruptible_sleep(
                    min(2.0 ** soft, 30.0) + random.uniform(0, 1)
                )

    def run(batch: list[int]) -> tuple[list[int], list[str], set[int]]:
        texts = [segs[i].text for i in batch]
        try:
            return batch, call_with_retry(texts, f"batch of {len(texts)}"), set()
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
            passed_through: set[int] = set()
            for position, t in enumerate(texts):
                try:
                    out.append(call_with_retry([t], "single line")[0])
                except (FatalTranslationError, RateLimitError):
                    # A rate limit must abort, never fan out line by line.
                    raise
                except TranslationError:
                    out.append(t)  # last resort: source passes through
                    passed_through.add(position)
            if not quiet:
                print(f"\r  batch fell back to per-line ({last}){' ' * 12}",
                      file=sys.stderr)
            return batch, out, passed_through

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
                batch, out, passed_through = future.result()
                for position, (i, translated) in enumerate(zip(batch, out, strict=True)):
                    results[i] = translated
                    if position in passed_through:
                        continue  # never cache untranslated source text
                    key = hashlib.sha256(
                        f"{tgt_key}\u0000{segs[i].text}".encode()
                    ).hexdigest()[:24]
                    cache[key] = translated
                if passed_through and fallback_callback:
                    fallback_callback(len(passed_through))
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

def _to_wrap_form(t: str, tags: list[str]) -> str:
    """Turn sentinels into single zero-width placeholder characters.

    Defensive against output that bypassed validation (direct callers, old
    caches): unknown or repeated placeholders and stray brackets are dropped,
    and missing ones are appended so no tag is lost.
    """
    seen: set[int] = set()

    def repl(m: re.Match) -> str:
        k = int(m.group(1))
        if 0 <= k < len(tags) and k not in seen:
            seen.add(k)
            return chr(_PH_BASE + k)
        return ""

    s = SENTINEL_RE.sub(repl, _PH_RE.sub("", t))
    s = s.replace("⟦", "").replace("⟧", "").strip()
    return s + "".join(chr(_PH_BASE + k) for k in range(len(tags)) if k not in seen)


def _from_wrap_form(s: str, tags: list[str]) -> str:
    def repl(m: re.Match) -> str:
        k = ord(m.group(0)) - _PH_BASE
        return tags[k] if 0 <= k < len(tags) else ""

    return _PH_RE.sub(repl, s)


def _with_dash(line: str, inside: bool) -> str:
    """Restore a speaker dash, after the leading tags if it was there."""
    if inside:
        lead = _LEAD_PH_RE.match(line)
        assert lead is not None
        head = line[:lead.end()].strip()
        if head:
            return f"{head}- {line[lead.end():]}"
    return f"- {line}"


def rebuild_cues(
    cues: list[Cue], segs: list[Segment], out: list[str], tgt_key: str,
    width: float, max_lines: int,
) -> list[Cue]:
    by_cue: dict[int, list[tuple[Segment, str]]] = {}
    for s, t in zip(segs, out, strict=True):
        by_cue.setdefault(s.cue_i, []).append((s, t))

    cjk = tgt_key in CJK_LANGS
    new: list[Cue] = []

    for ci, cue in enumerate(cues):
        items = by_cue.get(ci, [])
        if not items or not any(needs_translation(s) for s, _t in items):
            # Nothing was translated (empty cue, markup or drawing only):
            # keep the original lines exactly, without re-wrapping.
            new.append(cue)
            continue

        if len(items) >= 2:
            # Speaker pair: one line each, dash restored.
            lines = []
            for s, t in items:
                line = _to_wrap_form(t, s.tags).strip()
                if s.dashed:
                    line = _with_dash(line, s.dash_inside)
                lines.append(_from_wrap_form(line, s.tags).strip())
        else:
            s, t = items[0]
            # Wrap with each tag as an unbreakable zero-width character, so a
            # line break can never land inside `<font color="...">` or
            # `{\fnArial Black}`; restore the tags afterwards.
            t = _to_wrap_form(t, s.tags).strip()
            # A "CJK" target can still emit CJK-free lines (names, numbers,
            # untranslated codes). Wrapping those by character splits words in
            # half, so pick the wrapper from the actual output, not the target.
            use_cjk = cjk and HAS_CJK_RE.search(t) is not None
            body = (wrap_cjk(t, width, max_lines) if use_cjk
                    else wrap_latin(t, int(width * 2), max_lines))
            if s.dashed and body:
                body[0] = _with_dash(body[0], s.dash_inside)
            lines = [_from_wrap_form(line, s.tags) for line in body]

        new.append(Cue(cue.index, cue.start, cue.end, cue.rest, lines, cue.dialect))

    return new
