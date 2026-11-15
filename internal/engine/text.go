package engine

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Python's str.strip(), str.split() and str.splitlines() use a different
// whitespace set than Go's unicode.IsSpace (the information separators
// U+001C..U+001F count as whitespace). The helpers here match Python so line
// handling stays identical.

func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// pySpaceClass is the body of a regexp character class matching Python's \s.
const pySpaceClass = `\t\n\v\f\r \x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

func pyLStrip(s string) string { return strings.TrimLeftFunc(s, isPySpace) }

func pyFields(s string) []string { return strings.FieldsFunc(s, isPySpace) }

// splitLines mirrors str.splitlines(): it splits on \n, \r, \r\n, \v, \f,
// the information separators, NEL, LS and PS, and drops the terminators.
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, s[start:i])
			i += size
			start = i
		case '\r':
			lines = append(lines, s[start:i])
			i += size
			if i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
		default:
			i += size
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// digitValue returns the decimal value of a Unicode decimal digit (category
// Nd). Python's int() accepts any of them, so a model answering with
// full-width digits still yields a usable placeholder number.
func digitValue(r rune) int {
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	n := 0
	for x := r - 1; x >= 0 && unicode.IsDigit(x); x-- {
		n++
	}
	return n % 10
}

// maxDigits is the saturation value of parseDigits. Anything this large is
// out of range for every caller (placeholder numbers, line numbers), exactly
// as the arbitrary-precision int of the Python code would be.
const maxDigits = 1 << 30

// parseDigits converts a run of Unicode decimal digits, saturating instead
// of overflowing.
func parseDigits(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + digitValue(r)
		if n > maxDigits {
			return maxDigits
		}
	}
	return n
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// hasCJK reports whether r is in the kana/ideograph ranges the wrapping and
// continuation logic treats as CJK.
func hasCJK(r rune) bool {
	return r >= 0x3040 && r <= 0x30ff ||
		r >= 0x3400 && r <= 0x4dbf ||
		r >= 0x4e00 && r <= 0x9fff ||
		r >= 0xf900 && r <= 0xfaff
}

func containsCJK(s string) bool {
	for _, r := range s {
		if hasCJK(r) {
			return true
		}
	}
	return false
}

// firstRune and lastRune return RuneError for an empty string.
func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

// byteOrderMark is U+FEFF, stripped from the start of decoded text.
const byteOrderMark = "\ufeff"
