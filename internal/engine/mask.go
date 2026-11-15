package engine

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	sentinelOpen  = '⟦'
	sentinelClose = '⟧'

	// Wrapping works on text whose placeholders are single characters from
	// the Supplementary Private Use Area-A: zero-width and impossible to
	// split.
	placeholderBase = 0xF0000
	placeholderMax  = 0xFFFFD
)

// tagAlt: <i> <b> <u> <font ...> </...>, WebVTT karaoke timestamps
// <00:00:01.000>, ASS overrides {\an8}, and {y:i} legacy tags.
const tagAlt = `</?[a-zA-Z][^>]*>|<(?:\p{Nd}+:)?\p{Nd}{1,2}:\p{Nd}{2}[.,]\p{Nd}{1,3}>|\{[^}]*\}`

// reservedClass holds the characters that must never reach a provider as
// text: the sentinel brackets themselves and the internal placeholder range.
// They are masked as tags so they round-trip byte-for-byte instead of being
// mistaken for placeholders.
const reservedClass = `[\x{27e6}\x{27e7}\x{f0000}-\x{ffffd}]`

var (
	maskRE    = regexp.MustCompile(tagAlt + `|` + reservedClass)
	maskASSRE = regexp.MustCompile(tagAlt + `|\\[hn]|` + reservedClass)
	// sentinelRE tolerates spaces inside the sentinel, which models add.
	sentinelRE = regexp.MustCompile(`\x{27e6}[` + pySpaceClass + `]*(\p{Nd}+)[` + pySpaceClass + `]*\x{27e7}`)
	drawingRE  = regexp.MustCompile(`\\p(\p{Nd}+)`)
)

func isPlaceholder(r rune) bool { return r >= placeholderBase && r <= placeholderMax }

func isReserved(r rune) bool {
	return r == sentinelOpen || r == sentinelClose || isPlaceholder(r)
}

// Segment is one translatable unit. A cue is one segment, unless it holds a
// two-speaker dialogue pair, in which case each speaker is its own.
type Segment struct {
	CueIndex int
	// Text is the masked text, tags replaced by sentinels.
	Text string
	// Tags are the sentinel payloads, in order.
	Tags   []string
	Dashed bool
	// DashInside is set when the dash followed leading tags (`<i>- Hi`)
	// rather than preceding them.
	DashInside bool
}

type piece struct {
	tag  bool
	text string
}

// MaskTags replaces markup with numbered sentinels "⟦N⟧" and returns the
// masked text and the payloads.
//
// ASS vector drawings (the text after a \p1 override, up to and including the
// \p0 override that ends it) are folded into one placeholder: drawing
// commands are neither translatable nor wrappable.
//
// entities decodes character references (WebVTT &amp;) in the text between
// tags, so the provider sees & rather than markup. Decoding happens after
// tags are found, so &lt;i&gt; stays literal text.
func MaskTags(s string, ass, entities bool) (string, []string) {
	re := maskRE
	if ass {
		re = maskASSRE
	}
	var pieces []piece
	drawing := false
	pos := 0
	matches := re.FindAllStringIndex(s, -1)
	for n := 0; n <= len(matches); n++ {
		end := len(s)
		if n < len(matches) {
			end = matches[n][0]
		}
		if end > pos {
			if drawing {
				pieces[len(pieces)-1].text += s[pos:end]
			} else {
				pieces = append(pieces, piece{false, s[pos:end]})
			}
		}
		if n == len(matches) {
			break
		}
		tag := s[matches[n][0]:matches[n][1]]
		if drawing {
			pieces[len(pieces)-1].text += tag
		} else {
			pieces = append(pieces, piece{true, tag})
		}
		if strings.HasPrefix(tag, "{") {
			if modes := drawingRE.FindAllStringSubmatch(tag, -1); len(modes) > 0 {
				drawing = parseDigits(modes[len(modes)-1][1]) > 0
			}
		}
		pos = matches[n][1]
	}

	if entities {
		// A reference may decode to a reserved character (&#x27e6;); mask it
		// like a literal one.
		var decoded []piece
		for _, p := range pieces {
			if p.tag {
				decoded = append(decoded, p)
				continue
			}
			var run strings.Builder
			for _, r := range html.UnescapeString(p.text) {
				if isReserved(r) {
					if run.Len() > 0 {
						decoded = append(decoded, piece{false, run.String()})
						run.Reset()
					}
					decoded = append(decoded, piece{true, string(r)})
				} else {
					run.WriteRune(r)
				}
			}
			if run.Len() > 0 {
				decoded = append(decoded, piece{false, run.String()})
			}
		}
		pieces = decoded
	}

	tags := []string{}
	var out strings.Builder
	for _, p := range pieces {
		if p.tag {
			fmt.Fprintf(&out, "⟦%d⟧", len(tags))
			tags = append(tags, p.text)
		} else {
			out.WriteString(p.text)
		}
	}
	return out.String(), tags
}

