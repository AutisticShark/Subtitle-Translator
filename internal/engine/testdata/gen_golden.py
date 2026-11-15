"""Generates python_golden.json.gz: outputs of the original Python engine
(srt_translate.py and subtitle_formats.py) for seeded random inputs, replayed
by golden_test.go against the Go port.

Run from the repository root with a checkout of the pre-Go implementation:

    git worktree add --detach .migration/python-reference 840b559
    python -I internal/engine/testdata/gen_golden.py internal/engine/testdata/python_golden.json.gz .migration/python-reference

The generated file is the durable artifact; normal Go tests do not require
Python or the reference checkout. Remove the temporary worktree afterward.
"""
import gzip, json, os, random, sys
if len(sys.argv) != 3:
    raise SystemExit("Usage: gen_golden.py OUTPUT_GZIP LEGACY_CHECKOUT")
sys.path.insert(0, os.path.abspath(sys.argv[2]))
import srt_translate as st
from subtitle_formats import parse_subtitle, SubtitleFormatError

rnd = random.Random(12345)
TOK = ["<i>", "</i>", "{\\an8}", "{\\p1}", "{\\p0}", "{\\fnArial Black}", "text", "Hello", " ", "  ", "\\h", "\\n", "\\N",
       "⟦", "⟧", "⟦1⟧", "⟦ 0 ⟧", "&amp;", "&lt;i&gt;", "&#x27e6;", "&nbsp;", "\U000f0001", "-", "–", "—", "- ", "-10", "10",
       " - ", "<00:00:01.500>", "a", "中", "你好", "，", "。", "（", "）", "한글", "ｱ", "Ａ", "!", "?", "\u00a0", "\u3000", "<font color=\"#fff\">", "</font>", "<c.x>", "{y:i}", "{", "}", "<", ">", "&", "\u1680", "\x1c"]
def rs(n=6):
    return "".join(rnd.choice(TOK) for _ in range(rnd.randint(0, n)))

cases = {"mask": [], "segment": [], "wrapcjk": [], "wraplatin": [], "rebuild": [], "numbered": [], "srt": [], "docs": [], "placeholders": [], "wrapform": []}

for _ in range(400):
    s = rs(8)
    for ass in (False, True):
        for ent in (False, True):
            m, tags = st.mask_tags(s, ass, ent)
            cases["mask"].append({"s": s, "ass": ass, "ent": ent, "masked": m, "tags": tags})

for _ in range(400):
    lines = [rs(5) for _ in range(rnd.randint(0, 4))]
    dialect = rnd.choice(["", "ass", "vtt"])
    cue = st.Cue(1, "a", "b", "", lines, dialect)
    segs = st.segment_cue(cue, 0)
    cases["segment"].append({"lines": lines, "dialect": dialect,
        "segs": [{"text": s.text, "tags": s.tags, "dashed": s.dashed, "inside": s.dash_inside} for s in segs]})
    outs = []
    for s in segs:
        r = rnd.random()
        if r < 0.5: outs.append(s.text)
        elif r < 0.7: outs.append(rs(6))
        else: outs.append(s.text[::-1] if rnd.random() < .5 else s.text + " " + rs(4))
    lang = rnd.choice(["es", "zh-TW", "ja", "ko", "zh-CN"])
    width = rnd.choice([3, 5, 8, 16, 0.5, 2.5])
    ml = rnd.choice([1, 2, 3, 4])
    rb = st.rebuild_cues([cue], segs, outs, lang, width, ml)
    cases["rebuild"].append({"lines": lines, "dialect": dialect, "outs": outs, "lang": lang, "width": width, "ml": ml, "result": rb[0].lines})

PH = "".join(chr(0xF0000 + k) for k in range(4))
WT = ["你", "好", "，", "。", "（", "a", "bc", " ", "한", "글", "A", "Ａ", "！", "）", "「", "」"] + list(PH)
for _ in range(400):
    s = "".join(rnd.choice(WT) for _ in range(rnd.randint(0, 24)))
    lim = rnd.choice([1, 2, 2.5, 3, 4, 6, 10])
    ml = rnd.choice([1, 2, 3, 4])
    cases["wrapcjk"].append({"s": s, "lim": lim, "ml": ml, "result": st.wrap_cjk(s, lim, ml)})
    lim2 = rnd.choice([1, 3, 5, 8, 12, 20])
    cases["wraplatin"].append({"s": s, "lim": lim2, "ml": ml, "result": st.wrap_latin(s, lim2, ml)})

NT = ["1", "2", "3", "\t", ".", ")", ":", "：", "、", "-", " ", "Hola", "你好", "```", "\n", "\n", "\r", "０", "4", "x", "\x85", "\v"]
for _ in range(400):
    out = "".join(rnd.choice(NT) for _ in range(rnd.randint(0, 20)))
    n = rnd.randint(1, 4)
    try:
        cases["numbered"].append({"out": out, "n": n, "result": st.parse_numbered(out, n)})
    except st.TranslationError as e:
        cases["numbered"].append({"out": out, "n": n, "error": str(e)})

