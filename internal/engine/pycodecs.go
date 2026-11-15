package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
)

// Python-exact legacy decoding. The WHATWG decoders in golang.org/x/text are
// supersets of Python's codecs (Big5 is Big5-HKSCS, GBK reads GB18030, EUC-KR
// is UHC, ISO-8859-9 is windows-1254, ...) and map a few hundred sequences to
// other characters. Uploads must decode exactly as the Python application
// did: the same bytes accepted, the same text produced, everything else
// rejected rather than silently changed. pycodecs_data.go, generated from
// Python's codecs, holds the charmap tables and, for multibyte codecs, the
// sequences Python accepts plus Python's own output wherever the WHATWG
// mapping differs.

// pyMultiByte describes one multibyte codec.
type pyMultiByte struct {
	// whatwg is the htmlindex label whose decoder supplies the mapping of
	// accepted sequences that have no override.
	whatwg string
	// kinds classifies bytes 0x80..0xFF: 'x' invalid, 's' a single-byte
	// character, '2'/'3' the lead byte of a 2/3-byte sequence, 'g' a GB18030
	// lead (a 4-byte sequence when the next byte is 0x30..0x39, else 2-byte).
	kinds string
	// valid2 and valid3 hold inclusive [start, end] ranges of the accepted
	// sequences, each read as a big-endian number.
	valid2, valid3 []uint32
	// valid4 holds the accepted GB18030 4-byte sequences as ranges of their
	// linear index (see gbLinear).
	valid4 []uint32
	// makeup, for EUC-KR only, holds the choseong, jungseong and jongseong
	// indexes of the KS X 1001:1998 Annex 3 make-up sequence
	// A4 D4 A4 <cho> A4 <jung> A4 <jong>.
	makeup *[3]map[byte]int
	// overrides is Python's output where it differs from the WHATWG decoder,
	// keyed like valid2/valid3 (single bytes by their value); overrideRuns
	// holds runs of them as [start, end, first rune] triples, codes start..end
	// mapping to consecutive code points; overrides4 is keyed by linear index.
	overrides    map[uint32]string
	overrideRuns []uint32
	overrides4   map[uint32]string

	once sync.Once
	enc  encoding.Encoding
}

// normalizeCodecName mirrors Python's codec lookup: ASCII lower case, then
// encodings.normalize_encoding (runs of characters other than alphanumerics
// and '.' become one '_', non-ASCII alphanumerics are dropped).
func normalizeCodecName(name string) string {
	var out strings.Builder
	punct := false
	for _, r := range name {
		if r < utf8.RuneSelf && 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '.' {
			if punct && out.Len() > 0 {
				out.WriteByte('_')
			}
			if r < utf8.RuneSelf {
				out.WriteRune(r)
			}
			punct = false
		} else {
			punct = true
		}
	}
	return out.String()
}

// pythonCodec resolves an encoding name to Python's codec module name, or
// returns "" when Python would not find it. (Python keeps '.' in normalized
// names and never imports a module whose name holds one, so "utf.8" is
// unknown; the alias keys hold no '.' either.)
func pythonCodec(name string) string {
	return pyCodecAliases[normalizeCodecName(name)]
}

// decodeCharmap decodes with a Python charmap table (U+FFFE is undefined).
func decodeCharmap(raw []byte, table string) (string, error) {
	runes := []rune(table)
	var out strings.Builder
	out.Grow(len(raw))
	for i, b := range raw {
		r := runes[b]
		if r == 0xfffe {
			return "", fmt.Errorf("'charmap' codec can't decode byte 0x%02x in position %d: character maps to <undefined>", b, i)
		}
		out.WriteRune(r)
	}
	return out.String(), nil
}

func inRanges(ranges []uint32, code uint32) bool {
	// ranges is sorted pairs; find the first pair whose end is >= code.
	n := len(ranges) / 2
	i := sort.Search(n, func(k int) bool { return ranges[2*k+1] >= code })
	return i < n && ranges[2*i] <= code
}

