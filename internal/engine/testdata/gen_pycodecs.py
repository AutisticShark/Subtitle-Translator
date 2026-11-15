"""Generate internal/engine/pycodecs_data.go: Python-exact legacy decoders.

Usage: python -I gen_pycodecs.py <whatwg-dump-dir> <out.go>

The dump dir holds whatwg_<label>.txt files written by the Go test
TestDumpWHATWGForPyCodecs (run with PYCODECS_DUMP_DIR set): one line per input,
hex input, a tab, then hex UTF-8 output or ERR.
"""
import encodings
import encodings.aliases
import importlib
import os
import pkgutil
import sys
import unicodedata

dump_dir, out_path = sys.argv[1], sys.argv[2]

# Multibyte codecs are decoded with Python's token rules and validity, the
# WHATWG decoder for the mapping, and Python's own output wherever WHATWG
# differs.
MULTIBYTE = {
    "gbk": "gbk", "gb2312": "gbk",
    "big5": "big5", "cp950": "big5", "big5hkscs": "big5",
    "shift_jis": "shift_jis", "cp932": "shift_jis",
    "euc_kr": "euc-kr", "cp949": "euc-kr",
    "euc_jp": "euc-jp",
}
# Decoded by hand-written code in decode.go.
SPECIAL = {"utf_8", "utf_8_sig", "utf_16", "utf_16_le", "utf_16_be",
           "utf_32", "utf_32_le", "utf_32_be", "ascii", "latin_1"}
UNDEFINED = "￾"


def load_dump(label):
    table = {}
    with open(os.path.join(dump_dir, f"whatwg_{label}.txt"), encoding="ascii") as f:
        for line in f:
            h, out = line.rstrip("\n").split("\t")
            table[bytes.fromhex(h)] = None if out == "ERR" else bytes.fromhex(out).decode()
    return table


def py(raw, name):
    try:
        return raw.decode(name), None
    except UnicodeDecodeError as e:
        return None, e.reason


def go_string(s):
    out = ['"']
    for ch in s:
        cp = ord(ch)
        if (ch in '"\\' or cp < 0x20 or cp == 0x7f or not ch.isprintable()
                or unicodedata.category(ch).startswith("M")
                or 0xd800 <= cp <= 0xdfff or 0xe000 <= cp <= 0xf8ff
                or cp >= 0xf0000 or cp in (0xfffe, 0xfeff)):
            out.append(f"\\u{cp:04x}" if cp <= 0xffff else f"\\U{cp:08x}")
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def ranges(codes):
    out = []
    for c in sorted(codes):
        if out and out[-1][1] + 1 == c:
            out[-1][1] = c
        else:
            out.append([c, c])
    return out


def split_runs(ov, minimum=4):
    """Split overrides into runs of consecutive codes that map to consecutive
    single code points ([start, end, first rune]) and the remaining map."""
    runs, rest, codes = [], {}, sorted(ov)
    i = 0
    while i < len(codes):
        j = i
        while (j + 1 < len(codes) and codes[j + 1] == codes[j] + 1
               and len(ov[codes[j]]) == 1 and len(ov[codes[j + 1]]) == 1
               and ord(ov[codes[j + 1]]) == ord(ov[codes[j]]) + 1):
            j += 1
        if j - i + 1 >= minimum:
            runs.append((codes[i], codes[j], ord(ov[codes[i]])))
        else:
            for c in codes[i:j + 1]:
                rest[c] = ov[c]
        i = j + 1
    return runs, rest


# ---- single-byte charmap codecs ---------------------------------------------
single = {}
for mod in sorted(m.name for m in pkgutil.iter_modules(encodings.__path__)):
    try:
        module = importlib.import_module("encodings." + mod)
    except Exception:
        continue
    table = getattr(module, "decoding_table", None)
    if not isinstance(table, str) or len(table) != 256:
        continue
    for b in range(256):  # the table must be exactly what decoding does
        got, _ = py(bytes([b]), mod)
        want = None if table[b] == UNDEFINED else table[b]
        assert got == want, (mod, b, got, want)
    single[mod] = table

