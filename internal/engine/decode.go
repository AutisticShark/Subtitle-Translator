package engine

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// decodeBytes decodes raw with a Python codec name, resolved through Python's
// own normalization and alias table. Like Python's strict decoding, it fails
// on invalid input instead of substituting replacement characters, and it
// accepts and produces exactly what Python does (see pycodecs.go). UTF-16/32
// without an explicit byte order honour a BOM and otherwise assume
// little-endian.
func decodeBytes(raw []byte, encoding string) (string, error) {
	codec := "utf_8"
	if strings.TrimSpace(encoding) != "" {
		codec = pythonCodec(encoding)
	}
	switch codec {
	case "utf_8":
		return decodeUTF8(raw)
	case "utf_8_sig":
		return decodeUTF8(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf}))
	case "utf_16":
		return decodeUTF16(raw, 'b', "utf-16")
	case "utf_16_le":
		return decodeUTF16(raw, 'l', "utf-16-le")
	case "utf_16_be":
		return decodeUTF16(raw, 'B', "utf-16-be")
	case "utf_32":
		return decodeUTF32(raw, 'b', "utf-32")
	case "utf_32_le":
		return decodeUTF32(raw, 'l', "utf-32-le")
	case "utf_32_be":
		return decodeUTF32(raw, 'B', "utf-32-be")
	case "ascii":
		for i, b := range raw {
			if b >= 0x80 {
				return "", fmt.Errorf("'ascii' codec can't decode byte 0x%02x in position %d: ordinal not in range(128)", b, i)
			}
		}
		return string(raw), nil
	case "latin_1":
		// True ISO-8859-1: every byte is the code point of the same value.
		runes := make([]rune, len(raw))
		for i, b := range raw {
			runes[i] = rune(b)
		}
		return string(runes), nil
	}
	if table, ok := pySingleByteTables[codec]; ok {
		return decodeCharmap(raw, table)
	}
	if mb, ok := pyMultiByteCodecs[codec]; ok {
		return mb.decode(raw, codec)
	}
	// Codecs without an exact decoder here (utf_7, hz, johab, the ISO-2022
	// family, the JIS X 0213 codecs) are refused rather than approximated.
	return "", errors.New("unknown encoding: " + encoding)
}

func decodeUTF8(raw []byte) (string, error) {
	if utf8.Valid(raw) {
		return string(raw), nil
	}
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size <= 1 {
			reason := "invalid continuation byte"
			switch {
			case raw[i] >= 0x80 && raw[i] < 0xc2 || raw[i] > 0xf4:
				reason = "invalid start byte"
			case !utf8.FullRune(raw[i:]):
				reason = "unexpected end of data"
			}
			return "", fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", raw[i], i, reason)
		}
		i += size
	}
	return "", errors.New("'utf-8' codec can't decode the data")
}

// decodeUTF16 decodes UTF-16. order: 'l' little endian, 'B' big endian, 'b'
// BOM-detected (little endian without one).
func decodeUTF16(raw []byte, order byte, codec string) (string, error) {
	big := order == 'B'
	if order == 'b' && len(raw) >= 2 {
		switch {
		case raw[0] == 0xff && raw[1] == 0xfe:
			raw = raw[2:]
		case raw[0] == 0xfe && raw[1] == 0xff:
			raw, big = raw[2:], true
		}
	}
	if len(raw)%2 != 0 {
		return "", fmt.Errorf("'%s' codec can't decode byte 0x%02x in position %d: truncated data",
			codec, raw[len(raw)-1], len(raw)-1)
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		if big {
			units[i] = uint16(raw[2*i])<<8 | uint16(raw[2*i+1])
		} else {
			units[i] = uint16(raw[2*i+1])<<8 | uint16(raw[2*i])
		}
	}
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u >= 0xd800 && u < 0xdc00:
			if i+1 >= len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
				return "", fmt.Errorf("'%s' codec can't decode bytes in position %d-%d: illegal UTF-16 surrogate",
					codec, 2*i, 2*i+1)
			}
			i++
		case u >= 0xdc00 && u <= 0xdfff:
			return "", fmt.Errorf("'%s' codec can't decode bytes in position %d-%d: illegal encoding",
				codec, 2*i, 2*i+1)
		}
	}
	return string(utf16.Decode(units)), nil
}

func decodeUTF32(raw []byte, order byte, codec string) (string, error) {
	big := order == 'B'
	if order == 'b' && len(raw) >= 4 {
		switch {
		case bytes.HasPrefix(raw, []byte{0xff, 0xfe, 0x00, 0x00}):
			raw = raw[4:]
		case bytes.HasPrefix(raw, []byte{0x00, 0x00, 0xfe, 0xff}):
			raw, big = raw[4:], true
		}
	}
	if len(raw)%4 != 0 {
		return "", fmt.Errorf("'%s' codec can't decode bytes in position %d-%d: truncated data",
			codec, len(raw)-len(raw)%4, len(raw)-1)
	}
	var out strings.Builder
	for i := 0; i < len(raw); i += 4 {
		var v uint32
		if big {
			v = uint32(raw[i])<<24 | uint32(raw[i+1])<<16 | uint32(raw[i+2])<<8 | uint32(raw[i+3])
		} else {
			v = uint32(raw[i+3])<<24 | uint32(raw[i+2])<<16 | uint32(raw[i+1])<<8 | uint32(raw[i])
		}
		if v > 0x10ffff || v >= 0xd800 && v <= 0xdfff {
			return "", fmt.Errorf("'%s' codec can't decode bytes in position %d-%d: code point not in range(0x110000)",
				codec, i, i+3)
		}
		out.WriteRune(rune(v))
	}
	return out.String(), nil
}
