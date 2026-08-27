"""Subtitle format adapters used by both the web app and tests.

The translation engine operates on the existing :class:`srt_translate.Cue`
shape.  Adapters retain the format-specific envelope and only replace cue text.
"""

from __future__ import annotations

import copy
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Sequence

from srt_translate import Cue, TIMING_RE, parse_srt


SUPPORTED_EXTENSIONS = {".srt", ".vtt", ".ass", ".ssa"}


class SubtitleFormatError(ValueError):
    pass


@dataclass
class SubtitleDocument:
    format: str
    cues: list[Cue]
    newline: str = "\n"
    bom: bool = False
    metadata: dict = field(default_factory=dict)

    def clone_with_cues(self, cues: Sequence[Cue]) -> "SubtitleDocument":
        clone = copy.copy(self)
        clone.cues = list(cues)
        clone.metadata = copy.deepcopy(self.metadata)
        return clone

    def render(self) -> str:
        if self.format == "srt":
            return _render_srt(self)
        if self.format == "vtt":
            return _render_vtt(self)
        if self.format in {"ass", "ssa"}:
            return _render_ass(self)
        raise SubtitleFormatError(f"Unsupported format: {self.format}")

    def to_bytes(self) -> bytes:
        text = self.render()
        return (("\ufeff" if self.bom else "") + text).encode("utf-8")


def load_subtitle(path: Path, encoding: str = "utf-8") -> SubtitleDocument:
    raw = path.read_bytes()
    return parse_subtitle(raw, path.suffix, encoding=encoding)


def parse_subtitle(raw: bytes, extension: str, encoding: str = "utf-8") -> SubtitleDocument:
    ext = extension.lower()
    if ext not in SUPPORTED_EXTENSIONS:
        raise SubtitleFormatError(
            f"Unsupported subtitle type {ext or '(none)'}. Use SRT, VTT, ASS, or SSA."
        )
    bom = raw.startswith(b"\xef\xbb\xbf")
    try:
        text = raw.decode("utf-8-sig" if bom else encoding)
    except (LookupError, UnicodeDecodeError) as exc:
        raise SubtitleFormatError(f"Could not decode subtitle as {encoding}: {exc}") from exc
    newline = "\r\n" if b"\r\n" in raw[:8192] else "\n"
    normalized = text.replace("\r\n", "\n").replace("\r", "\n")

    if ext == ".srt":
        cues = parse_srt(normalized)
        doc = SubtitleDocument("srt", cues, newline, bom)
    elif ext == ".vtt":
        doc = _parse_vtt(normalized, newline, bom)
    else:
        doc = _parse_ass(normalized, ext[1:], newline, bom)
    if not doc.cues:
        raise SubtitleFormatError("No subtitle cues were found in the file.")
    return doc


def _render_srt(doc: SubtitleDocument) -> str:
    out: list[str] = []
    for number, cue in enumerate(doc.cues, 1):
        out.extend((str(cue.index or number), f"{cue.start} --> {cue.end}{cue.rest}"))
        out.extend(cue.lines or [""])
        out.append("")
    return doc.newline.join(out)


VTT_TIMING_RE = re.compile(
    r"(?P<start>(?:\d{1,2}:)?\d{2}:\d{2}[.,]\d{3})\s*-->\s*"
    r"(?P<end>(?:\d{1,2}:)?\d{2}:\d{2}[.,]\d{3})(?P<rest>.*)"
)


