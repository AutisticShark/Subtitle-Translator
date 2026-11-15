package app

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultLocale = "en"
	localeCookie  = "ui_locale"
)

// catalogs mirrors i18n.py: locales/*.json files with "locale", "name",
// "aliases" and "messages". English source strings are the fallback.
type catalogs struct {
	messages map[string]map[string]string
	labels   map[string]string
	order    []string
	aliases  map[string]string
}

type catalogFile struct {
	Locale   string          `json:"locale"`
	Name     string          `json:"name"`
	Aliases  []any           `json:"aliases"`
	Messages json.RawMessage `json:"messages"`
}

func loadCatalogs(locales fs.FS) (*catalogs, error) {
	if locales == nil {
		return nil, fmt.Errorf("missing localization catalogs")
	}
	names, err := fs.Glob(locales, "*.json")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	result := &catalogs{
		messages: map[string]map[string]string{},
		labels:   map[string]string{},
		aliases:  map[string]string{},
	}
	for _, name := range names {
		raw, err := fs.ReadFile(locales, name)
		if err != nil {
			return nil, err
		}
		var file catalogFile
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, fmt.Errorf("invalid localization catalog %s: %w", name, err)
		}
		messages := map[string]string{}
		if len(file.Messages) > 0 && string(file.Messages) != "null" {
			if err := json.Unmarshal(file.Messages, &messages); err != nil {
				return nil, fmt.Errorf("invalid localization catalog: %s", name)
			}
		}
		locale := file.Locale
		if locale == "" {
			locale = strings.TrimSuffix(path.Base(name), ".json")
		}
		if _, seen := result.messages[locale]; !seen {
			result.order = append(result.order, locale)
		}
		result.messages[locale] = messages
		result.labels[locale] = locale
		if file.Name != "" {
			result.labels[locale] = file.Name
		}
		result.aliases[aliasKey(locale)] = locale
		for _, alias := range file.Aliases {
			result.aliases[aliasKey(fmt.Sprint(alias))] = locale
		}
	}
	if _, ok := result.messages[defaultLocale]; !ok {
		return nil, fmt.Errorf("missing default localization catalog: %s", defaultLocale)
	}
	return result, nil
}

func aliasKey(value string) string {
	return strings.ReplaceAll(strings.ToLower(value), "_", "-")
}

// normalize returns the supported canonical locale for a user-supplied tag,
// trying progressively shorter prefixes (zh-Hant-HK reaches zh-Hant before zh).
func (c *catalogs) normalize(value string) string {
	candidate := aliasKey(strings.TrimSpace(value))
	if candidate == "" {
		return ""
	}
	subtags := strings.Split(candidate, "-")
	for length := len(subtags); length > 0; length-- {
		if locale, ok := c.aliases[strings.Join(subtags[:length], "-")]; ok {
			return locale
		}
	}
	return ""
}

// requestLocale applies the documented order: ?lang=, the ui_locale cookie,
// Accept-Language (by quality), then English.
func (c *catalogs) requestLocale(r *http.Request) string {
	if locale := c.normalize(r.URL.Query().Get("lang")); locale != "" {
		return locale
	}
	if cookie, err := r.Cookie(localeCookie); err == nil {
		if locale := c.normalize(cookie.Value); locale != "" {
			return locale
		}
	}
	for _, tag := range acceptedLanguages(r.Header.Values("Accept-Language")) {
		if locale := c.normalize(tag); locale != "" {
			return locale
		}
	}
	return defaultLocale
}

// acceptedLanguages parses Accept-Language like Werkzeug: entries with q > 0,
// ordered by quality (stable for equal qualities).
func acceptedLanguages(headers []string) []string {
	type entry struct {
		tag     string
		quality float64
	}
	var entries []entry
	for _, header := range headers {
		for _, item := range strings.Split(header, ",") {
			parts := strings.Split(item, ";")
			tag := strings.TrimSpace(parts[0])
			if tag == "" {
				continue
			}
			quality := 1.0
			for _, parameter := range parts[1:] {
				name, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
				if ok && strings.EqualFold(strings.TrimSpace(name), "q") {
					parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
					if err != nil {
						parsed = 0
					}
					quality = parsed
				}
			}
			if quality > 0 {
				entries = append(entries, entry{tag, quality})
			}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].quality > entries[j].quality })
	tags := make([]string, len(entries))
	for i, item := range entries {
		tags[i] = item.tag
	}
	return tags
}

// translate looks message up in locale's catalog and substitutes {name}
// fields like Python's str.format with keyword arguments.
func (c *catalogs) translate(locale, message string, args ...any) string {
	translated := message
	if catalog, ok := c.messages[locale]; ok {
		if value, ok := catalog[message]; ok {
			translated = value
		}
	}
	if len(args) == 0 {
		return translated
	}
	values := make(map[string]string, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		values[fmt.Sprint(args[i])] = fmt.Sprint(args[i+1])
	}
	return formatFields(translated, values)
}

func formatFields(template string, values map[string]string) string {
	var out strings.Builder
	for i := 0; i < len(template); i++ {
		ch := template[i]
		switch {
		case ch == '{' && i+1 < len(template) && template[i+1] == '{':
			out.WriteByte('{')
			i++
		case ch == '}' && i+1 < len(template) && template[i+1] == '}':
			out.WriteByte('}')
			i++
		case ch == '{':
			end := strings.IndexByte(template[i:], '}')
			if end < 0 {
				out.WriteString(template[i:])
				return out.String()
			}
			name := template[i+1 : i+end]
			if value, ok := values[name]; ok {
				out.WriteString(value)
			} else {
				out.WriteString(template[i : i+end+1])
			}
			i += end
		default:
			out.WriteByte(ch)
		}
	}
	return out.String()
}

// localeLabels returns the selectable locales in catalog order.
func (c *catalogs) localeLabels() []localeLabel {
	labels := make([]localeLabel, 0, len(c.order))
	for _, code := range c.order {
		labels = append(labels, localeLabel{Code: code, Label: c.labels[code]})
	}
	return labels
}

type localeLabel struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}