# ---- multibyte codecs -------------------------------------------------------
multi = {}
for name, label in MULTIBYTE.items():
    whatwg = load_dump(label)
    for b in range(0x80):
        assert py(bytes([b]), name)[0] == chr(b), (name, b)
    kinds, overrides, valid2, valid3 = [], {}, [], []
    for b in range(0x80, 0x100):
        text, reason = py(bytes([b]), name)
        if text is not None:
            kinds.append("s")
            if whatwg.get(bytes([b])) != text:
                overrides[b] = text
            continue
        if reason != "incomplete multibyte sequence":
            kinds.append("x")
            continue
        length = 2
        for t in range(256):
            if name == "euc_kr" and (b, t) == (0xa4, 0xd4):
                continue  # the 8-byte make-up sequence, handled separately
            if py(bytes([b, t]), name)[1] == "incomplete multibyte sequence":
                length = 3
        if length == 3:
            assert all(py(bytes([b, t]), name)[0] is None for t in range(256)), (name, b)
        kinds.append(str(length))
        if length == 2:
            for t in range(256):
                text2, _ = py(bytes([b, t]), name)
                if text2 is not None:
                    code = b << 8 | t
                    valid2.append(code)
                    if whatwg.get(bytes([b, t])) != text2:
                        overrides[code] = text2
        else:
            for t in range(256):
                for u in range(256):
                    seq = bytes([b, t, u])
                    text3, _ = py(seq, name)
                    if text3 is not None:
                        code = b << 16 | t << 8 | u
                        valid3.append(code)
                        if whatwg.get(seq) != text3:
                            overrides[code] = text3
    multi[name] = (label, "".join(kinds), ranges(valid2), ranges(valid3), overrides)

# GB18030: a lead byte starts a 2-byte sequence, or a 4-byte one when the
# second byte is 0x30..0x39. Four-byte sequences are keyed by their linear
# index ((b1-0x80)*10 + b2-0x30)*1260 + (b3-0x81)*10 + b4-0x30.
def gb_linear(b1, b2, b3, b4):
    return ((b1 - 0x80) * 10 + b2 - 0x30) * 1260 + (b3 - 0x81) * 10 + b4 - 0x30

gb_whatwg = load_dump("gb18030")
gb_kinds, gb_valid2, gb_valid4, gb_over, gb_over4 = [], [], [], {}, {}
for b in range(0x80):
    assert py(bytes([b]), "gb18030")[0] == chr(b)
for b in range(0x80, 0x100):
    text, reason = py(bytes([b]), "gb18030")
    if text is not None:
        gb_kinds.append("s")
        if gb_whatwg.get(bytes([b])) != text:
            gb_over[b] = text
        continue
    if reason != "incomplete multibyte sequence":
        gb_kinds.append("x")
        continue
    gb_kinds.append("g")
    incomplete = set()
    for t in range(256):
        text2, reason2 = py(bytes([b, t]), "gb18030")
        if text2 is not None:
            gb_valid2.append(b << 8 | t)
            if gb_whatwg.get(bytes([b, t])) != text2:
                gb_over[b << 8 | t] = text2
        elif reason2 == "incomplete multibyte sequence":
            incomplete.add(t)
    assert incomplete == set(range(0x30, 0x3a)), (b, sorted(incomplete))
    for t in range(0x30, 0x3a):
        # Outside 81..FE / 30..39 the third and fourth bytes are illegal.
        for u, v in ((0x80, 0x30), (0xff, 0x30), (0x81, 0x2f), (0x81, 0x3a), (0x20, 0x41)):
            assert py(bytes([b, t, u, v]), "gb18030")[0] is None
        for u in range(0x81, 0xff):
            for v in range(0x30, 0x3a):
                seq = bytes([b, t, u, v])
                text4, _ = py(seq, "gb18030")
                if text4 is not None:
                    index = gb_linear(b, t, u, v)
                    gb_valid4.append(index)
                    if gb_whatwg.get(seq) != text4:
                        gb_over4[index] = text4
multi["gb18030"] = ("gb18030", "".join(gb_kinds), ranges(gb_valid2), [], gb_over)
gb_extra = (ranges(gb_valid4), gb_over4)

# EUC-KR's KS X 1001:1998 Annex 3 make-up sequence: A4 D4 A4 <cho> A4 <jung>
# A4 <jong> composes U+AC00 + cho*588 + jung*28 + jong. Each slot is checked
# independently against its own table; derive those tables from Python.
def makeup(c, j, k):
    return py(bytes([0xa4, 0xd4, 0xa4, c, 0xa4, j, 0xa4, k]), "euc_kr")[0]

base = next((c, j, k) for c in range(0xa1, 0xff) for j in range(0xa1, 0xff)
            for k in range(0xa1, 0xff) if makeup(c, j, k))
jamo = []
for slot, divisor, modulo in ((0, 588, 1 << 30), (1, 28, 21), (2, 1, 28)):
    table = {}
    for value in range(256):
        args = list(base)
        args[slot] = value
        text = makeup(*args)
        if text is not None:
            table[value] = (ord(text) - 0xac00) // divisor % modulo
    jamo.append(table)
