package engine

import (
	"strings"
	"testing"
)

func TestDisplayWidth(t *testing.T) {
	equal(t, DisplayWidth("中A"), 1.5)
	equal(t, DisplayWidth(""), 0.0)
	equal(t, DisplayWidth("abcd"), 2.0)
	equal(t, DisplayWidth("한글"), 2.0)
	equal(t, DisplayWidth("ＡＢ"), 2.0)           // full-width Latin
	equal(t, DisplayWidth("ｱｲ"), 1.0)           // half-width katakana
	equal(t, DisplayWidth("a\U000f0001b"), 1.0) // placeholders are zero-width
	equal(t, DisplayWidth("😀"), 1.0)            // emoji are wide
}

func TestWrapCJKKeepsClosingPunctuationOffANewLine(t *testing.T) {
	lines := WrapCJK("你好，世界！再見。", 3, 2)

	if len(lines) > 2 {
		t.Fatalf("%q", lines)
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "，") || strings.HasPrefix(line, "。") || strings.HasPrefix(line, "！") {
			t.Fatalf("closing punctuation led a line: %q", lines)
		}
	}
	equal(t, strings.Join(lines, ""), "你好，世界！再見。")
}

func TestWrapCJKShortAndEmpty(t *testing.T) {
	equal(t, WrapCJK("你好", 5, 2), []string{"你好"})
	equal(t, WrapCJK("  ", 5, 2), []string{""})
	equal(t, WrapCJK("", 5, 2), []string{""})
	equal(t, WrapCJK("  你好  ", 5, 2), []string{"你好"})
}

func TestWrapCJKRebalancesToTwoLines(t *testing.T) {
	// 10 characters at width 4 greedily wraps to 3 lines; max 2 lines asks for
	// a balanced two-line result.
	lines := WrapCJK("一二三四五六七八九十", 4, 2)
	equal(t, lines, []string{"一二三四五", "六七八九十"})
	lines = WrapCJK("一二三四五六七八九十", 4, 3)
	equal(t, strings.Join(lines, ""), "一二三四五六七八九十")
	if len(lines) != 3 {
		t.Fatalf("%q", lines)
	}
	lines = WrapCJK("一二三四五六七八九十", 4, 1)
	equal(t, lines, []string{"一二三四五六七八九十"})
}

func TestWrapCJKOpeningPunctuationMovesWithTheNextCharacter(t *testing.T) {
	lines := WrapCJK("你好世界（再見）", 4, 2)
	for _, line := range lines {
		if strings.HasSuffix(line, "（") {
			t.Fatalf("opening bracket stranded: %q", lines)
		}
	}
	equal(t, strings.Join(lines, ""), "你好世界（再見）")
}

func TestWrapCJKKeepsPlaceholdersAttached(t *testing.T) {
	text := "\U000f0000你好世界你好世界\U000f0001"
	lines := WrapCJK(text, 3, 4)
	equal(t, strings.Join(lines, ""), text)
	for _, line := range lines {
		if strings.TrimFunc(line, isPlaceholder) == "" {
			t.Fatalf("placeholder-only line: %q", lines)
		}
	}
	if !strings.HasPrefix(lines[0], "\U000f0000你") {
		t.Fatalf("%q", lines)
	}
}

func TestWrapLatinPreservesAllWords(t *testing.T) {
	lines := WrapLatin("one two three four", 7, 2)

	equal(t, strings.Join(lines, " "), "one two three four")
	equal(t, lines, []string{"one two", "three four"})
	equal(t, WrapLatin("", 7, 2), []string{""})
	equal(t, WrapLatin("   ", 7, 2), []string{""})
	equal(t, WrapLatin("short", 40, 2), []string{"short"})
}

func TestWrapLatinLimitIsInHalfWidthCells(t *testing.T) {
	// Each Hangul syllable is two cells; Latin letters are one.
	equal(t, WrapLatin("가나다 라마바", 13, 2), []string{"가나다 라마바"})
	equal(t, WrapLatin("가나다 라마바", 12, 2), []string{"가나다", "라마바"})
	equal(t, WrapLatin("abcdef ghijkl", 13, 2), []string{"abcdef ghijkl"})
}

func TestWrapLatinKoreanRespectsTheWidthAndBreaksAtSpaces(t *testing.T) {
	const korean = "안녕하세요 오늘 날씨가 정말 좋네요 그렇죠"
	cue := newCue("", "Hello, the weather is really nice today, isn't it")
	segs := SegmentCue(cue, 0)

	rebuilt, _ := RebuildCues([]Cue{cue}, segs, []string{korean}, "ko", 10, 3)
	lines := rebuilt[0].Lines

	if len(lines) < 2 {
		t.Fatalf("%q", lines)
	}
	for _, line := range lines {
		if DisplayWidth(line) > 10 {
			t.Fatalf("line too wide: %q", lines)
		}
	}
	equal(t, strings.Join(lines, " "), korean)
}

func TestWrapLatinNeverEmitsAnEmptyLine(t *testing.T) {
	text := "xxxxxxxx xxxxxxxx xxxxxxxx xxxxxxxx xx " + strings.Repeat("x", 40)

	for maxLines := 1; maxLines < 7; maxLines++ {
		lines := WrapLatin(text, 10, maxLines)
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				t.Fatalf("maxLines %d: empty line in %q", maxLines, lines)
			}
		}
		if len(lines) > maxLines {
			t.Fatalf("maxLines %d: %d lines", maxLines, len(lines))
		}
		equal(t, strings.Join(lines, " "), text)
	}
}

func TestWrapLatinBalancesWhenGreedyNeedsTooManyLines(t *testing.T) {
	lines := WrapLatin("aa bb cc dd ee ff", 4, 2)
	equal(t, lines, []string{"aa bb cc", "dd ee ff"})
	lines = WrapLatin("aaaa bb c dd", 5, 0) // maxLines is clamped to 1
	equal(t, lines, []string{"aaaa bb c dd"})
}

func TestLatinWordsGluePlaceholderOnlyWords(t *testing.T) {
	const a, b = "\U000f0000", "\U000f0001"
	equal(t, latinWords("aaaa "+a+" bbbb"), []string{"aaaa", a + " bbbb"})
	equal(t, latinWords("aaaa bbbb "+a), []string{"aaaa", "bbbb " + a})
	equal(t, latinWords(a+" "+b), []string{a + " " + b})
	equal(t, latinWords(a+" "+b+" word"), []string{a + " " + b + " word"})
	equal(t, len(latinWords("")), 0)
}

func TestBalanceLinesStopsWhenLongWordsUseUpTheWords(t *testing.T) {
	lines := balanceLines([]string{"xxxxxxxxxxxxxxxx", "y"}, 5)
	equal(t, lines, []string{"xxxxxxxxxxxxxxxx", "y"})
}
