package engine

import (
	"strings"
	"testing"
)

func TestMaskAndRestoreInlineTags(t *testing.T) {
	masked, tags := MaskTags(`<i>Hello</i> {\an8}world`, false, false)

	equal(t, masked, "⟦0⟧Hello⟦1⟧ ⟦2⟧world")
	equal(t, tags, []string{"<i>", "</i>", `{\an8}`})
	// The model may add spaces inside a sentinel.
	equal(t, UnmaskTags(strings.Replace(masked, "⟦1⟧", "⟦ 1 ⟧", 1), tags), `<i>Hello</i> {\an8}world`)
	// A sentinel without a payload is dropped.
	equal(t, UnmaskTags("a⟦7⟧b", tags), "ab")
}

func TestMaskNoTagsReturnsEmptyNonNilSlice(t *testing.T) {
	masked, tags := MaskTags("plain", false, false)
	equal(t, masked, "plain")
	equal(t, tags, []string{})
}

func TestMaskKaraokeTimestamps(t *testing.T) {
	masked, tags := MaskTags("<c>Never <00:00:01.500>gonna</c>", false, false)

	equal(t, masked, "⟦0⟧Never ⟦1⟧gonna⟦2⟧")
	equal(t, tags[1], "<00:00:01.500>")
}

func TestMaskASSHardSpaceAndSoftBreakOnlyForASS(t *testing.T) {
	masked, tags := MaskTags(`a\hb\nc`, true, false)
	equal(t, masked, "a⟦0⟧b⟦1⟧c")
	equal(t, tags, []string{`\h`, `\n`})

	masked, tags = MaskTags(`C:\new\home`, false, false)
	equal(t, masked, `C:\new\home`)
	equal(t, tags, []string{})
}

func TestMaskDrawings(t *testing.T) {
	const drawing = `{\p1}m 0 0 l 100 0 100 100 0 100{\p0}`
	masked, tags := MaskTags(drawing+"Hello", true, false)
	equal(t, masked, "⟦0⟧Hello")
	equal(t, tags, []string{drawing})

	// Unterminated: the drawing runs to the end.
	masked, tags = MaskTags(`{\p2}m 0 0 l 1 1`, true, false)
	equal(t, masked, "⟦0⟧")
	equal(t, tags, []string{`{\p2}m 0 0 l 1 1`})
	if NeedsTranslation(Segment{Text: masked, Tags: tags}) {
		t.Fatal("a drawing needs no translation")
	}

	// The last \p in a tag decides; tags inside a drawing are folded in.
	masked, _ = MaskTags(`{\p1\p0}text{\p1}m 0 0{\fs20}x{\p0}end`, true, false)
	equal(t, masked, "⟦0⟧text⟦1⟧end")
	// \pos is not a drawing mode.
	masked, _ = MaskTags(`{\pos(1,2)}text`, true, false)
	equal(t, masked, "⟦0⟧text")
}

func TestMaskLiteralSentinelBracketsAndPlaceholderRange(t *testing.T) {
	masked, tags := MaskTags("Press ⟦1⟧ then <i>go</i>", false, false)
	equal(t, strings.Count(masked, "⟦"), 4)
	equal(t, tags, []string{"⟦", "⟧", "<i>", "</i>"})
	equal(t, UnmaskTags(masked, tags), "Press ⟦1⟧ then <i>go</i>")

	masked, tags = MaskTags("a\U000f0003b", false, false)
	equal(t, masked, "a⟦0⟧b")
	equal(t, tags, []string{"\U000f0003"})
}

func TestMaskEntitiesDecodeBetweenTagsOnly(t *testing.T) {
	masked, tags := MaskTags("&lt;i&gt;hi&lt;/i&gt; <i>x</i>", false, true)
	equal(t, masked, "<i>hi</i> ⟦0⟧x⟦1⟧")
	equal(t, tags, []string{"<i>", "</i>"})

	masked, tags = MaskTags("a &#x27e6;0&#x27e7; b", false, true)
	equal(t, masked, "a ⟦0⟧0⟦1⟧ b")
	equal(t, tags, []string{"⟦", "⟧"})

	// Not decoded without the entities flag, and never decoded twice.
	masked, _ = MaskTags("Tom &amp; Jerry", false, false)
	equal(t, masked, "Tom &amp; Jerry")
	masked, _ = MaskTags("&amp;lt;", false, true)
	equal(t, masked, "&lt;")
}

