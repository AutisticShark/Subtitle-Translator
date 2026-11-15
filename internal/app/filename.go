package app

import (
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	maxFilenameStemBytes = 150
	fallbackFilenameStem = "subtitle"
)

var (
	supportedExtensions      = map[string]bool{".srt": true, ".vtt": true, ".ass": true, ".ssa": true}
	unsafeFilenameCharacters = "\"'<>:|?*\\/"
	windowsReservedStems     = func() map[string]bool {
		names := map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}
		for _, prefix := range []string{"com", "lpt"} {
			for number := '1'; number <= '9'; number++ {
				names[prefix+string(number)] = true
			}
		}
		return names
	}()
	multipleSpaces = regexp.MustCompile(` {2,}`)
	pathSeparators = regexp.MustCompile(`[\\/]`)
)

func isUnicodeSpace(r rune) bool { return unicode.IsSpace(r) }

// assigned reports whether r has a Unicode general category (Python's Cn is unassigned).
func assigned(r rune) bool {
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.C)
}

// uploadDisplayName mirrors webapp.upload_display_name: a Unicode-preserving,
// filesystem-safe name, or "" when the extension is unsupported. The extension
// is checked on the client's original name; the uploaded source itself is always
// stored as source.<ext>, so this name only labels translated outputs.
func uploadDisplayName(raw string) string {
	name := norm.NFC.String(raw)
	parts := pathSeparators.Split(name, -1)
	name = pythonStrip(parts[len(parts)-1])
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return ""
	}
	stem, extension := name[:dot], name[dot+1:]
	if !supportedExtensions["."+strings.ToLower(extension)] {
		return ""
	}
	var cleaned strings.Builder
	for _, r := range stem {
		switch {
		case r == utf8.RuneError || unicode.Is(unicode.C, r) || !assigned(r) ||
			strings.ContainsRune(unsafeFilenameCharacters, r):
			continue
		case unicode.Is(unicode.Z, r):
			cleaned.WriteByte(' ')
		default:
			cleaned.WriteRune(r)
		}
	}
	clean := strings.Trim(multipleSpaces.ReplaceAllString(cleaned.String(), " "), " .")
	if len(clean) > maxFilenameStemBytes {
		clean = clean[:maxFilenameStemBytes]
		for len(clean) > 0 && !utf8.ValidString(clean) {
			clean = clean[:len(clean)-1]
		}
	}
	clean = strings.Trim(clean, " .")
	if clean == "" {
		clean = fallbackFilenameStem
	}
	first, _, _ := strings.Cut(clean, ".")
	if windowsReservedStems[strings.ToLower(pythonStrip(first))] {
		clean = "_" + clean
	}
	return clean + "." + extension
}

// pathStem and pathSuffix follow pathlib.PurePath.stem/suffix for one name.
func pathSuffix(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	dot := strings.LastIndex(name, ".")
	if dot <= 0 || dot == len(name)-1 {
		return ""
	}
	return name[dot:]
}

func pathStem(name string) string {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	return strings.TrimSuffix(base, pathSuffix(base))
}

// contentDisposition matches Werkzeug's send_file(as_attachment=True): an ASCII
// fallback plus an RFC 5987 filename* for non-ASCII names.
func contentDisposition(name string) string {
	ascii := true
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return "attachment; " + quoteHeaderValue("filename", name)
	}
	var simple strings.Builder
	for _, r := range norm.NFKD.String(name) {
		if r < 0x80 {
			simple.WriteRune(r)
		}
	}
	return "attachment; " + quoteHeaderValue("filename", simple.String()) +
		"; filename*=UTF-8''" + rfc5987Escape(name)
}

func quoteHeaderValue(key, value string) string {
	token := value != ""
	for _, r := range value {
		if !(r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("!#$%&'*+-.^_`|~", r))) {
			token = false
			break
		}
	}
	if token {
		return key + "=" + value
	}
	return key + `="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// rfc5987Escape is urllib.parse.quote(value, safe="!#$&+^`|") over UTF-8 bytes.
func rfc5987Escape(value string) string {
	const upperHex = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if isAlnum(ch) || strings.IndexByte("_.-~!#$&+^`|", ch) >= 0 {
			out.WriteByte(ch)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(upperHex[ch>>4])
		out.WriteByte(upperHex[ch&0xf])
	}
	return out.String()
}

func isAlnum(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
}
