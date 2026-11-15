package engine

import (
	"regexp"
	"strconv"
	"strings"
)

// SupportedExtensions lists the subtitle formats the engine can read.
var SupportedExtensions = map[string]bool{".srt": true, ".vtt": true, ".ass": true, ".ssa": true}

// Document is a parsed subtitle file: the cues plus the format-specific
// envelope needed to write it back with only the cue text replaced.
type Document struct {
	// Format is "srt", "vtt", "ass" or "ssa".
	Format  string
	Cues    []Cue
	Newline string
	BOM     bool

	vtt *vttMeta
	ass *assMeta
}

type vttBlock struct {
	raw        bool
	lines      []string // raw blocks: the block lines
	cue        int      // cue blocks: index into Document.Cues
	identifier []string // cue blocks: lines before the timing line
}

type vttMeta struct {
	header string
	blocks []vttBlock
}

type assRecord struct {
	line      int
	values    []string
	textIndex int
	prefix    string
}

type assMeta struct {
	lines   []string
	records []assRecord
}

// CloneWithCues returns a copy of the document that carries cues instead.
// The envelope is deep-copied, so changing the clone never touches the
// original.
func (d *Document) CloneWithCues(cues []Cue) *Document {
	clone := *d
	clone.Cues = append([]Cue(nil), cues...)
	if d.vtt != nil {
		meta := &vttMeta{header: d.vtt.header, blocks: make([]vttBlock, len(d.vtt.blocks))}
		for i, block := range d.vtt.blocks {
			block.lines = append([]string(nil), block.lines...)
			block.identifier = append([]string(nil), block.identifier...)
			meta.blocks[i] = block
		}
		clone.vtt = meta
	}
	if d.ass != nil {
		meta := &assMeta{
			lines:   append([]string(nil), d.ass.lines...),
			records: make([]assRecord, len(d.ass.records)),
		}
		for i, record := range d.ass.records {
			record.values = append([]string(nil), record.values...)
			meta.records[i] = record
		}
		clone.ass = meta
	}
	return &clone
}

// Render returns the document as text (without BOM).
func (d *Document) Render() (string, error) {
	switch d.Format {
	case "srt":
		return renderSRT(d), nil
	case "vtt":
		return renderVTT(d)
	case "ass", "ssa":
		return renderASS(d)
	}
	return "", formatErrorf("Unsupported format: %s", d.Format)
}

// ToBytes renders the document as UTF-8, with a BOM when the source had one.
func (d *Document) ToBytes() ([]byte, error) {
	text, err := d.Render()
	if err != nil {
		return nil, err
	}
	if d.BOM {
		text = byteOrderMark + text
	}
	return []byte(text), nil
}

// normalizeExtension turns ".SRT", "srt" or a file name such as "Show.en.srt"
// into a lower-case extension with its dot ("" when there is none).
func normalizeExtension(extension string) string {
	ext := strings.ToLower(strings.TrimSpace(extension))
	if i := strings.LastIndexByte(ext, '.'); i >= 0 {
		return ext[i:]
	}
	if ext == "" {
		return ""
	}
	return "." + ext
}

