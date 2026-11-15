package engine

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
)

const assHeader = "[Script Info]\nTitle: Demo\n\n[Events]\n" +
	"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n"

func assLine(text string) string {
	return "Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,," + text + "\n"
}

func TestParseSRTToleratesBOMJunkMissingIndicesAndInternalBlankLines(t *testing.T) {
	source := "\ufeffjunk\r\n" +
		"00:00:01,000 --> 00:00:02,000  X1:10\r\n" +
		"First line\r\n\r\ncontinued\r\n\r\n" +
		"9\r\n00:00:03.000 --> 00:00:04.500\r\nLast line"
	cues := ParseSRT(source)

	if len(cues) != 2 {
		t.Fatalf("got %d cues", len(cues))
	}
	equal(t, cues[0].Index, 1)
	equal(t, cues[0].Rest, "  X1:10")
	equal(t, cues[0].Lines, []string{"First line", "", "continued"})
	equal(t, cues[1].Index, 9)
	equal(t, cues[1].Start, "00:00:03.000")
	equal(t, cues[1].End, "00:00:04.500")
	equal(t, cues[1].Lines, []string{"Last line"})
}

func TestParseSRTNonASCIIDigitLinesAreNotIndices(t *testing.T) {
	source := "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n" +
		"①\n00:00:03,000 --> 00:00:04,000\nWorld\n\n" +
		"²\n00:00:05,000 --> 00:00:06,000\nAgain\n"
	cues := ParseSRT(source)

	if len(cues) != 3 {
		t.Fatalf("got %d cues", len(cues))
	}
	equal(t, []int{cues[0].Index, cues[1].Index, cues[2].Index}, []int{1, 2, 3})
	equal(t, cues[1].Lines[0], "World")
	doc := mustParse(t, source, ".srt")
	equal(t, len(doc.Cues), 3)
}

func TestParseSRTHeaderWithoutBlankSeparatorStartsANewCue(t *testing.T) {
	cues := ParseSRT("1\n00:00:01,000 --> 00:00:02,000\nHello\n" +
		"2\n00:00:03,000 --> 00:00:04,000\nWorld\n" +
		"00:00:05,000 --> 00:00:06,000\nAgain\n")

	equal(t, [][]string{cues[0].Lines, cues[1].Lines, cues[2].Lines},
		[][]string{{"Hello"}, {"World"}, {"Again"}})
	equal(t, []int{cues[0].Index, cues[1].Index, cues[2].Index}, []int{1, 2, 3})
}

func TestParseSRTKeepsBlankLinesInsideACueAndStripsTrailingOnes(t *testing.T) {
	cues := ParseSRT("1\n00:00:01,000 --> 00:00:02,000\nA\n\nB\n\n\n2\n00:00:03,000 --> 00:00:04,000\nC\n\n\n")

	equal(t, cues[0].Lines, []string{"A", "", "B"})
	equal(t, cues[1].Lines, []string{"C"})
}

func TestParseSRTShortTimestampsAndSearchSemantics(t *testing.T) {
	// The timing regexp is searched, not anchored: text before it is ignored.
	cues := ParseSRT("junk 0:00:01.5 --> 0:00:02.25 tail\nHi\n")

	equal(t, len(cues), 1)
	equal(t, cues[0].Start, "0:00:01.5")
	equal(t, cues[0].End, "0:00:02.25")
	equal(t, cues[0].Rest, " tail")
}

func TestParseSRTNoCues(t *testing.T) {
	equal(t, len(ParseSRT("not subtitles")), 0)
	equal(t, len(ParseSRT("")), 0)
}

func TestSRTRoundTripKeepsIndicesFromZeroAndCRLF(t *testing.T) {
	source := "0\n00:00:01,000 --> 00:00:02,000\nA\n\n1\n00:00:03,000 --> 00:00:04,000\nB\n"
	equal(t, mustBytes(t, mustParse(t, source, ".srt")), []byte(source))

	crlf := "1\r\n00:00:01,000 --> 00:00:03,000\r\n<i>Hello</i> world\r\n"
	rendered := string(mustBytes(t, mustParse(t, crlf, ".srt")))
	equal(t, rendered, crlf)
}

func TestSRTPreservesUTF8BOMAndNewlineStyle(t *testing.T) {
	source := "\xef\xbb\xbf1\r\n00:00:01,000 --> 00:00:03,000\r\nHello\r\n\r\n"
	doc, err := ParseSubtitle([]byte(source), ".SRT", "")
	if err != nil {
		t.Fatal(err)
	}
	rendered := mustBytes(t, doc)

	if !bytes.HasPrefix(rendered, []byte("\xef\xbb\xbf")) {
		t.Fatal("BOM lost")
	}
	if strings.Contains(strings.ReplaceAll(string(rendered), "\r\n", ""), "\n") {
		t.Fatal("bare LF in CRLF output")
	}
	equal(t, doc.Newline, "\r\n")
	if !doc.BOM {
		t.Fatal("BOM flag not set")
	}
}