func (c *pyMultiByte) decode(raw []byte, codec string) (string, error) {
	c.once.Do(func() {
		enc, err := htmlindex.Get(c.whatwg)
		if err != nil {
			panic("engine: missing WHATWG decoder " + c.whatwg)
		}
		c.enc = enc
	})
	illegal := func(pos int) error {
		return fmt.Errorf("'%s' codec can't decode byte 0x%02x in position %d: illegal multibyte sequence", codec, raw[pos], pos)
	}
	incomplete := func(pos int) error {
		return fmt.Errorf("'%s' codec can't decode byte 0x%02x in position %d: incomplete multibyte sequence", codec, raw[pos], pos)
	}
	decoder := c.enc.NewDecoder()
	var out strings.Builder
	out.Grow(len(raw))
	for i := 0; i < len(raw); {
		b := raw[i]
		if b < 0x80 {
			out.WriteByte(b)
			i++
			continue
		}
		n := 0
		switch c.kinds[b-0x80] {
		case 's':
			n = 1
		case '2':
			n = 2
		case '3':
			n = 3
		case 'g':
			if i+1 >= len(raw) {
				return "", incomplete(i)
			}
			if second := raw[i+1]; second < 0x30 || second > 0x39 {
				n = 2
				break
			}
			if i+4 > len(raw) {
				return "", incomplete(i)
			}
			b3, b4 := raw[i+2], raw[i+3]
			if b3 < 0x81 || b3 > 0xfe || b4 < 0x30 || b4 > 0x39 {
				return "", illegal(i)
			}
			index := gbLinear(raw[i : i+4])
			if !inRanges(c.valid4, index) {
				return "", illegal(i)
			}
			if text, ok := c.overrides4[index]; ok {
				out.WriteString(text)
			} else if err := c.whatwgToken(decoder, &out, raw[i:i+4]); err != nil {
				return "", fmt.Errorf("'%s' codec: %v at position %d", codec, err, i)
			}
			i += 4
			continue
		default:
			return "", illegal(i)
		}
		if c.makeup != nil && b == 0xa4 && i+1 < len(raw) && raw[i+1] == 0xd4 {
			if i+8 > len(raw) {
				return "", incomplete(i)
			}
			seq := raw[i : i+8]
			cho, ok1 := c.makeup[0][seq[3]]
			jung, ok2 := c.makeup[1][seq[5]]
			jong, ok3 := c.makeup[2][seq[7]]
			if seq[2] != 0xa4 || seq[4] != 0xa4 || seq[6] != 0xa4 || !ok1 || !ok2 || !ok3 {
				return "", illegal(i)
			}
			out.WriteRune(rune(0xac00 + cho*588 + jung*28 + jong))
			i += 8
			continue
		}
		if i+n > len(raw) {
			return "", incomplete(i)
		}
		var code uint32
		for _, x := range raw[i : i+n] {
			code = code<<8 | uint32(x)
		}
		switch n {
		case 2:
			if !inRanges(c.valid2, code) {
				return "", illegal(i)
			}
		case 3:
			if !inRanges(c.valid3, code) {
				return "", illegal(i)
			}
		}
		if text, ok := c.overrides[code]; ok {
			out.WriteString(text)
		} else if r, ok := runOverride(c.overrideRuns, code); ok {
			out.WriteRune(r)
		} else if err := c.whatwgToken(decoder, &out, raw[i:i+n]); err != nil {
			return "", fmt.Errorf("'%s' codec: %v at position %d", codec, err, i)
		}
		i += n
	}
	return out.String(), nil
}

// whatwgToken decodes one accepted sequence with the WHATWG decoder.
func (c *pyMultiByte) whatwgToken(decoder *encoding.Decoder, out *strings.Builder, token []byte) error {
	var dst [64]byte
	decoder.Reset()
	written, _, err := decoder.Transform(dst[:], token, true)
	// No U+FFFD check: GB18030 maps 84 31 A4 37 to U+FFFD itself, and the
	// generator verified every accepted sequence against this decoder.
	if err != nil || written == 0 {
		return errors.New("internal table mismatch")
	}
	out.Write(dst[:written])
	return nil
}

// gbLinear is the linear index of a GB18030 4-byte sequence.
func gbLinear(seq []byte) uint32 {
	return ((uint32(seq[0])-0x80)*10+uint32(seq[1])-0x30)*1260 + (uint32(seq[2])-0x81)*10 + uint32(seq[3]) - 0x30
}

// runOverride looks code up in [start, end, first rune] triples.
func runOverride(runs []uint32, code uint32) (rune, bool) {
	n := len(runs) / 3
	i := sort.Search(n, func(k int) bool { return runs[3*k+1] >= code })
	if i < n && runs[3*i] <= code {
		return rune(runs[3*i+2] + code - runs[3*i]), true
	}
	return 0, false
}