def _parse_vtt(text: str, newline: str, bom: bool) -> SubtitleDocument:
    lines = text.lstrip("\ufeff").split("\n")
    if not lines or not lines[0].strip().upper().startswith("WEBVTT"):
        raise SubtitleFormatError("WebVTT file must begin with WEBVTT.")

    cues: list[Cue] = []
    blocks: list[dict] = []
    i = 1
    while i < len(lines):
        while i < len(lines) and not lines[i].strip():
            i += 1
        if i >= len(lines):
            break
        start = i
        while i < len(lines) and lines[i].strip():
            i += 1
        block = lines[start:i]
        timing_i = next((n for n, line in enumerate(block) if VTT_TIMING_RE.fullmatch(line.strip())), None)
        if timing_i is None:
            blocks.append({"kind": "raw", "lines": block})
            continue
        match = VTT_TIMING_RE.fullmatch(block[timing_i].strip())
        assert match is not None
        cue_index = len(cues)
        cues.append(Cue(cue_index + 1, match.group("start"), match.group("end"),
                        match.group("rest"), block[timing_i + 1 :]))
        blocks.append({
            "kind": "cue",
            "cue": cue_index,
            "identifier": block[:timing_i],
        })
    return SubtitleDocument("vtt", cues, newline, bom, {
        "header": lines[0],
        "blocks": blocks,
    })


def _render_vtt(doc: SubtitleDocument) -> str:
    out = [doc.metadata.get("header", "WEBVTT"), ""]
    for block in doc.metadata.get("blocks", []):
        if block["kind"] == "raw":
            out.extend(block["lines"])
        else:
            cue = doc.cues[block["cue"]]
            out.extend(block.get("identifier", []))
            out.append(f"{cue.start} --> {cue.end}{cue.rest}")
            out.extend(cue.lines or [""])
        out.append("")
    return doc.newline.join(out)


def _parse_ass(text: str, fmt: str, newline: str, bom: bool) -> SubtitleDocument:
    lines = text.lstrip("\ufeff").split("\n")
    in_events = False
    fields: list[str] = []
    cues: list[Cue] = []
    records: list[dict] = []

    for line_i, line in enumerate(lines):
        stripped = line.strip()
        if stripped.startswith("[") and stripped.endswith("]"):
            in_events = stripped.lower() == "[events]"
            continue
        if not in_events:
            continue
        if stripped.lower().startswith("format:"):
            fields = [part.strip().lower() for part in line.split(":", 1)[1].split(",")]
            continue
        if not stripped.lower().startswith("dialogue:"):
            continue
        if not fields:
            raise SubtitleFormatError("ASS/SSA [Events] section has no Format line.")
        colon = line.index(":")
        content = line[colon + 1 :]
        leading_space = content[: len(content) - len(content.lstrip())]
        values = content.lstrip().split(",", len(fields) - 1)
        if len(values) != len(fields) or "text" not in fields:
            raise SubtitleFormatError("Malformed ASS/SSA Dialogue line.")
        text_i = fields.index("text")
        start_i = fields.index("start") if "start" in fields else None
        end_i = fields.index("end") if "end" in fields else None
        cue_i = len(cues)
        cue_text = values[text_i].replace("\\N", "\n").replace("\\n", "\n")
        cues.append(Cue(cue_i + 1,
                        values[start_i] if start_i is not None else "",
                        values[end_i] if end_i is not None else "", "", cue_text.split("\n")))
        records.append({
            "line": line_i,
            "values": values,
            "text_index": text_i,
            "prefix": line[: colon + 1] + leading_space,
        })

    return SubtitleDocument(fmt, cues, newline, bom, {"lines": lines, "records": records})


def _render_ass(doc: SubtitleDocument) -> str:
    lines = list(doc.metadata["lines"])
    for cue, record in zip(doc.cues, doc.metadata["records"]):
        values = list(record["values"])
        # ASS uses literal \N for an explicit subtitle line break.
        values[record["text_index"]] = "\\N".join(cue.lines)
        lines[record["line"]] = record["prefix"] + ",".join(values)
    return doc.newline.join(lines)


def translated_filename(source_name: str, language_suffix: str) -> str:
    path = Path(source_name)
    stem = re.sub(r"\.(en|eng|english)$", "", path.stem, flags=re.I)
    return f"{stem}{language_suffix}{path.suffix.lower()}"