for c in range(256):  # the slots really are independent
    for j in (0xa1, 0xbf, 0xd4):
        for k in (0xa1, 0xd4, 0xbe):
            want = None
            if c in jamo[0] and j in jamo[1] and k in jamo[2]:
                want = chr(0xac00 + jamo[0][c] * 588 + jamo[1][j] * 28 + jamo[2][k])
            assert makeup(c, j, k) == want, (c, j, k)
assert py(b"\xa4\xd4", "euc_kr")[1] == "incomplete multibyte sequence"
assert py(b"\xa4\xd4\xa4\xa1\xa4\xbf", "euc_kr")[1] == "incomplete multibyte sequence"

# ---- aliases: Python's normalized alias table plus the module names ---------
known = set(single) | set(multi) | SPECIAL
aliases = {k: v for k, v in encodings.aliases.aliases.items() if v in known}
for name in known:
    aliases.setdefault(name, name)

# ---- emit -------------------------------------------------------------------
w = ["// Code generated by testdata/gen_pycodecs.py from the codecs of Python "
     + sys.version.split()[0] + "; DO NOT EDIT.", "", "package engine", ""]
w.append("// pyCodecAliases maps a normalized codec name to Python's codec module name.")
w.append("var pyCodecAliases = map[string]string{")
for k in sorted(aliases):
    w.append(f"\t{go_string(k)}: {go_string(aliases[k])},")
w.append("}")
w.append("")
w.append("// pySingleByteTables holds each charmap codec's 256-entry decoding table;")
w.append("// U+FFFE marks an undefined byte, as in Python.")
w.append("var pySingleByteTables = map[string]string{")
for k in sorted(single):
    w.append(f"\t{go_string(k)}: {go_string(single[k])},")
w.append("}")
w.append("")
w.append("// pyMultiByteCodecs describes each multibyte codec relative to the WHATWG")
w.append("// decoder that supplies its mapping (see pycodecs.go).")
w.append("var pyMultiByteCodecs = map[string]*pyMultiByte{")
stats = []
for k in sorted(multi):
    label, kinds, r2, r3, ov = multi[k]
    stats.append((k, len(r2), len(r3), len(ov)))
    w.append(f"\t{go_string(k)}: {{")
    w.append(f"\t\twhatwg: {go_string(label)},")
    w.append(f"\t\tkinds:  {go_string(kinds)},")
    for fname, rs in (("valid2", r2), ("valid3", r3)):
        if not rs:
            continue
        w.append(f"\t\t{fname}: []uint32{{")
        flat = [f"0x{x:x}" for r in rs for x in r]
        for i in range(0, len(flat), 12):
            w.append("\t\t\t" + ", ".join(flat[i:i + 12]) + ",")
        w.append("\t\t},")
    if k == "gb18030":
        r4, ov4 = gb_extra
        w.append("\t\tvalid4: []uint32{")
        flat = [f"0x{x:x}" for r in r4 for x in r]
        for i in range(0, len(flat), 12):
            w.append("\t\t\t" + ", ".join(flat[i:i + 12]) + ",")
        w.append("\t\t},")
        if ov4:
            w.append("\t\toverrides4: map[uint32]string{")
            for code in sorted(ov4):
                w.append(f"\t\t\t0x{code:x}: {go_string(ov4[code])},")
            w.append("\t\t},")
        stats.append(("gb18030/4", len(r4), 0, len(ov4)))
    if k == "euc_kr":
        w.append("\t\tmakeup: &[3]map[byte]int{")
        for table in jamo:
            w.append("\t\t\t{" + ", ".join(f"0x{b:x}: {v}" for b, v in sorted(table.items())) + "},")
        w.append("\t\t},")
    runs, rest = split_runs(ov)
    if runs:
        w.append("\t\toverrideRuns: []uint32{")
        for a, b, r in runs:
            w.append(f"\t\t\t0x{a:x}, 0x{b:x}, 0x{r:x},")
        w.append("\t\t},")
    if rest:
        w.append("\t\toverrides: map[uint32]string{")
        for code in sorted(rest):
            w.append(f"\t\t\t0x{code:x}: {go_string(rest[code])},")
        w.append("\t\t},")
    w.append("\t},")
w.append("}")
with open(out_path, "w", encoding="utf-8", newline="\n") as f:
    f.write("\n".join(w) + "\n")
print("single-byte codecs:", len(single), "aliases:", len(aliases))
for s in stats:
    print("  %-10s ranges2=%d ranges3=%d overrides=%d" % s)