// UnmaskTags restores sentinels to their payloads. A sentinel without a
// payload is dropped.
func UnmaskTags(s string, tags []string) string {
	return sentinelRE.ReplaceAllStringFunc(s, func(match string) string {
		sub := sentinelRE.FindStringSubmatch(match)
		k := parseDigits(sub[1])
		if k >= 0 && k < len(tags) {
			return tags[k]
		}
		return ""
	})
}

func sentinelCounts(s string) map[int]int {
	counts := map[int]int{}
	for _, sub := range sentinelRE.FindAllStringSubmatch(s, -1) {
		counts[parseDigits(sub[1])]++
	}
	return counts
}

// PlaceholdersMatch is true when translated holds exactly the source's
// placeholders.
//
// Order may change (translation reorders words), but every placeholder must
// appear as often as in the source and no stray sentinel bracket may remain;
// otherwise restoring the tags would drop, duplicate, or invent markup.
func PlaceholdersMatch(source, translated string) bool {
	expected, got := sentinelCounts(source), sentinelCounts(translated)
	if len(expected) != len(got) {
		return false
	}
	for k, n := range expected {
		if got[k] != n {
			return false
		}
	}
	rest := sentinelRE.ReplaceAllString(translated, "")
	return !strings.ContainsRune(rest, sentinelOpen) && !strings.ContainsRune(rest, sentinelClose)
}

func hasText(s string) bool {
	return pyStrip(sentinelRE.ReplaceAllString(s, "")) != ""
}

// NeedsTranslation is false for segments with no text besides markup; they
// are passed through unchanged.
func NeedsTranslation(seg Segment) bool { return hasText(seg.Text) }

func skipSpace(s string, pos int) int {
	for pos < len(s) {
		r, size := utf8.DecodeRuneInString(s[pos:])
		if !isPySpace(r) {
			break
		}
		pos += size
	}
	return pos
}

// matchSentinel matches a strict "⟦<digits>⟧" at pos (no inner spaces) and
// returns the position after it.
func matchSentinel(s string, pos int) (int, bool) {
	r, size := utf8.DecodeRuneInString(s[pos:])
	if pos >= len(s) || r != sentinelOpen {
		return pos, false
	}
	p := pos + size
	start := p
	for p < len(s) {
		r, size = utf8.DecodeRuneInString(s[p:])
		if !unicode.IsDigit(r) {
			break
		}
		p += size
	}
	if p == start || p >= len(s) {
		return pos, false
	}
	if r, size = utf8.DecodeRuneInString(s[p:]); r != sentinelClose {
		return pos, false
	}
	return p + size, true
}