func TestPlaceholdersMatch(t *testing.T) {
	const source = "⟦0⟧Hello⟦1⟧ world"
	for translated, want := range map[string]bool{
		"mundo ⟦0⟧Hola⟦ 1 ⟧":     true,
		"⟦0⟧Hola mundo":          false,
		"⟦0⟧Hola⟦1⟧⟦1⟧ mundo":    false,
		"⟦0⟧Hola⟦1⟧ ⟦2⟧mundo":    false,
		"⟦0⟧Hola⟦1⟧ ⟦mundo":      false,
		"⟦0⟧Hola⟦1⟧ mundo⟧":      false,
		"⟦１⟧Hola⟦0⟧ mundo":       true, // full-width digit still names placeholder 1
		"⟦1⟧Hola⟦0⟧ reordered":   true,
		"⟦0⟧Hola⟦1⟧ mundo extra": true,
	} {
		if got := PlaceholdersMatch(source, translated); got != want {
			t.Errorf("PlaceholdersMatch(%q) = %v, want %v", translated, got, want)
		}
	}
	if !PlaceholdersMatch("plain", "simple") || PlaceholdersMatch("plain", "⟦0⟧simple") {
		t.Error("plain sources must match only placeholder-free output")
	}
}

func TestNeedsTranslation(t *testing.T) {
	for text, want := range map[string]bool{
		"":             false,
		"   ":          false,
		"⟦0⟧":          false,
		"⟦0⟧ ⟦1⟧":      false,
		"⟦0⟧\u00a0":    false, // NBSP is whitespace to Python's strip
		"⟦0⟧a":         true,
		"-":            true,
		"[music]":      true,
		"\u3000x":      true,
		"\u001c\u001d": false,
	} {
		if got := NeedsTranslation(Segment{Text: text}); got != want {
			t.Errorf("NeedsTranslation(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestSpeakerDialogueIsSplitAndDashesAreRestored(t *testing.T) {
	cue := newCue("", "- <i>Hello</i>", "— Goodbye")
	segs := SegmentCue(cue, 0)

	equal(t, []string{segs[0].Text, segs[1].Text}, []string{"⟦0⟧Hello⟦1⟧", "Goodbye"})
	rebuilt, err := RebuildCues([]Cue{cue}, segs, []string{"⟦0⟧Hola⟦1⟧", "Adiós"}, "es", 20, 2)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, rebuilt[0].Lines, []string{"- <i>Hola</i>", "- Adiós"})
}

func TestSingleDashedWrappedCueRemainsOneSegment(t *testing.T) {
	segs := SegmentCue(newCue("", "- A long", "sentence"), 0)

	equal(t, len(segs), 1)
	equal(t, segs[0].Dashed, true)
	equal(t, segs[0].Text, "A long sentence")
}

func TestUndashedLineAnsweredByADashedLineIsAPair(t *testing.T) {
	cue := newCue("", "Where are you going?", "-To the store.")
	segs := SegmentCue(cue, 0)

	equal(t, []string{segs[0].Text, segs[1].Text}, []string{"Where are you going?", "To the store."})
	equal(t, []bool{segs[0].Dashed, segs[1].Dashed}, []bool{false, true})
	rebuilt, _ := RebuildCues([]Cue{cue}, segs, []string{"¿Adónde vas?", "A la tienda."}, "es", 40, 2)
	equal(t, rebuilt[0].Lines, []string{"¿Adónde vas?", "- A la tienda."})
}

func TestOtherSingleDashShapesStayOneSegment(t *testing.T) {
	for _, lines := range [][]string{
		{"-Hello there"}, {"- A long", "sentence"},
		{"I was going", "to say", "- really?"}, {"Where are", "you going?"},
	} {
		if got := len(SegmentCue(newCue("", lines...), 0)); got != 1 {
			t.Errorf("%q: %d segments", lines, got)
		}
	}
}

func TestDashInsideLeadingTagsIsDetectedAndRestored(t *testing.T) {
	cue := newCue("", "<i>- Where?</i>", "<i>- Home.</i>")
	segs := SegmentCue(cue, 0)

	equal(t, []string{segs[0].Text, segs[1].Text}, []string{"⟦0⟧Where?⟦1⟧", "⟦0⟧Home.⟦1⟧"})
	equal(t, []bool{segs[0].DashInside, segs[1].DashInside}, []bool{true, true})
	rebuilt, _ := RebuildCues([]Cue{cue}, segs, []string{"⟦0⟧¿Dónde?⟦1⟧", "⟦0⟧A casa.⟦1⟧"}, "es", 20, 2)
	equal(t, rebuilt[0].Lines, []string{"<i>- ¿Dónde?</i>", "<i>- A casa.</i>"})
}

func TestDashAfterASSOverrideIsDetected(t *testing.T) {
	cue := newCue("ass", `{\an8}- Hi`, "- Bye")
	segs := SegmentCue(cue, 0)

	equal(t, len(segs), 2)
	rebuilt, _ := RebuildCues([]Cue{cue}, segs, []string{"⟦0⟧Hola", "Adiós"}, "es", 20, 2)
	equal(t, rebuilt[0].Lines, []string{`{\an8}- Hola`, "- Adiós"})
}

func TestUndashedContinuationJoinsThePreviousSpeaker(t *testing.T) {
	cue := newCue("", "- Where are you going", "right now?", "- Home.")
	segs := SegmentCue(cue, 0)

	equal(t, []string{segs[0].Text, segs[1].Text}, []string{"Where are you going right now?", "Home."})
	equal(t, []bool{segs[0].Dashed, segs[1].Dashed}, []bool{true, true})
	rebuilt, _ := RebuildCues([]Cue{cue}, segs, []string{"¿Adónde vas ahora?", "A casa."}, "es", 20, 2)
	equal(t, rebuilt[0].Lines, []string{"- ¿Adónde vas ahora?", "- A casa."})
}

func TestMinusSignBeforeADigitIsNotADash(t *testing.T) {
	segs := SegmentCue(newCue("", "-10 degrees outside"), 0)

	equal(t, segs[0].Dashed, false)
	equal(t, segs[0].Text, "-10 degrees outside")
	rebuilt, _ := RebuildCues([]Cue{newCue("", "-10 degrees outside")}, segs, []string{segs[0].Text}, "es", 40, 2)
	equal(t, rebuilt[0].Lines, []string{"-10 degrees outside"})

	speakers := SegmentCue(newCue("", "-10 degrees?", "- Yes."), 0)
	equal(t, []string{speakers[0].Text, speakers[1].Text}, []string{"-10 degrees?", "Yes."})
	equal(t, []bool{speakers[0].Dashed, speakers[1].Dashed}, []bool{false, true})
}

func TestDashVariants(t *testing.T) {
	for line, want := range map[string]struct {
		dashed, inside bool
		body           string
	}{
		"- Hi":           {true, false, "Hi"},
		"-Hi":            {true, false, "Hi"},
		"– Hi":           {true, false, "Hi"},
		"—Hi":            {true, false, "Hi"},
		"-":              {false, false, "-"},
		"- ":             {false, false, "- "},
		"-5":             {false, false, "-5"},
		"– 5 things":     {true, false, "5 things"},
		"<i>-Hi</i>":     {true, true, "<i>Hi</i>"},
		"<i> <b>- Hi":    {true, true, "<i> <b>Hi"},
		"Hi - there":     {false, false, "Hi - there"},
		"<i>Hi</i> - no": {false, false, "<i>Hi</i> - no"},
		"  - spaced":     {true, false, "spaced"},
	} {
		dashed, inside, body := splitDash(line, false)
		if dashed != want.dashed || inside != want.inside || body != want.body {
			t.Errorf("splitDash(%q) = %v, %v, %q; want %v, %v, %q", line, dashed, inside, body,
				want.dashed, want.inside, want.body)
		}
	}
}

func TestSegmentCueVTTDecodesEntitiesAndSRTDoesNot(t *testing.T) {
	segs := SegmentCue(newCue("vtt", "Tom &amp; Jerry &lt;3"), 0)
	equal(t, segs[0].Text, "Tom & Jerry <3")

	segs = SegmentCue(newCue("", "Tom &amp; Jerry"), 0)
	equal(t, segs[0].Text, "Tom &amp; Jerry")
}

func TestSegmentCueEmptyCueYieldsOneEmptySegment(t *testing.T) {
	segs := SegmentCue(newCue("", "", "  "), 0)
	equal(t, len(segs), 1)
	equal(t, segs[0].Text, "")
	if NeedsTranslation(segs[0]) {
		t.Fatal("empty segment must not be translated")
	}
}

func TestSegmentCueIsolatesDashDetectionFromEntityDecoding(t *testing.T) {
	// `&#45;` decodes to a hyphen only after dash detection, so it is text.
	segs := SegmentCue(newCue("vtt", "&#45; not a dash"), 0)
	equal(t, segs[0].Dashed, false)
	equal(t, segs[0].Text, "- not a dash")
}

func TestToWrapFormIsDefensive(t *testing.T) {
	tags := []string{"<i>", "</i>"}
	for in, want := range map[string]string{
		"⟦0⟧Hola⟦1⟧":       "\U000f0000Hola\U000f0001",
		"Hola⟦0⟧⟦0⟧ ⟦7⟧ ⟦": "Hola\U000f0000\U000f0001",
		"x\U000f0005y":     "xy\U000f0000\U000f0001",
		"  ⟦1⟧Hola⟦ 0 ⟧  ": "\U000f0001Hola\U000f0000",
		"just text":        "just text\U000f0000\U000f0001",
	} {
		if got := ToWrapForm(in, tags); got != want {
			t.Errorf("ToWrapForm(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromWrapFormRestoresTagsAndEscapesOnlyText(t *testing.T) {
	tags := []string{"<c.loud>", "</c>"}
	in := "\U000f0000Fish & chips > all\U000f0001 \U000f0009"
	equal(t, FromWrapForm(in, tags, false), "<c.loud>Fish & chips > all</c> ")
	equal(t, FromWrapForm(in, tags, true), "<c.loud>Fish &amp; chips &gt; all</c> ")
	equal(t, FromWrapForm(`a"b'c`, tags, true), `a"b'c`)
}

func TestRebuildToleratesUnvalidatedOutput(t *testing.T) {
	cue := newCue("", "<i>Hello</i>")
	segs := SegmentCue(cue, 0)

	rebuilt, err := RebuildCues([]Cue{cue}, segs, []string{"⟦0⟧Hola⟦0⟧ ⟦7⟧ ⟦"}, "es", 40, 2)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, rebuilt[0].Lines, []string{"<i>Hola</i>"})
}

func TestRebuildRejectsMismatchedLengths(t *testing.T) {
	if _, err := RebuildCues([]Cue{newCue("", "a")}, plainSegments("a"), nil, "es", 40, 2); err == nil {
		t.Fatal("expected an error")
	}
}

func TestRebuildKeepsUntranslatedCuesVerbatim(t *testing.T) {
	cue := newCue("ass", `{\an8}`)
	segs := SegmentCue(cue, 0)
	rebuilt, _ := RebuildCues([]Cue{cue}, segs, []string{segs[0].Text}, "es", 40, 2)
	equal(t, rebuilt[0].Lines, []string{`{\an8}`})

	empty := Cue{Index: 3, Start: "a", End: "b"}
	rebuilt, _ = RebuildCues([]Cue{empty}, nil, nil, "es", 40, 2)
	equal(t, rebuilt[0].Index, 3)
	equal(t, len(rebuilt[0].Lines), 0)
}

func TestRebuildKeepsUntranslatedLatinWordsIntactForACJKTarget(t *testing.T) {
	cue := newCue("", "placeholder")
	segs := []Segment{{CueIndex: 0, Text: "placeholder"}}

	rebuilt, _ := RebuildCues([]Cue{cue}, segs, []string{"Sherlock Holmes"}, "zh-TW", 4, 2)

	equal(t, rebuilt[0].Lines, []string{"Sherlock", "Holmes"})
}

func TestRebuildWrapsCJKTargetByCharacter(t *testing.T) {
	cue := newCue("", "placeholder")
	segs := []Segment{{CueIndex: 0, Text: "placeholder"}}

	rebuilt, _ := RebuildCues([]Cue{cue}, segs, []string{"你好，世界！再見。"}, "zh-TW", 3, 2)

	if len(rebuilt[0].Lines) != 2 || strings.Join(rebuilt[0].Lines, "") != "你好，世界！再見。" {
		t.Fatalf("%q", rebuilt[0].Lines)
	}
	for _, line := range rebuilt[0].Lines {
		if strings.ContainsAny(string([]rune(line)[:1]), "，。！") {
			t.Fatalf("punctuation led a line: %q", rebuilt[0].Lines)
		}
	}
}

func TestVTTTranslatedTextIsEscapedButTagsAreNot(t *testing.T) {
	const source = "WEBVTT\n\n" +
		"00:01.000 --> 00:02.000\nTom &amp; Jerry &lt;3\n\n" +
		"00:03.000 --> 00:04.000\n<c.loud>Fish &amp; chips &gt; all</c>\n\n" +
		"00:05.000 --> 00:06.000\n&nbsp;\n"
	doc := mustParse(t, source, ".vtt")

	var rec recorder
	got := translateDocument(t, doc, rec.provider(identity), "es", 40, 2)
	equal(t, rec.calls, [][]string{{"Tom & Jerry <3", "⟦0⟧Fish & chips > all⟦1⟧"}})
	equal(t, string(got), source)

	rendered := string(translateDocument(t, doc, providerFunc(func(texts []string) ([]string, error) {
		out := make([]string, len(texts))
		for i, text := range texts {
			out[i] = text + " <&>"
		}
		return out, nil
	}), "es", 40, 2))
	contains(t, rendered, "\nTom &amp; Jerry &lt;3 &lt;&amp;&gt;\n")
	contains(t, rendered, "\n<c.loud>Fish &amp; chips &gt; all</c> &lt;&amp;&gt;\n")
	// The markup-only cue is not translated and keeps its original bytes.
	contains(t, rendered, "\n00:05.000 --> 00:06.000\n&nbsp;\n")

	failing := providerFunc(func([]string) ([]string, error) { return nil, transientf("down") })
	equal(t, string(translateDocument(t, doc, failing, "es", 40, 2)), source)
}

func TestTagSafeWrapping(t *testing.T) {
	rebuild := func(line, language string, width float64, translated, dialect string) []string {
		cue := newCue(dialect, line)
		segs := SegmentCue(cue, 0)
		out := segs[0].Text
		if translated != "" {
			out = translated
		}
		rebuilt, err := RebuildCues([]Cue{cue}, segs, []string{out}, language, width, 4)
		if err != nil {
			t.Fatal(err)
		}
		return rebuilt[0].Lines
	}
	balanced := func(lines []string, tags ...string) {
		t.Helper()
		for _, line := range lines {
			if strings.Count(line, "<") != strings.Count(line, ">") || strings.Count(line, "{") != strings.Count(line, "}") {
				t.Fatalf("split a tag: %q", lines)
			}
		}
		joined := strings.Join(lines, "")
		for _, tag := range tags {
			contains(t, joined, tag)
		}
	}

	t.Run("font tag", func(t *testing.T) {
		lines := rebuild(`<font color="#ff0000">Hello there my friend</font>`, "es", 5, "", "")
		if len(lines) < 2 {
			t.Fatal(lines)
		}
		balanced(lines, `<font color="#ff0000">`, "</font>")
		if !strings.HasPrefix(lines[0], `<font color="#ff0000">Hello`) {
			t.Fatal(lines)
		}
	})
	t.Run("ass override with spaces", func(t *testing.T) {
		lines := rebuild(`{\fnArial Black}Hello there my friend`, "es", 5, "", "ass")
		balanced(lines, `{\fnArial Black}`)
		if !strings.HasPrefix(lines[0], `{\fnArial Black}Hello`) {
			t.Fatal(lines)
		}
	})
	t.Run("cjk never splits an override", func(t *testing.T) {
		lines := rebuild(`{\pos(100,200)}Hello`, "zh-TW", 3, "⟦0⟧你好世界你好世界", "ass")
		if len(lines) < 2 {
			t.Fatal(lines)
		}
		balanced(lines, `{\pos(100,200)}`)
		if !strings.HasPrefix(lines[0], `{\pos(100,200)}你`) {
			t.Fatal(lines)
		}
	})
	t.Run("tag between spaces never forms its own line", func(t *testing.T) {
		lines := rebuild("aaaa <i> bbbb cccc</i>", "es", 3, "", "")
		for _, line := range lines {
			if line == "<i>" || line == "</i>" {
				t.Fatalf("markup-only line: %q", lines)
			}
		}
		equal(t, strings.Join(lines, " "), "aaaa <i> bbbb cccc</i>")
	})
}

func TestASSDrawingIsNeverSentOrWrapped(t *testing.T) {
	const drawing = `{\p1}m 0 0 l 100 0 100 100 0 100{\p0}`
	source := assHeader + assLine(drawing) + assLine(drawing+"Hello there my good friend")
	doc := mustParse(t, source, ".ass")

	var rec recorder
	rendered := string(translateDocument(t, doc, rec.provider(func(s string) string { return "T:" + s }), "es", 5, 4))

	sent := rec.all()
	equal(t, len(sent), 1)
	notContains(t, sent[0], "m 0 0")
	contains(t, rendered, strings.TrimSuffix(assLine(drawing), "\n"))
	// The text after the drawing is wrapped; the drawing itself is not.
	contains(t, rendered, "T:"+drawing+`Hello\N`)
}

func TestEmptySegmentsAreNotSentAndPassThrough(t *testing.T) {
	source := assHeader + assLine("") + assLine("Hello") + assLine(`{\an8}`)
	doc := mustParse(t, source, ".ass")
	segs := SegmentDocument(doc.Cues)
	var rec recorder
	var reported []int
	opt := testOptions()
	opt.Fallback = func(n int) { reported = append(reported, n) }

	out, err := runTranslation(t, segs, rec.provider(func(s string) string { return "T:" + s }), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	cues, _ := RebuildCues(doc.Cues, segs, out, "es", 40, 2)
	rendered := mustRender(t, doc.CloneWithCues(cues))

	equal(t, rec.calls, [][]string{{"Hello"}})
	equal(t, len(reported), 0)
	contains(t, rendered, strings.TrimSuffix(assLine(""), "\n"))
	contains(t, rendered, strings.TrimSuffix(assLine("T:Hello"), "\n"))
	contains(t, rendered, strings.TrimSuffix(assLine(`{\an8}`), "\n"))
}

func TestSRTCueWithoutTextIsNotSent(t *testing.T) {
	doc := mustParse(t, "1\n00:00:01,000 --> 00:00:02,000\n\n2\n00:00:03,000 --> 00:00:04,000\nHi\n", ".srt")
	var rec recorder

	rendered := string(translateDocument(t, doc, rec.provider(func(s string) string { return "T:" + s }), "es", 40, 2))

	equal(t, rec.calls, [][]string{{"Hi"}})
	equal(t, rendered, strings.Replace(mustRender(t, doc), "Hi", "T:Hi", 1))
}

func TestLiteralSentinelsInSourceRoundTrip(t *testing.T) {
	const line = "Press ⟦1⟧ then <i>go</i>"
	cue := newCue("", line)
	segs := SegmentCue(cue, 0)
	out, err := runTranslation(t, segs, NewEcho(), testOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, _ := RebuildCues([]Cue{cue}, segs, out, "es", 40, 2)

	equal(t, rebuilt[0].Lines, []string{"[es] " + line})
}