func TestBOMOverridesDeclaredEncoding(t *testing.T) {
	source := "\xef\xbb\xbf1\n00:00:01,000 --> 00:00:02,000\n你好\n"
	doc, err := ParseSubtitle([]byte(source), ".srt", "gbk")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, doc.Cues[0].Lines, []string{"你好"})
}

func TestUTF16WithBOMAndCRLFNewlineIsDetected(t *testing.T) {
	text := "1\r\n00:00:01,000 --> 00:00:02,000\r\nHello\r\n"
	raw := []byte{0xff, 0xfe}
	for _, unit := range utf16.Encode([]rune(text)) {
		raw = append(raw, byte(unit), byte(unit>>8))
	}
	doc, err := ParseSubtitle(raw, ".srt", "utf-16")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, doc.Newline, "\r\n")
	if !bytes.Contains(mustBytes(t, doc), []byte("\r\nHello\r\n")) {
		t.Fatal("CRLF not preserved")
	}
	// The output is UTF-8, as in the original application.
	if bytes.HasPrefix(mustBytes(t, doc), []byte{0xff, 0xfe}) {
		t.Fatal("output must be UTF-8")
	}
}

func encodeUTF16(text string, bigEndian, bom bool) []byte {
	var raw []byte
	if bom {
		if bigEndian {
			raw = append(raw, 0xfe, 0xff)
		} else {
			raw = append(raw, 0xff, 0xfe)
		}
	}
	for _, unit := range utf16.Encode([]rune(text)) {
		if bigEndian {
			raw = append(raw, byte(unit>>8), byte(unit))
		} else {
			raw = append(raw, byte(unit), byte(unit>>8))
		}
	}
	return raw
}

func TestEncodings(t *testing.T) {
	const sample = "1\n00:00:01,000 --> 00:00:02,000\n你好 😀 café\n"
	cases := []struct {
		name     string
		encoding string
		raw      []byte
		want     string
	}{
		{"default is utf-8", "", []byte(sample), sample},
		{"utf8 alias", "UTF8", []byte(sample), sample},
		{"utf-16 le with bom", "utf-16", encodeUTF16(sample, false, true), sample},
		{"utf-16 be with bom", "UTF-16", encodeUTF16(sample, true, true), sample},
		{"utf-16 without bom is little endian", "utf16", encodeUTF16(sample, false, false), sample},
		{"utf-16-le", "utf-16-le", encodeUTF16(sample, false, false), sample},
		{"utf-16-be", "utf_16_be", encodeUTF16(sample, true, false), sample},
		{"utf-8-sig", "utf-8-sig", []byte(sample), sample},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := ParseSubtitle(tc.raw, ".srt", tc.encoding)
			if err != nil {
				t.Fatal(err)
			}
			equal(t, string(mustBytes(t, doc)), tc.want)
		})
	}
}

func TestLegacyEncodings(t *testing.T) {
	// "你好" in GBK, Big5 and Shift_JIS ("こんにちは"), and "café" in Latin-1 and
	// Windows-1252 (where 0x80 is the euro sign).
	header := "1\n00:00:01,000 --> 00:00:02,000\n"
	cases := []struct {
		encoding string
		body     []byte
		want     string
	}{
		{"gbk", []byte{0xc4, 0xe3, 0xba, 0xc3}, "你好"},
		{"GB2312", []byte{0xc4, 0xe3, 0xba, 0xc3}, "你好"},
		{"cp936", []byte{0xc4, 0xe3, 0xba, 0xc3}, "你好"},
		{"gb18030", []byte{0xc4, 0xe3, 0xba, 0xc3}, "你好"},
		{"big5", []byte{0xa7, 0x41, 0xa6, 0x6e}, "你好"},
		{"shift_jis", []byte{0x82, 0xb1, 0x82, 0xf1, 0x82, 0xc9, 0x82, 0xbf, 0x82, 0xcd}, "こんにちは"},
		{"Shift-JIS", []byte{0x82, 0xb1, 0x82, 0xf1}, "こん"},
		{"euc-kr", []byte{0xbe, 0xc8, 0xb3, 0xe7}, "안녕"},
		{"latin-1", []byte{'c', 'a', 'f', 0xe9}, "café"},
		{"latin1", []byte{0x80}, "\u0080"},
		{"iso-8859-1", []byte{'c', 'a', 'f', 0xe9}, "café"},
		{"cp1252", []byte{'c', 'a', 'f', 0xe9, 0x80}, "café€"},
		{"windows-1252", []byte{0x80}, "€"},
		{"ascii", []byte("abc"), "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.encoding, func(t *testing.T) {
			raw := append([]byte(header), tc.body...)
			doc, err := ParseSubtitle(raw, ".srt", tc.encoding)
			if err != nil {
				t.Fatal(err)
			}
			equal(t, doc.Cues[0].Lines, []string{tc.want})
		})
	}
}