// matchDash implements the speaker-dash pattern
//
//	^(?P<lead>\s*(?:⟦\d+⟧\s*)*)(?:-(?!\d)|[–—])\s*(?=\S)
//
// by hand (RE2 has no lookahead). It returns the end of the lead group and
// the end of the whole match. A hyphen-minus directly followed by a digit is
// a minus sign (`-10 degrees`), not a dash.
func matchDash(masked string) (leadEnd, end int, ok bool) {
	pos := skipSpace(masked, 0)
	for {
		next, matched := matchSentinel(masked, pos)
		if !matched {
			break
		}
		pos = skipSpace(masked, next)
	}
	leadEnd = pos
	if pos >= len(masked) {
		return 0, 0, false
	}
	r, size := utf8.DecodeRuneInString(masked[pos:])
	switch r {
	case '-':
		if next, _ := utf8.DecodeRuneInString(masked[pos+size:]); pos+size < len(masked) && unicode.IsDigit(next) {
			return 0, 0, false
		}
	case '–', '—':
	default:
		return 0, 0, false
	}
	end = skipSpace(masked, pos+size)
	if end >= len(masked) {
		return 0, 0, false
	}
	return leadEnd, end, true
}

// splitDash detects a speaker dash on the masked line, so leading tags cannot
// hide it. It returns (dashed, dash after tags, raw line without the dash).
func splitDash(line string, ass bool) (dashed, inside bool, body string) {
	masked, tags := MaskTags(line, ass, false)
	leadEnd, end, ok := matchDash(masked)
	if !ok {
		return false, false, line
	}
	lead := pyStrip(masked[:leadEnd])
	return true, lead != "", UnmaskTags(lead+masked[end:], tags)
}

// SegmentCue splits a cue into translatable segments.
func SegmentCue(cue Cue, cueIndex int) []Segment {
	ass := cue.Dialect == "ass"
	vtt := cue.Dialect == "vtt"
	var stripped []string
	for _, line := range cue.Lines {
		if s := pyStrip(line); s != "" {
			stripped = append(stripped, s)
		}
	}
	// Dash detection works on the raw lines; entities are decoded only by
	// the final masking below, so `&amp;lt;` is never decoded twice.
	type parsedLine struct {
		dashed, inside bool
		body           string
	}
	parsed := make([]parsedLine, len(stripped))
	dashedCount := 0
	for i, line := range stripped {
		d, in, body := splitDash(line, ass)
		parsed[i] = parsedLine{d, in, body}
		if d {
			dashedCount++
		}
	}

	// Two or more dashed lines => speaker pair; keep the speakers distinct.
	// An undashed line continues the preceding speaker's sentence. So does an
	// undashed line answered by a dashed one (`Where to?` / `-The store.`):
	// the first speaker's dash is often omitted.
	singleDashPair := len(parsed) == 2 && !parsed[0].dashed && parsed[1].dashed
	if singleDashPair || dashedCount >= 2 {
		type group struct {
			dashed, inside bool
			bodies         []string
		}
		var groups []group
		for _, p := range parsed {
			if p.dashed || len(groups) == 0 {
				groups = append(groups, group{p.dashed, p.inside, []string{pyStrip(p.body)}})
			} else {
				last := &groups[len(groups)-1]
				last.bodies = append(last.bodies, pyStrip(p.body))
			}
		}
		segs := make([]Segment, 0, len(groups))
		for _, g := range groups {
			text, tags := MaskTags(joinSpace(g.bodies), ass, vtt)
			segs = append(segs, Segment{cueIndex, text, tags, g.dashed, g.inside})
		}
		return segs
	}

	// Otherwise the cue is one sentence fragment possibly wrapped over lines.
	dashed, inside, joined := splitDash(joinSpace(stripped), ass)
	text, tags := MaskTags(pyStrip(joined), ass, vtt)
	return []Segment{{cueIndex, text, tags, dashed, inside}}
}

// SegmentDocument segments every cue of a document.
func SegmentDocument(cues []Cue) []Segment {
	var segs []Segment
	for i, cue := range cues {
		segs = append(segs, SegmentCue(cue, i)...)
	}
	return segs
}
