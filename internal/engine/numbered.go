package engine

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	numberedRE   = regexp.MustCompile(`^(\p{Nd}+)[` + pySpaceClass + `]*[\t.)\x{ff1a}:\x{3001}-][` + pySpaceClass + `]*(.*)$`)
	numberOnlyRE = regexp.MustCompile(`^(\p{Nd}+)$`)
)

func joinContinuation(head, tail string) string {
	if head == "" {
		return tail
	}
	// CJK text has no inter-word spaces; do not invent one at the line break.
	if hasCJK(lastRune(head)) && hasCJK(firstRune(tail)) {
		return head + tail
	}
	return head + " " + tail
}

// ParseNumbered pulls "N<tab>text" pairs back out of a model reply,
// tolerating stray formatting.
//
// A bare "N" (the tab and empty translation trimmed away) is an empty line,
// and an unnumbered line continues the entry before it. Text before the first
// numbered line is preamble and ignored. Numbers outside 1..expected are
// continuation text.
func ParseNumbered(out string, expected int) ([]string, error) {
	got := map[int]string{}
	last := 0
	for _, line := range splitLines(out) {
		line = pyStrip(line)
		if line == "" || strings.HasPrefix(line, "```") {
			continue
		}
		if m := numberedRE.FindStringSubmatch(line); m != nil {
			if n := parseDigits(m[1]); n >= 1 && n <= expected {
				last = n
				got[n] = pyStrip(m[2])
				continue
			}
		}
		if m := numberOnlyRE.FindStringSubmatch(line); m != nil {
			n := parseDigits(m[1])
			if _, seen := got[n]; n >= 1 && n <= expected && !seen {
				last = n
				got[n] = ""
				continue
			}
		}
		if last != 0 {
			got[last] = joinContinuation(got[last], line)
		}
	}
	var missing []string
	for i := 1; i <= expected; i++ {
		if _, ok := got[i]; !ok {
			missing = append(missing, fmt.Sprint(i))
		}
	}
	if len(missing) > 0 {
		shown := missing
		if len(shown) > 10 {
			shown = shown[:10]
		}
		return nil, transientf("model returned %d/%d lines; missing [%s]",
			len(got), expected, strings.Join(shown, ", "))
	}
	result := make([]string, expected)
	for i := range result {
		result[i] = got[i+1]
	}
	return result, nil
}
