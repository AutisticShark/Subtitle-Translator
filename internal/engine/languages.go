package engine

import (
	"path"
	"regexp"
	"strings"
)

// Language describes one target language.
type Language struct {
	// Suffix is inserted before the file extension: show.srt -> show.zh.tw.srt.
	Suffix string
	// Name is the English display name, also used in the LLM prompt.
	Name string
	// DeepL is the DeepL target_lang code.
	DeepL string
	// Style is extra prompt guidance for LLM providers; may be empty.
	Style string
}

// Languages is the target language table, keyed by BCP-47 style code. The
// Google provider sends the key itself as the target.
var Languages = map[string]Language{
	"zh-TW": {
		Suffix: ".zh.tw",
		Name:   "Traditional Chinese (Taiwan)",
		DeepL:  "ZH-HANT",
		Style: "Use Taiwan Mandarin vocabulary and idiom, not Chinese vocabulary " +
			"mechanically converted to traditional glyphs. Use Taiwan full-width " +
			"punctuation conventions.",
	},
	"zh-CN": {
		Suffix: ".zh.cn",
		Name:   "Simplified Chinese (China)",
		DeepL:  "ZH-HANS",
		Style:  "Use Chinese Mandarin vocabulary and standard simplified punctuation.",
	},
	"ja":    {Suffix: ".ja", Name: "Japanese", DeepL: "JA"},
	"ko":    {Suffix: ".ko", Name: "Korean", DeepL: "KO"},
	"es":    {Suffix: ".es", Name: "Spanish", DeepL: "ES"},
	"fr":    {Suffix: ".fr", Name: "French", DeepL: "FR"},
	"de":    {Suffix: ".de", Name: "German", DeepL: "DE"},
	"it":    {Suffix: ".it", Name: "Italian", DeepL: "IT"},
	"pt-BR": {Suffix: ".pt.br", Name: "Brazilian Portuguese", DeepL: "PT-BR"},
	"ru":    {Suffix: ".ru", Name: "Russian", DeepL: "RU"},
	"nl":    {Suffix: ".nl", Name: "Dutch", DeepL: "NL"},
	"pl":    {Suffix: ".pl", Name: "Polish", DeepL: "PL"},
	"tr":    {Suffix: ".tr", Name: "Turkish", DeepL: "TR"},
	"uk":    {Suffix: ".uk", Name: "Ukrainian", DeepL: "UK"},
	"id":    {Suffix: ".id", Name: "Indonesian", DeepL: "ID"},
}

// LanguageOrder lists the Languages keys in the order of the original table,
// for UIs that need a stable order (Go maps iterate randomly).
var LanguageOrder = []string{
	"zh-TW", "zh-CN", "ja", "ko", "es", "fr", "de", "it", "pt-BR", "ru", "nl",
	"pl", "tr", "uk", "id",
}

// cjkLanguages are the targets that wrap by character instead of by word.
var cjkLanguages = map[string]bool{"zh-TW": true, "zh-CN": true, "ja": true}

// IsCJKLanguage reports whether target is wrapped by character.
func IsCJKLanguage(target string) bool { return cjkLanguages[target] }

var englishSuffixRE = regexp.MustCompile(`(?i)\.(?:en|eng|english)$`)

// TranslatedFilename names the output file for a source file name and target
// language: a trailing .en/.eng/.english is dropped from the stem (case
// insensitive), the language suffix is added, and the extension is lowered:
// "show.en.ass" + zh-TW -> "show.zh.tw.ass". A directory part of filename is
// ignored. An unknown target falls back to a suffix derived from its code.
func TranslatedFilename(filename, target string) string {
	suffix := ""
	if language, ok := Languages[target]; ok {
		suffix = language.Suffix
	} else if target != "" {
		suffix = "." + strings.ToLower(strings.ReplaceAll(target, "-", "."))
	}
	return TranslatedFilenameSuffix(filename, suffix)
}

// TranslatedFilenameSuffix is TranslatedFilename with an explicit language
// suffix such as ".zh.tw".
func TranslatedFilenameSuffix(filename, languageSuffix string) string {
	stem, extension := splitName(filename)
	stem = englishSuffixRE.ReplaceAllString(stem, "")
	return stem + languageSuffix + strings.ToLower(extension)
}

// splitName follows pathlib.PurePosixPath: the last path component, and the
// suffix after its last dot unless that dot starts or ends the name.
func splitName(filename string) (stem, extension string) {
	name := path.Base(strings.TrimRight(filename, "/"))
	if name == "." || name == "/" {
		name = ""
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 && i < len(name)-1 {
		return name[:i], name[i:]
	}
	return name, ""
}
