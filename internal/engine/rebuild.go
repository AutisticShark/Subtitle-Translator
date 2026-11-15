package engine

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

var vttEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// ToWrapForm turns sentinels into single zero-width placeholder characters.
//
// It is defensive against output that bypassed validation (direct callers,
// old caches): unknown or repeated placeholders and stray brackets are
// dropped, and missing ones are appended so no tag is lost.
func ToWrapForm(t string, tags []string) string {
	seen := map[int]bool{}
	// Placeholder-range characters in the text are removed first.
	t = strings.Map(func(r rune) rune {
		if isPlaceholder(r) {
			return -1
		}
		return r
	}, t)
	s := sentinelRE.ReplaceAllStringFunc(t, func(match string) string {
		k := parseDigits(sentinelRE.FindStringSubmatch(match)[1])
		if k >= 0 && k < len(tags) && !seen[k] {
			seen[k] = true
			return string(rune(placeholderBase + k))
		}
		return ""
	})
	s = strings.NewReplacer(string(sentinelOpen), "", string(sentinelClose), "").Replace(s)
	s = pyStrip(s)
	var missing strings.Builder
	for k := range tags {
		if !seen[k] {
			missing.WriteRune(rune(placeholderBase + k))
		}
	}
	return s + missing.String()
}

// FromWrapForm restores tags from placeholders. escape re-escapes &, < and >
// in the text (WebVTT), never inside the restored markup.
func FromWrapForm(s string, tags []string, escape bool) string {
	var out strings.Builder
	var text strings.Builder
	flush := func() {
		if escape {
			out.WriteString(vttEscaper.Replace(text.String()))
		} else {
			out.WriteString(text.String())
		}
		text.Reset()
	}
	for _, r := range s {
		if !isPlaceholder(r) {
			text.WriteRune(r)
			continue
		}
		flush()
		if n := int(r) - placeholderBase; n >= 0 && n < len(tags) {
			out.WriteString(tags[n])
		}
	}
	flush()
	return out.String()
}

// withDash restores a speaker dash, after the leading tags if it was there.
func withDash(line string, inside bool) string {
	if inside {
		end := 0
		for end < len(line) {
			r, size := utf8.DecodeRuneInString(line[end:])
			if !isPySpace(r) && !isPlaceholder(r) {
				break
			}
			end += size
		}
		if head := pyStrip(line[:end]); head != "" {
			return head + "- " + line[end:]
		}
	}
	return "- " + line
}

// RebuildCues builds the translated cues from the segments' translations.
// width is the line limit in full-width characters and maxLines the most
// lines a single-speaker cue may use. Cues whose segments carry no text are
// kept exactly as they were.
func RebuildCues(cues []Cue, segs []Segment, out []string, target string, width float64, maxLines int) ([]Cue, error) {
	if len(segs) != len(out) {
		return nil, fmt.Errorf("engine: %d segments but %d translations", len(segs), len(out))
	}
	type item struct {
		seg  Segment
		text string
	}
	byCue := map[int][]item{}
	for i, seg := range segs {
		byCue[seg.CueIndex] = append(byCue[seg.CueIndex], item{seg, out[i]})
	}

	cjk := IsCJKLanguage(target)
	result := make([]Cue, 0, len(cues))
	for ci, cue := range cues {
		items := byCue[ci]
		translatable := false
		for _, it := range items {
			if NeedsTranslation(it.seg) {
				translatable = true
				break
			}
		}
		if !translatable {
			// Nothing was translated (empty cue, markup or drawing only):
			// keep the original lines exactly, without re-wrapping.
			result = append(result, cue.clone())
			continue
		}

		// WebVTT text was decoded for translation; re-escape it on the way out.
		vtt := cue.Dialect == "vtt"
		var lines []string
		if len(items) >= 2 {
			// Speaker pair: one line each, dash restored.
			for _, it := range items {
				line := pyStrip(ToWrapForm(it.text, it.seg.Tags))
				if it.seg.Dashed {
					line = withDash(line, it.seg.DashInside)
				}
				lines = append(lines, pyStrip(FromWrapForm(line, it.seg.Tags, vtt)))
			}
		} else {
			it := items[0]
			// Wrap with each tag as an unbreakable zero-width character, so a
			// line break can never land inside `<font color="...">` or
			// `{\fnArial Black}`; restore the tags afterwards.
			t := pyStrip(ToWrapForm(it.text, it.seg.Tags))
			// A "CJK" target can still emit CJK-free lines (names, numbers,
			// untranslated codes). Wrapping those by character splits words
			// in half, so pick the wrapper from the actual output, not the
			// target.
			var body []string
			if cjk && containsCJK(t) {
				body = WrapCJK(t, width, maxLines)
			} else {
				body = WrapLatin(t, int(width*2), maxLines)
			}
			if it.seg.Dashed && len(body) > 0 {
				body[0] = withDash(body[0], it.seg.DashInside)
			}
			for _, line := range body {
				lines = append(lines, FromWrapForm(line, it.seg.Tags, vtt))
			}
		}
		rebuilt := cue.clone()
		rebuilt.Lines = lines
		result = append(result, rebuilt)
	}
	return result, nil
}
