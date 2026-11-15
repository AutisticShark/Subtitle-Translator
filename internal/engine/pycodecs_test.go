package engine

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Reference results from Python 3.15's codecs. The WHATWG decoders the port
// first used accepted the rejected inputs below, or decoded them differently
// (Big5 A145, Shift JIS 8160, GB18030 A3A0, ISO-8859-9 0x80, KOI8-U AE).
// The generator verified all 81 codecs exhaustively (2.48 million inputs:
// every byte and byte pair, every EUC-JP triple and GB18030 quadruple, EUC-KR
// make-up sequences and random streams) against Python with no difference.
func TestLegacyDecodingMatchesPython(t *testing.T) {
	cases := []struct {
		codec, input, want string
		ok                 bool
	}{
		{"cp1252", "80", "€", true},
		{"cp1252", "81", "", false},
		{"windows-1252", "636166e9", "café", true},
		{"cp1255", "ca", "", false},
		{"gbk", "80", "", false},
		{"gbk", "8140", "丂", true},
		{"gbk", "81308130", "", false},
		{"gb2312", "8140", "", false},
		{"gb2312", "a1a4", "・", true},
		{"gb18030", "81308130", "\u0080", true},
		{"gb18030", "8431a437", "�", true},
		{"gb18030", "80", "", false},
		{"gb18030", "a3a0", "", true},
		{"gb18030", "aaa1", "", true},
		{"big5", "a145", "•", true},
		{"big5", "8740", "", false},
		{"cp950", "a145", "‧", true},
		{"big5hkscs", "8862", "Ê̄", true},
		{"shift_jis", "8160", "〜", true},
		{"shift_jis", "80", "", false},
		{"cp932", "80", "\u0080", true},
		{"cp932", "a0", "", true},
		{"cp932", "f040", "", true},
		{"euc_kr", "a4d4a4a1a4bfa4d4", "가", true},
		{"euc_kr", "a4d4", "", false},
		{"euc_kr", "8141", "", false},
		{"cp949", "8141", "갂", true},
		{"euc_jp", "a1c1", "〜", true},
		{"euc_jp", "8fb0a1", "丂", true},
		{"koi8_u", "ae", "╝", true},
		{"iso8859_9", "80", "\u0080", true},
		{"iso8859_2", "80", "\u0080", true},
		{"latin5", "d0", "Ğ", true},
	}
	for _, c := range cases {
		raw, _ := hex.DecodeString(c.input)
		got, err := decodeBytes(raw, c.codec)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("%s %s: got %q, %v; want %q", c.codec, c.input, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("%s %s: decoded to %q, Python rejects it", c.codec, c.input, got)
		}
	}
}

// A rejected byte stops decoding with Python's message instead of changing
// the text; a valid stream mixing ASCII and multibyte text decodes in order.
func TestLegacyDecodingErrorsAndStreams(t *testing.T) {
	_, err := decodeBytes([]byte("ok \x80 no"), "gbk")
	if err == nil || !strings.Contains(err.Error(), "'gbk' codec can't decode byte 0x80 in position 3") {
		t.Fatalf("got %v", err)
	}
	_, err = decodeBytes([]byte("ab\x81"), "cp1252")
	if err == nil || !strings.Contains(err.Error(), "character maps to <undefined>") {
		t.Fatalf("got %v", err)
	}
	got, err := decodeBytes([]byte("1\r\n\xa7\x41\xa6\x6e A\xa1\x45"), "big5")
	if err != nil || got != "1\r\n你好 A•" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = decodeBytes([]byte("x\x81\x30\x81\x30\xc4\xe3y"), "gb18030")
	if err != nil || got != "x\u0080你y" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// Codec names resolve as Python's codecs.lookup does; unknown names, names
// with '.', and codecs without an exact decoder are refused.
func TestCodecNameResolutionMatchesPython(t *testing.T) {
	for name, want := range map[string]string{
		"UTF-8": "utf_8", "U8": "utf_8", "cp65001": "utf_8", "utf-8-sig": "utf_8_sig",
		"unicodelittleunmarked": "utf_16_le", "Latin1": "latin_1", "iso_ir_100": "latin_1",
		"US-ASCII": "ascii", "Windows 1252": "cp1252", "1252": "cp1252", "latin5": "iso8859_9",
		"chinese": "gb2312", "936": "gbk", "big5-tw": "big5", "ms950": "cp950", "hkscs": "big5hkscs",
		"  shift-jis  ": "shift_jis", "Shift JIS": "shift_jis", "mskanji": "cp932",
		"ksc5601": "cp949", "uhc": "cp949", "ujis": "euc_jp", "tis-620": "tis_620",
		"utf８": "utf_8", "UTF--8": "utf_8",
		"utf.8": "", "iso8859.9": "", "x-mac-cyrillic": "", "bogus": "", "ｕｔｆ８": "",
	} {
		if got := pythonCodec(name); got != want {
			t.Errorf("pythonCodec(%q) = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"utf.8", "x-mac-cyrillic", "hz", "johab", "utf-7", "iso-2022-jp"} {
		if _, err := decodeBytes([]byte("A"), name); err == nil || !strings.HasPrefix(err.Error(), "unknown encoding") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
