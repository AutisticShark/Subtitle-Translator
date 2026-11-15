package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// randomHex returns 2n lowercase hex characters (uuid.uuid4().hex for n=16).
func randomHex(n int) string {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buffer)
}

// uuid4 returns a random RFC 4122 UUID in its hyphenated form (str(uuid.uuid4())).
func uuid4() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	buffer[6] = buffer[6]&0x0f | 0x40
	buffer[8] = buffer[8]&0x3f | 0x80
	text := hex.EncodeToString(buffer)
	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:]
}

func uuidHex() string {
	text := uuid4()
	return strings.ReplaceAll(text, "-", "")
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func utf8Valid(value []byte) bool { return utf8.Valid(value) }

// pythonStr renders a decoded JSON value the way Python's str() renders the
// value json.loads produced, which is how settings were stored. ok is false for
// booleans, nulls, arrays and objects (rejected by validation).
func pythonStr(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		literal := typed.String()
		if !strings.ContainsAny(literal, ".eE") {
			// Python ints print canonically ("-0" becomes "0").
			if parsed, err := strconv.ParseInt(literal, 10, 64); err == nil {
				return strconv.FormatInt(parsed, 10), true
			}
			return strings.TrimPrefix(literal, "+"), true
		}
		parsed, err := strconv.ParseFloat(literal, 64)
		if err != nil {
			if math.IsInf(parsed, 0) {
				if parsed > 0 {
					return "inf", true
				}
				return "-inf", true
			}
			return literal, true
		}
		return pythonFloatRepr(parsed), true
	case float64:
		return pythonFloatRepr(typed), true
	case int:
		return strconv.Itoa(typed), true
	}
	return "", false
}

// pythonFloatRepr mirrors repr(float): shortest round-trip digits, positional
// for exponents in [-4, 16), always with a fractional part, else scientific.
func pythonFloatRepr(value float64) string {
	switch {
	case math.IsInf(value, 1):
		return "inf"
	case math.IsInf(value, -1):
		return "-inf"
	case math.IsNaN(value):
		return "nan"
	}
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	mantissa, exponentText, _ := strings.Cut(scientific, "e")
	exponent, _ := strconv.Atoi(exponentText)
	if exponent >= -4 && exponent < 16 {
		text := strconv.FormatFloat(value, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	sign := "+"
	if exponent < 0 {
		sign = "-"
		exponent = -exponent
	}
	return fmt.Sprintf("%se%s%02d", mantissa, sign, exponent)
}

// pythonJSONStrings mirrors json.dumps(list_of_str) with its default
// ensure_ascii=True and ", " separators. Archive cache names hash this text.
func pythonJSONStrings(values []string) string {
	var out strings.Builder
	out.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			out.WriteString(", ")
		}
		out.WriteByte('"')
		for _, r := range value {
			switch r {
			case '"':
				out.WriteString(`\"`)
			case '\\':
				out.WriteString(`\\`)
			case '\n':
				out.WriteString(`\n`)
			case '\r':
				out.WriteString(`\r`)
			case '\t':
				out.WriteString(`\t`)
			case '\b':
				out.WriteString(`\b`)
			case '\f':
				out.WriteString(`\f`)
			default:
				switch {
				case r < 0x20 || (r >= 0x80 && r <= 0xffff):
					fmt.Fprintf(&out, `\u%04x`, r)
				case r > 0xffff:
					r -= 0x10000
					fmt.Fprintf(&out, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
				default:
					out.WriteRune(r)
				}
			}
		}
		out.WriteByte('"')
	}
	out.WriteByte(']')
	return out.String()
}

// roundHalfEven matches Python 3's round() for progress percentages.
func roundHalfEven(value float64) int { return int(math.RoundToEven(value)) }