ST = ["1", "2", "00:00:01,000 --> 00:00:02,000", "00:00:03.5 --> 00:00:04.25  X1:1", "text", "", "", " ", "Hello", "①", "- hi", "9", "junk", "0:00:01,0 --> 0:00:02,1", "\ufeff"]
for _ in range(300):
    raw = "\n".join(rnd.choice(ST) for _ in range(rnd.randint(0, 14)))
    if rnd.random() < .3: raw = raw.replace("\n", "\r\n")
    cues = st.parse_srt(raw)
    cases["srt"].append({"raw": raw, "cues": [{"i": c.index, "s": c.start, "e": c.end, "r": c.rest, "l": c.lines} for c in cues]})

VT = ["WEBVTT", "WEBVTT - x", "Kind: captions", "", "", "NOTE hi", "id1", "00:01.000 --> 00:02.000", "00:01.000 --> 00:02.000 align:start", "1:00:01.000 --> 1:00:02.000", "Hello &amp; bye", "<i>x</i>", "- a", "- b", "STYLE", "::cue{}", "00:03.00 --> 00:04.000", "&nbsp;"]
docs_in = []
for _ in range(150):
    raw = "\n".join(rnd.choice(VT) for _ in range(rnd.randint(1, 16)))
    if rnd.random() < .7: raw = "WEBVTT\n" + raw
    if rnd.random() < .2: raw = raw.replace("\n", "\r\n")
    docs_in.append(("vtt", raw))
AH = "[Script Info]\nTitle: x\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n"
D = "Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,"
AT = [D + "{\\an8}Hello\\Nworld", "Dialogue:0,0:00:01.00,0:00:03.00,Default,,0,0,0,,a, b\\h c\\n d", D + "{\\p1}m 0 0 l 1 1{\\p0}", D, "Comment: 0,a,b,c,d,0,0,0,,x", "[Fonts]", "Dialogue: bad", "Format: Start, Text", "- a\\N- b", D + "- a\\N- b", "", " " + D + "indented", "[Events]", AH.split("\n")[4]]
for _ in range(150):
    raw = (AH if rnd.random() < .8 else "") + "\n".join(rnd.choice(AT) for _ in range(rnd.randint(0, 8)))
    if rnd.random() < .2: raw = raw.replace("\n", "\r\n")
    docs_in.append((rnd.choice(["ass", "ssa"]), raw))
for _ in range(150):
    raw = "\n".join(rnd.choice(ST) for _ in range(rnd.randint(0, 14)))
    if rnd.random() < .3: raw = raw.replace("\n", "\r\n")
    docs_in.append(("srt", raw))

docs = []
for fmt, raw in docs_in:
    entry = {"fmt": fmt, "raw": raw}
    try:
        doc = parse_subtitle(raw.encode("utf-8"), "." + fmt)
        entry["roundtrip"] = doc.to_bytes().decode("utf-8")
        segs = []
        for i, c in enumerate(doc.cues):
            segs.extend(st.segment_cue(c, i))
        out = st.translate_segments(segs, st.make_echo(), "zh-TW", "English", 20, 1, 1, st.Throttle(), {}, 1)
        cues = st.rebuild_cues(doc.cues, segs, out, "zh-TW", 6, 2)
        entry["echo"] = doc.clone_with_cues(cues).to_bytes().decode("utf-8")
        out = st.translate_segments(segs, st.make_echo(), "es", "English", 20, 1, 1, st.Throttle(), {}, 1)
        cues = st.rebuild_cues(doc.cues, segs, out, "es", 6, 2)
        entry["echo_es"] = doc.clone_with_cues(cues).to_bytes().decode("utf-8")
        entry["ncues"] = len(doc.cues)
    except SubtitleFormatError as e:
        entry["error"] = str(e)
    docs.append(entry)
cases["docs"] = docs

for _ in range(300):
    src = "".join(rnd.choice(["⟦0⟧", "⟦1⟧", "⟦2⟧", "a", " ", "x"]) for _ in range(rnd.randint(0, 5)))
    tr = "".join(rnd.choice(["⟦0⟧", "⟦1⟧", "⟦2⟧", "⟦ 0 ⟧", "⟦", "⟧", "⟦１⟧", "a", " ", "x"]) for _ in range(rnd.randint(0, 6)))
    cases["placeholders"].append({"src": src, "tr": tr, "match": st.placeholders_match(src, tr), "needs": st.needs_translation(st.Segment(0, tr, [], False))})
    tags = ["<i>", "</i>", "<b>"][: rnd.randint(0, 3)]
    wf = st._to_wrap_form(tr, tags)
    cases["wrapform"].append({"s": tr, "tags": tags, "result": wf, "back": st._from_wrap_form(wf, tags, True)})

with gzip.GzipFile(sys.argv[1], "wb", mtime=0) as f:
    f.write(json.dumps(cases, ensure_ascii=True).encode("ascii"))
print({k: len(v) for k, v in cases.items()})
