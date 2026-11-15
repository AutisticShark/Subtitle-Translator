package engine

import (
	"strings"

	"golang.org/x/text/width"
)

const (
	cjkPunctNoLead  = "，。、；：？！）」』】》〉,.!?;:)]}"
	cjkPunctNoTrail = "（「『【《〈([{"
)

// cells is the width in half-width cells: full-width and wide characters
// (CJK, Hangul) take two, tag placeholders none, everything else one.
func cells(s string) int {
	total := 0
	for _, r := range s {
		total += runeCells(r)
	}
	return total
}

func runeCells(r rune) int {
	if isPlaceholder(r) {
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// DisplayWidth counts full-width characters as 1 and half-width ones as 0.5,
// so a limit is expressed in "full-width equivalents". Internal tag
// placeholders are zero-width.
func DisplayWidth(s string) float64 { return float64(cells(s)) / 2 }

func isNoLead(r rune) bool  { return strings.ContainsRune(cjkPunctNoLead, r) }
func isNoTrail(r rune) bool { return strings.ContainsRune(cjkPunctNoTrail, r) }

func runesWidth(rs []rune) float64 {
	total := 0
	for _, r := range rs {
		total += runeCells(r)
	}
	return float64(total) / 2
}

// WrapCJK is a greedy wrap that does not orphan closing punctuation onto a new
// line. Tag placeholders stay attached to the text before them. s may hold
// placeholder characters (see ToWrapForm); limit is in full-width characters.
func WrapCJK(s string, limit float64, maxLines int) []string {
	s = pyStrip(s)
	if s == "" || DisplayWidth(s) <= limit {
		return []string{s}
	}
	if maxLines < 1 {
		maxLines = 1
	}
	text := []rune(s)

	var lines [][]rune
	var cur []rune
	for _, ch := range text {
		if len(cur) > 0 && runesWidth(append(cur[:len(cur):len(cur)], ch)) > limit &&
			!isNoLead(ch) && !isPlaceholder(ch) {
			var carry []rune
			if isNoTrail(cur[len(cur)-1]) {
				cur, carry = cur[:len(cur)-1], []rune{cur[len(cur)-1]}
			}
			lines = append(lines, cur)
			cur = append(append([]rune(nil), carry...), ch)
		} else {
			cur = append(cur, ch)
		}
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}

	// Balanced two-line subtitles read better than a full line plus a stub,
	// so rebalance whenever we wrapped at all (and always if we blew past
	// maxLines).
	nLines := maxLines
	if len(lines) <= maxLines {
		nLines = max(len(lines), 2)
		if maxLines < nLines {
			nLines = maxLines
		}
	}
	target := DisplayWidth(s) / float64(nLines)
	lines, cur = nil, nil
	for _, ch := range text {
		if len(cur) > 0 && runesWidth(cur) >= target && len(lines) < nLines-1 &&
			!isNoLead(ch) && !isPlaceholder(ch) {
			var carry []rune
			if isNoTrail(cur[len(cur)-1]) {
				cur, carry = cur[:len(cur)-1], []rune{cur[len(cur)-1]}
			}
			lines = append(lines, cur)
			cur = append(append([]rune(nil), carry...), ch)
		} else {
			cur = append(cur, ch)
		}
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}

	var out []string
	for _, line := range lines {
		if stripped := pyStrip(string(line)); stripped != "" {
			out = append(out, stripped)
		}
	}
	return out
}

// latinWords splits on whitespace, gluing placeholder-only words (a tag
// between two spaces) to the next word, or to the previous one at the end, so
// a line never consists of markup alone.
func latinWords(s string) []string {
	var words []string
	pending := ""
	havePending := false
	for _, w := range pyFields(s) {
		if cells(w) == 0 {
			if havePending {
				pending += " " + w
			} else {
				pending, havePending = w, true
			}
			continue
		}
		if havePending {
			words = append(words, pending+" "+w)
		} else {
			words = append(words, w)
		}
		pending, havePending = "", false
	}
	if havePending {
		if len(words) > 0 {
			words[len(words)-1] += " " + pending
		} else {
			words = append(words, pending)
		}
	}
	return words
}

// WrapLatin wraps at spaces. limit is in half-width cells, so space-separated
// scripts with wide characters (Korean Hangul) get half as many per line.
func WrapLatin(s string, limit, maxLines int) []string {
	words := latinWords(s)
	var lines []string
	cur := ""
	for _, w := range words {
		if cur != "" && cells(cur)+1+cells(w) > limit {
			lines = append(lines, cur)
			cur = w
		} else if cur != "" {
			cur += " " + w
		} else {
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) == 0 {
		return []string{""}
	}

	maxLines = max(1, maxLines)
	if len(lines) <= maxLines {
		return lines
	}
	// The width is a preference, while maxLines is a hard subtitle-layout
	// constraint. Rebalance all words when the greedy pass needs too many
	// lines.
	return balanceLines(words, maxLines)
}

// balanceLines spreads words over at most maxLines lines of similar width.
// Long words can use up the words early; it stops rather than emit empty
// lines.
func balanceLines(words []string, maxLines int) []string {
	remaining := append([]string(nil), words...)
	var balanced []string
	for len(remaining) > 0 && len(balanced) < maxLines-1 {
		slots := maxLines - len(balanced)
		remainingWidth := len(remaining) - 1
		for _, w := range remaining {
			remainingWidth += cells(w)
		}
		target := (remainingWidth + slots - 1) / slots
		current := remaining[0]
		remaining = remaining[1:]
		for len(remaining) > 0 && cells(current)+1+cells(remaining[0]) <= target {
			current += " " + remaining[0]
			remaining = remaining[1:]
		}
		balanced = append(balanced, current)
	}
	if len(remaining) > 0 {
		balanced = append(balanced, strings.Join(remaining, " "))
	}
	return balanced
}