func TestDecodeErrorsAreSubtitleFormatErrors(t *testing.T) {
	cases := []struct {
		name     string
		raw      []byte
		encoding string
	}{
		{"invalid utf-8", []byte("\xff"), "utf-8"},
		{"truncated utf-8", []byte("ab\xe4\xbd"), "utf-8"},
		{"unknown encoding", []byte("abc"), "no-such-codec"},
		{"odd utf-16", []byte{0xff, 0xfe, 'a'}, "utf-16"},
		{"lone utf-16 surrogate", []byte{0x00, 0xd8, 'a', 0x00}, "utf-16-le"},
		{"non-ascii in ascii", []byte{0xc3, 0xa9}, "ascii"},
		{"invalid gbk", []byte{0x81}, "gbk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSubtitle(tc.raw, ".srt", tc.encoding)
			var formatErr *SubtitleFormatError
			if !errors.As(err, &formatErr) {
				t.Fatalf("got %v", err)
			}
			contains(t, err.Error(), "Could not decode subtitle as "+tc.encoding)
			equal(t, ErrorTypeName(err), "SubtitleFormatError")
		})
	}
}

func TestEmptyAndUnsupportedFilesAreRejected(t *testing.T) {
	_, err := ParseSubtitle([]byte("not subtitles"), ".srt", "")
	contains(t, err.Error(), "No subtitle cues")
	_, err = ParseSubtitle([]byte("hello"), ".txt", "")
	contains(t, err.Error(), "Unsupported subtitle type .txt")
	_, err = ParseSubtitle([]byte("hello"), "", "")
	contains(t, err.Error(), "(none)")
}

func TestExtensionIsNormalised(t *testing.T) {
	const source = "1\n00:00:01,000 --> 00:00:02,000\nHi\n"
	for _, ext := range []string{".srt", "srt", ".SRT", "Show.en.Srt", " srt "} {
		if _, err := ParseSubtitle([]byte(source), ext, ""); err != nil {
			t.Errorf("%q: %v", ext, err)
		}
	}
}

