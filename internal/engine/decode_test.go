package engine

import (
	"strings"
	"testing"
)

func encodeUTF32(text string, bigEndian, bom bool) []byte {
	var raw []byte
	put := func(v rune) {
		b := []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
		if bigEndian {
			b[0], b[1], b[2], b[3] = b[3], b[2], b[1], b[0]
		}
		raw = append(raw, b...)
	}
	if bom {
		put(0xfeff)
	}
	for _, r := range text {
		put(r)
	}
	return raw
}

func TestDecodeUTF32(t *testing.T) {
	const text = "你好 😀 café"
	cases := []struct {
		encoding string
		raw      []byte
	}{
		{"utf-32", encodeUTF32(text, false, true)},
		{"UTF-32", encodeUTF32(text, true, true)},
		{"utf32", encodeUTF32(text, false, false)},
		{"utf-32-le", encodeUTF32(text, false, false)},
		{"utf_32_be", encodeUTF32(text, true, false)},
	}
	for _, tc := range cases {
		got, err := decodeBytes(tc.raw, tc.encoding)
		if err != nil || got != text {
			t.Errorf("%s: %q, %v", tc.encoding, got, err)
		}
	}
	for _, raw := range [][]byte{{1, 2, 3}, {0xff, 0xff, 0x11, 0x00}, {0x00, 0xd8, 0x00, 0x00}} {
		if _, err := decodeBytes(raw, "utf-32-le"); err == nil {
			t.Errorf("%v accepted", raw)
		}
	}
}

func TestDecodeUTF8Errors(t *testing.T) {
	for raw, reason := range map[string]string{
		"\xff":       "invalid start byte",
		"a\x80":      "invalid start byte",
		"\xc3(":      "invalid continuation byte",
		"ab\xe4\xbd": "unexpected end of data",
	} {
		_, err := decodeBytes([]byte(raw), "utf-8")
		if err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("%q: %v, want %s", raw, err, reason)
		}
	}
	got, err := decodeBytes([]byte("\xef\xbb\xbfabc"), "utf-8-sig")
	if err != nil || got != "abc" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestDecodeUnknownAndHyphenatedLabels(t *testing.T) {
	if _, err := decodeBytes([]byte("x"), "klingon"); err == nil || !strings.Contains(err.Error(), "unknown encoding: klingon") {
		t.Fatalf("got %v", err)
	}
	for _, label := range []string{"Windows-1251", "cp1251", "koi8-r", "ISO-8859-2", "latin2", "EUC_KR", "euc-jp", "gb2312", "big5-hkscs"} {
		if _, err := decodeBytes([]byte("abc"), label); err != nil {
			t.Errorf("%s: %v", label, err)
		}
	}
	got, err := decodeBytes([]byte{0xcf, 0xf0, 0xe8, 0xe2, 0xe5, 0xf2}, "cp1251")
	if err != nil || got != "Привет" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestRenderRejectsBrokenEnvelopes(t *testing.T) {
	if _, err := (&Document{Format: "ass"}).Render(); err == nil {
		t.Error("an ASS document without an envelope must not render")
	}
	if _, err := (&Document{Format: "ass"}).ToBytes(); err == nil {
		t.Error("ToBytes must surface the render error")
	}
	doc := mustParse(t, assHeader+assLine("Hi"), ".ass")
	if _, err := doc.CloneWithCues(nil).Render(); err == nil {
		t.Error("an ASS document with missing cues must not render")
	}
	vtt := mustParse(t, "WEBVTT\n\n00:01.000 --> 00:02.000\nHi\n", ".vtt")
	if _, err := vtt.CloneWithCues(nil).Render(); err == nil {
		t.Error("a VTT document with missing cues must not render")
	}
	// A bare VTT document renders with the default header.
	out, err := (&Document{Format: "vtt", Newline: "\n"}).Render()
	if err != nil || out != "WEBVTT\n" {
		t.Errorf("%q %v", out, err)
	}
}
