package engine

import (
	"regexp"
	"strconv"
	"strings"
)

// timingRE matches an SRT timing line. It is searched, not anchored, so
// stray text before the timestamps is ignored as in the Python parser.
var timingRE = regexp.MustCompile(
	`(?P<start>\p{Nd}{1,2}:\p{Nd}{2}:\p{Nd}{2}[,.]\p{Nd}{1,3})[` + pySpaceClass + `]*-->[` + pySpaceClass + `]*` +
		`(?P<end>\p{Nd}{1,2}:\p{Nd}{2}:\p{Nd}{2}[,.]\p{Nd}{1,3})(?P<rest>.*)`)

// Cue is one subtitle cue. The shape is shared by all formats.
type Cue struct {
	Index int
	Start string
	End   string
	// Rest is trailing position data on the timing line, e.g. "  X1:100 X2:500".
	Rest  string
	Lines []string
	// Dialect is the markup dialect of the text. "ass" additionally masks the
	// \h hard space and \n soft break escapes, which are plain text in the
	// other formats. "vtt" text escapes &, < and > as character references.
	Dialect string
}

func (c Cue) clone() Cue {
	c.Lines = append([]string(nil), c.Lines...)
	return c
}

type timing struct{ start, end, rest string }

func matchTiming(line string) (timing, bool) {
	m := timingRE.FindStringSubmatch(line)
	if m == nil {
		return timing{}, false
	}
	return timing{
		start: m[timingRE.SubexpIndex("start")],
		end:   m[timingRE.SubexpIndex("end")],
		rest:  m[timingRE.SubexpIndex("rest")],
	}, true
}

func isBlank(line string) bool { return pyStrip(line) == "" }

// ParseSRT is a tolerant SRT parser: it handles a BOM, CRLF, missing or
// duplicate indices, blank lines inside cues and files that do not end with a
// newline. An index too large for an int is treated as no index.
func ParseSRT(raw string) []Cue {
	raw = strings.TrimLeft(raw, byteOrderMark)
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")
	n := len(lines)

	// startsCue is true for a timing line, or an index line directly followed
	// by one.
	startsCue := func(j int) bool {
		if _, ok := matchTiming(lines[j]); ok {
			return true
		}
		if j+1 < n && isASCIIDigits(pyStrip(lines[j])) {
			_, ok := matchTiming(lines[j+1])
			return ok
		}
		return false
	}

	var cues []Cue
	i := 0
	autoIndex := 0
	for i < n {
		for i < n && isBlank(lines[i]) {
			i++
		}
		if i >= n {
			break
		}

		index, hasIndex := 0, false
		if text := pyStrip(lines[i]); isASCIIDigits(text) && i+1 < n {
			if _, ok := matchTiming(lines[i+1]); ok {
				if value, err := strconv.Atoi(text); err == nil {
					index, hasIndex = value, true
				}
				i++
			}
		}

		t, ok := matchTiming(lines[i])
		if !ok {
			// A junk line we cannot make sense of: skip it rather than abort.
			i++
			continue
		}
		i++

		autoIndex++
		body := []string{}
		// Consume until a blank line followed by a new cue header, so blank
		// lines inside a cue do not truncate it. A header directly after text
		// (a missing blank separator) also ends the cue.
		for i < n {
			if isBlank(lines[i]) {
				j := i + 1
				for j < n && isBlank(lines[j]) {
					j++
				}
				if j >= n || startsCue(j) {
					i = j
					break
				}
				body = append(body, "")
				i++
				continue
			}
			if startsCue(i) {
				break
			}
			body = append(body, lines[i])
			i++
		}
		for len(body) > 0 && isBlank(body[len(body)-1]) {
			body = body[:len(body)-1]
		}

		if !hasIndex {
			index = autoIndex
		}
		cues = append(cues, Cue{
			Index: index,
			Start: t.start,
			End:   t.end,
			Rest:  strings.TrimRightFunc(t.rest, isPySpace),
			Lines: body,
		})
	}
	return cues
}