func TestCountCues(t *testing.T) {
	n, err := CountCues([]byte("1\n00:00:01,000 --> 00:00:02,000\nA\n\n2\n00:00:03,000 --> 00:00:04,000\nB\n"), "srt", "")
	if err != nil || n != 2 {
		t.Fatalf("got %d, %v", n, err)
	}
	if _, err = CountCues([]byte("junk"), ".srt", ""); err == nil {
		t.Fatal("expected an error")
	}
	if _, err = CountCues([]byte("\xff"), ".srt", "utf-8"); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestVTTPreservesHeaderIdentifierNotesAndSettings(t *testing.T) {
	source := "WEBVTT - captions\n\nNOTE generated here\n\nintro\n" +
		"00:01.000 --> 00:03.000 align:start position:10%\nHello\n\n"
	doc := mustParse(t, source, ".vtt")
	rendered := string(translateDocument(t, doc, NewEcho(), "zh-TW", 40, 2))

	if !strings.HasPrefix(rendered, "WEBVTT - captions") {
		t.Fatal(rendered)
	}
	contains(t, rendered, "NOTE generated here")
	contains(t, rendered, "intro\n00:01.000 --> 00:03.000 align:start position:10%")
	contains(t, rendered, "[zh-TW] Hello")
}

func TestVTTRoundTrips(t *testing.T) {
	for name, source := range map[string]string{
		"multi-line header": "WEBVTT\nKind: captions\nLanguage: en\n\n00:01.000 --> 00:02.000\nHello\n",
		"three-digit hours": "WEBVTT\n\n100:00:01.000 --> 100:00:02.500\nHello\n",
		"style block":       "WEBVTT\n\nSTYLE\n::cue { color: red }\n\nid-1\n00:01.000 --> 00:02.000 line:0\nA\nB\n",
	} {
		t.Run(name, func(t *testing.T) {
			doc := mustParse(t, source, ".vtt")
			equal(t, string(mustBytes(t, doc)), source)
		})
	}
	doc := mustParse(t, "WEBVTT\n\n100:00:01.000 --> 100:00:02.500\nHello\n", ".vtt")
	equal(t, doc.Cues[0].Start, "100:00:01.000")
	doc = mustParse(t, "WEBVTT\nKind: captions\nLanguage: en\n\n00:01.000 --> 00:02.000\nHello\n", ".vtt")
	equal(t, len(doc.Cues), 1)
}

func TestVTTCueDirectlyAfterTheHeaderGetsTheBlankSeparator(t *testing.T) {
	doc := mustParse(t, "WEBVTT\n00:01.000 --> 00:02.000\nHello\n", ".vtt")
	equal(t, string(mustBytes(t, doc)), "WEBVTT\n\n00:01.000 --> 00:02.000\nHello\n")
}

func TestVTTTimingEdgeCases(t *testing.T) {
	// A cue timing line must match completely; an almost timing line is text
	// of a raw block, not a cue.
	doc := mustParse(t, "WEBVTT\n\n00:01.00 --> 00:02.000\nnope\n\n00:03.000 --> 00:04.000\nyes\n", ".vtt")
	equal(t, len(doc.Cues), 1)
	equal(t, doc.Cues[0].Lines, []string{"yes"})
	equal(t, doc.Cues[0].Dialect, "vtt")
	// The identifier may carry several lines; settings keep their spacing.
	doc = mustParse(t, "WEBVTT\n\nfirst\nsecond\n00:01.000 --> 00:02.000   align:end\ntext\n", ".vtt")
	equal(t, doc.Cues[0].Rest, "   align:end")
	equal(t, string(mustBytes(t, doc)), "WEBVTT\n\nfirst\nsecond\n00:01.000 --> 00:02.000   align:end\ntext\n")
}

func TestVTTRequiresAHeaderAndAtLeastOneCue(t *testing.T) {
	_, err := ParseSubtitle([]byte("00:01.000 --> 00:03.000\nHello\n"), ".vtt", "")
	contains(t, err.Error(), "begin with WEBVTT")
	_, err = ParseSubtitle([]byte("WEBVTT\n\nNOTE metadata only\n"), ".vtt", "")
	contains(t, err.Error(), "No subtitle cues")
	// The header keyword is matched case-insensitively after stripping.
	mustParse(t, "  webvtt\n\n00:01.000 --> 00:02.000\nA\n", ".vtt")
}

func TestASSPreservesScriptAndNonTextFields(t *testing.T) {
	source := assHeader + "Dialogue: 0,0:00:01.00,0:00:03.00,Default,Jane,0,0,0,,{\\an8}Hello\\Nworld\n"
	doc := mustParse(t, source, ".ass")
	rendered := string(translateDocument(t, doc, NewEcho(), "zh-TW", 40, 2))

	contains(t, rendered, "Title: Demo")
	contains(t, rendered, "Dialogue: 0,0:00:01.00,0:00:03.00,Default,Jane,0,0,0,,")
	contains(t, rendered, "{\\an8}")
	contains(t, rendered, "[zh-TW]")
	equal(t, doc.Cues[0].Start, "0:00:01.00")
	equal(t, doc.Cues[0].End, "0:00:03.00")
	equal(t, doc.Cues[0].Dialect, "ass")
	equal(t, doc.Cues[0].Lines, []string{"{\\an8}Hello", "world"})
}

func TestASSRoundTripKeepsEverythingElse(t *testing.T) {
	source := "\ufeff[Script Info]\r\nTitle: SSA Demo\r\n\r\n[V4 Styles]\r\nFormat: Name, Fontname\r\nStyle: Default,Arial\r\n\r\n" +
		"[Events]\r\n" +
		"Format: Marked, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\r\n" +
		"Comment: 0,0:00:00.00,0:00:01.00,Default,,0,0,0,,not a dialogue\r\n" +
		"Dialogue:   Marked=0,0:00:01.00,0:00:03.00,Default,,0,0,0,,Hello\\Nworld, with commas\r\n" +
		"\r\n[Fonts]\r\nDialogue: ignored,because,wrong,section\r\n"
	doc := mustParse(t, source, ".ssa")
	rendered := mustBytes(t, doc)

	equal(t, doc.Format, "ssa")
	if !bytes.HasPrefix(rendered, []byte("\xef\xbb\xbf")) {
		t.Fatal("BOM lost")
	}
	equal(t, string(rendered), source)
	equal(t, len(doc.Cues), 1)
	equal(t, doc.Cues[0].Lines, []string{"Hello", "world, with commas"})
}

func TestASSTextFieldNeedNotBeLast(t *testing.T) {
	source := "[Events]\nFormat: Start, Text, End\nDialogue: 0:00:01.00,Hello, there,0:00:02.00\n"
	_, err := ParseSubtitle([]byte(source), ".ass", "")
	// Three fields split three ways: Text is "Hello", End is " there,0:00:02.00".
	if err != nil {
		t.Fatal(err)
	}
	doc := mustParse(t, source, ".ass")
	equal(t, doc.Cues[0].Lines, []string{"Hello"})
	equal(t, doc.Cues[0].End, " there,0:00:02.00")
	equal(t, string(mustBytes(t, doc)), source)
}

func TestASSRejectsDialogueWithoutFormatAndMalformedDialogue(t *testing.T) {
	_, err := ParseSubtitle([]byte("[Events]\nDialogue: 0,0:00:01.00,0:00:02.00,Hello\n"), ".ass", "")
	contains(t, err.Error(), "no Format line")
	_, err = ParseSubtitle([]byte("[Events]\nFormat: Start, End, Text\nDialogue: 0:00:01.00,0:00:02.00\n"), ".ass", "")
	contains(t, err.Error(), "Malformed")
	_, err = ParseSubtitle([]byte("[Events]\nFormat: Start, End\nDialogue: 0:00:01.00,0:00:02.00\n"), ".ass", "")
	contains(t, err.Error(), "Malformed")
	_, err = ParseSubtitle([]byte("[Script Info]\nTitle: x\n"), ".ass", "")
	contains(t, err.Error(), "No subtitle cues")
}

func TestASSSoftBreakAndHardSpaceArePreserved(t *testing.T) {
	source := assHeader + assLine("Hello\\nthere\\hworld\\Nsecond line")
	doc := mustParse(t, source, ".ass")
	equal(t, string(mustBytes(t, doc)), source)

	var rec recorder
	rendered := string(translateDocument(t, doc, rec.provider(identity), "es", 40, 2))
	equal(t, rec.calls, [][]string{{"Hello⟦0⟧there⟦1⟧world second line"}})
	contains(t, rendered, "Hello\\nthere\\hworld second line")
}

func TestCloneWithCuesDoesNotMutateTheOriginal(t *testing.T) {
	vtt := mustParse(t, "WEBVTT\n\nid\n00:01.000 --> 00:03.000\nHello\n", ".vtt")
	clone := vtt.CloneWithCues(vtt.Cues)
	clone.vtt.header = "WEBVTT changed"
	clone.vtt.blocks[0].identifier[0] = "changed"
	equal(t, vtt.vtt.header, "WEBVTT")
	equal(t, vtt.vtt.blocks[0].identifier, []string{"id"})

	ass := mustParse(t, assHeader+assLine("Hi"), ".ass")
	aclone := ass.CloneWithCues(ass.Cues)
	aclone.ass.lines[0] = "changed"
	aclone.ass.records[0].values[0] = "changed"
	if ass.ass.lines[0] == "changed" || ass.ass.records[0].values[0] == "changed" {
		t.Fatal("ASS envelope shared with the clone")
	}

	cues := []Cue{{Index: 1, Start: "a", End: "b", Lines: []string{"x"}}}
	slice := (&Document{Format: "srt", Newline: "\n"}).CloneWithCues(cues)
	slice.Cues[0].Lines[0] = "changed"
	// Cue values are copied but the line slices are shared by design: the
	// rebuild step always allocates new ones.
	_ = slice
}

func TestRenderUnsupportedFormat(t *testing.T) {
	if _, err := (&Document{Format: "sub"}).Render(); err == nil {
		t.Fatal("expected error")
	}
}

func TestSRTRendersEmptyCueAsBlankLine(t *testing.T) {
	doc := &Document{Format: "srt", Newline: "\n", Cues: []Cue{{Index: 1, Start: "00:00:01,000", End: "00:00:02,000"}}}
	equal(t, mustRender(t, doc), "1\n00:00:01,000 --> 00:00:02,000\n\n")
}

func TestLargeTimingAndLeadingZeroIndexIsNormalised(t *testing.T) {
	// int("007") == 7 in the Python original; a number too large for an int
	// is treated as no index instead of overflowing.
	doc := mustParse(t, "007\n00:00:01,000 --> 00:00:02,000\nA\n\n99999999999999999999\n00:00:03,000 --> 00:00:04,000\nB\n", ".srt")
	equal(t, []int{doc.Cues[0].Index, doc.Cues[1].Index}, []int{7, 2})
}