// ParseSubtitle parses raw subtitle bytes. extension is ".srt", ".vtt",
// ".ass" or ".ssa" (with or without the dot, any case; a file name works
// too). encoding is a Python-style codec name and defaults to UTF-8; a UTF-8
// byte order mark always wins over it.
func ParseSubtitle(raw []byte, extension, encoding string) (*Document, error) {
	ext := normalizeExtension(extension)
	if !SupportedExtensions[ext] {
		shown := ext
		if shown == "" {
			shown = "(none)"
		}
		return nil, formatErrorf("Unsupported subtitle type %s. Use SRT, VTT, ASS, or SSA.", shown)
	}
	if encoding == "" {
		encoding = "utf-8"
	}
	bom := len(raw) >= 3 && raw[0] == 0xef && raw[1] == 0xbb && raw[2] == 0xbf
	var (
		text string
		err  error
	)
	if bom {
		text, err = decodeBytes(raw[3:], "utf-8")
	} else {
		text, err = decodeBytes(raw, encoding)
	}
	if err != nil {
		return nil, &SubtitleFormatError{
			Msg: "Could not decode subtitle as " + encoding + ": " + err.Error(),
			Err: err,
		}
	}
	// Detect on decoded text: in UTF-16 the CR and LF bytes are not adjacent.
	newline := "\n"
	if strings.Contains(prefixRunes(text, 8192), "\r\n") {
		newline = "\r\n"
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")

	var doc *Document
	switch ext {
	case ".srt":
		doc = &Document{Format: "srt", Cues: ParseSRT(normalized), Newline: newline, BOM: bom}
	case ".vtt":
		doc, err = parseVTT(normalized, newline, bom)
	default:
		doc, err = parseASS(normalized, ext[1:], newline, bom)
	}
	if err != nil {
		return nil, err
	}
	if len(doc.Cues) == 0 {
		return nil, formatErrorf("No subtitle cues were found in the file.")
	}
	return doc, nil
}

// CountCues parses a subtitle file and returns its number of cues. It is the
// cheap validation step before any provider or key is involved.
func CountCues(source []byte, extension, encoding string) (int, error) {
	doc, err := ParseSubtitle(source, extension, encoding)
	if err != nil {
		return 0, err
	}
	return len(doc.Cues), nil
}

// prefixRunes returns the first n code points of s.
func prefixRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

func renderSRT(d *Document) string {
	out := make([]string, 0, len(d.Cues)*4)
	for _, cue := range d.Cues {
		out = append(out, strconv.Itoa(cue.Index), cue.Start+" --> "+cue.End+cue.Rest)
		if len(cue.Lines) == 0 {
			out = append(out, "")
		} else {
			out = append(out, cue.Lines...)
		}
		out = append(out, "")
	}
	return strings.Join(out, d.Newline)
}

// vttTimingRE: WebVTT hours have two or more digits; a single digit is
// tolerated as well. Matched against the whole stripped line.
var vttTimingRE = regexp.MustCompile(
	`\A(?P<start>(?:\p{Nd}+:)?\p{Nd}{2}:\p{Nd}{2}[.,]\p{Nd}{3})[` + pySpaceClass + `]*-->[` + pySpaceClass + `]*` +
		`(?P<end>(?:\p{Nd}+:)?\p{Nd}{2}:\p{Nd}{2}[.,]\p{Nd}{3})(?P<rest>.*)\z`)

func matchVTTTiming(line string) (timing, bool) {
	m := vttTimingRE.FindStringSubmatch(pyStrip(line))
	if m == nil {
		return timing{}, false
	}
	return timing{
		start: m[vttTimingRE.SubexpIndex("start")],
		end:   m[vttTimingRE.SubexpIndex("end")],
		rest:  m[vttTimingRE.SubexpIndex("rest")],
	}, true
}

func parseVTT(text string, newline string, bom bool) (*Document, error) {
	lines := strings.Split(strings.TrimLeft(text, byteOrderMark), "\n")
	if !strings.HasPrefix(strings.ToUpper(pyStrip(lines[0])), "WEBVTT") {
		return nil, formatErrorf("WebVTT file must begin with WEBVTT.")
	}

	// The header block runs to the first blank line and may carry metadata
	// lines such as `Kind: captions` and `Language: en`. A header cannot hold
	// "-->", so a cue missing its blank separator is still parsed as a cue.
	headerEnd := 1
	for headerEnd < len(lines) && !isBlank(lines[headerEnd]) &&
		!strings.Contains(lines[headerEnd], "-->") {
		headerEnd++
	}

	var cues []Cue
	var blocks []vttBlock
	i := headerEnd
	for i < len(lines) {
		for i < len(lines) && isBlank(lines[i]) {
			i++
		}
		if i >= len(lines) {
			break
		}
		start := i
		for i < len(lines) && !isBlank(lines[i]) {
			i++
		}
		block := lines[start:i]
		timingAt := -1
		var t timing
		for n, line := range block {
			if found, ok := matchVTTTiming(line); ok {
				timingAt, t = n, found
				break
			}
		}
		if timingAt < 0 {
			blocks = append(blocks, vttBlock{raw: true, lines: append([]string(nil), block...)})
			continue
		}
		// The "vtt" dialect decodes character references (`&amp;`) for
		// translation and re-escapes &, < and > in the translated text.
		cues = append(cues, Cue{
			Index:   len(cues) + 1,
			Start:   t.start,
			End:     t.end,
			Rest:    t.rest,
			Lines:   append([]string(nil), block[timingAt+1:]...),
			Dialect: "vtt",
		})
		blocks = append(blocks, vttBlock{
			cue:        len(cues) - 1,
			identifier: append([]string(nil), block[:timingAt]...),
		})
	}
	return &Document{
		Format: "vtt", Cues: cues, Newline: newline, BOM: bom,
		vtt: &vttMeta{header: strings.Join(lines[:headerEnd], "\n"), blocks: blocks},
	}, nil
}

func renderVTT(d *Document) (string, error) {
	header := "WEBVTT"
	var blocks []vttBlock
	if d.vtt != nil {
		header, blocks = d.vtt.header, d.vtt.blocks
	}
	out := append(strings.Split(header, "\n"), "")
	for _, block := range blocks {
		if block.raw {
			out = append(out, block.lines...)
		} else {
			if block.cue < 0 || block.cue >= len(d.Cues) {
				return "", formatErrorf("WebVTT cue block %d has no matching cue", block.cue)
			}
			cue := d.Cues[block.cue]
			out = append(out, block.identifier...)
			out = append(out, cue.Start+" --> "+cue.End+cue.Rest)
			if len(cue.Lines) == 0 {
				out = append(out, "")
			} else {
				out = append(out, cue.Lines...)
			}
		}
		out = append(out, "")
	}
	return strings.Join(out, d.Newline), nil
}

func parseASS(text, format, newline string, bom bool) (*Document, error) {
	lines := strings.Split(strings.TrimLeft(text, byteOrderMark), "\n")
	inEvents := false
	var fields []string
	var cues []Cue
	var records []assRecord

	for lineIndex, line := range lines {
		stripped := pyStrip(line)
		if strings.HasPrefix(stripped, "[") && strings.HasSuffix(stripped, "]") {
			inEvents = strings.ToLower(stripped) == "[events]"
			continue
		}
		if !inEvents {
			continue
		}
		lower := strings.ToLower(stripped)
		if strings.HasPrefix(lower, "format:") {
			_, after, _ := strings.Cut(line, ":")
			fields = fields[:0:0]
			for _, part := range strings.Split(after, ",") {
				fields = append(fields, strings.ToLower(pyStrip(part)))
			}
			continue
		}
		if !strings.HasPrefix(lower, "dialogue:") {
			continue
		}
		if len(fields) == 0 {
			return nil, formatErrorf("ASS/SSA [Events] section has no Format line.")
		}
		colon := strings.IndexByte(line, ':')
		content := line[colon+1:]
		trimmed := pyLStrip(content)
		leadingSpace := content[:len(content)-len(trimmed)]
		values := strings.SplitN(trimmed, ",", len(fields))
		textIndex := indexOf(fields, "text")
		if len(values) != len(fields) || textIndex < 0 {
			return nil, formatErrorf("Malformed ASS/SSA Dialogue line.")
		}
		startIndex, endIndex := indexOf(fields, "start"), indexOf(fields, "end")
		cue := Cue{Index: len(cues) + 1, Dialect: "ass"}
		if startIndex >= 0 {
			cue.Start = values[startIndex]
		}
		if endIndex >= 0 {
			cue.End = values[endIndex]
		}
		// Only \N is a hard line break. The soft break \n and the hard space
		// \h stay in the text, where the translation engine masks them.
		cue.Lines = strings.Split(values[textIndex], `\N`)
		cues = append(cues, cue)
		records = append(records, assRecord{
			line:      lineIndex,
			values:    values,
			textIndex: textIndex,
			prefix:    line[:colon+1] + leadingSpace,
		})
	}
	return &Document{
		Format: format, Cues: cues, Newline: newline, BOM: bom,
		ass: &assMeta{lines: lines, records: records},
	}, nil
}

func indexOf(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}

func renderASS(d *Document) (string, error) {
	if d.ass == nil {
		return "", formatErrorf("ASS/SSA document has no script envelope")
	}
	if len(d.Cues) != len(d.ass.records) {
		return "", formatErrorf("ASS/SSA document has %d cues for %d dialogue lines",
			len(d.Cues), len(d.ass.records))
	}
	lines := append([]string(nil), d.ass.lines...)
	for n, cue := range d.Cues {
		record := d.ass.records[n]
		values := append([]string(nil), record.values...)
		// ASS uses a literal \N for an explicit subtitle line break.
		values[record.textIndex] = strings.Join(cue.Lines, `\N`)
		lines[record.line] = record.prefix + strings.Join(values, ",")
	}
	return strings.Join(lines, d.Newline), nil
}
