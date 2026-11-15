package app

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"
)

// bootData is embedded in the initial HTML as
// <script id="app-boot" type="application/json">, so the UI starts with the
// right locale and theme before its module script runs (no theme flash). JSON
// script blocks are data, not code, so the CSP needs no inline-script allowance.
type bootData struct {
	Locale      string            `json:"locale"`
	Theme       string            `json:"theme"`
	Locales     []localeLabel     `json:"locales"`
	MaxUploadMB int               `json:"max_upload_mb"`
	Languages   []languageLabel   `json:"languages"`
	Messages    map[string]string `json:"messages"`
}

type languageLabel struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func (s *Server) languageLabels(locale string) []languageLabel {
	labels := make([]languageLabel, 0, len(s.languages))
	for _, code := range languageOrder() {
		if language, ok := s.languages[code]; ok {
			labels = append(labels, languageLabel{Code: code, Name: s.tr(locale, language.Name)})
		}
	}
	return labels
}

// handleIndex renders the UI shell with <html lang data-theme>, a translated
// title and the boot data, and persists an explicit ?lang= choice.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if s.indexHTML == nil {
		http.NotFound(w, r)
		return
	}
	locale := localeOf(r)
	theme := "system"
	if cookie, err := r.Cookie(accessCookieName); err == nil && cookie.Value != "" {
		// Invalid or expired credentials still receive the public shell.
		if auth, failure := s.authenticate(r); failure == nil && auth.user.Active && accountThemes[auth.user.Theme] {
			theme = auth.user.Theme
		}
	} else if r.Header.Get("Authorization") != "" {
		if auth, failure := s.authenticate(r); failure == nil && auth.user.Active && accountThemes[auth.user.Theme] {
			theme = auth.user.Theme
		}
	}
	boot := bootData{
		Locale: locale, Theme: theme, Locales: s.catalogs.localeLabels(), MaxUploadMB: s.cfg.MaxUploadMB,
		Languages: s.languageLabels(locale), Messages: s.catalogs.messages[locale],
	}
	if boot.Messages == nil {
		boot.Messages = map[string]string{}
	}
	page, err := renderIndex(s.indexHTML, boot, s.tr(locale, "Subtitle Translator"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if selected := s.catalogs.normalize(r.URL.Query().Get("lang")); selected != "" {
		cookie := &http.Cookie{
			Name: localeCookie, Value: selected, Path: "/", MaxAge: 365 * 24 * 60 * 60,
			Expires: s.now().Add(365 * 24 * time.Hour), Secure: s.cfg.CookieSecure,
			SameSite: http.SameSiteStrictMode,
		}
		w.Header().Add("Set-Cookie", cookie.String())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

// renderIndex rewrites the built index.html token by token: it sets lang and
// data-theme on <html>, data-locale on <body>, replaces the <title> text, and
// inserts the boot JSON before </head>. Everything else is copied verbatim.
func renderIndex(source []byte, boot bootData, title string) ([]byte, error) {
	payload, err := json.Marshal(boot)
	if err != nil {
		return nil, err
	}
	// json.Marshal escapes <, > and & (and U+2028/U+2029), so the payload
	// cannot close the script element or start a comment.
	script := `<script id="app-boot" type="application/json">` + string(payload) + `</script>`
	var out bytes.Buffer
	tokenizer := xhtml.NewTokenizer(bytes.NewReader(source))
	inTitle, titleDone, bootDone := false, false, false
	for {
		kind := tokenizer.Next()
		if kind == xhtml.ErrorToken {
			if tokenizer.Err() == io.EOF {
				break
			}
			return nil, tokenizer.Err()
		}
		raw := tokenizer.Raw()
		switch kind {
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			token := tokenizer.Token()
			switch token.Data {
			case "html":
				out.WriteString(rewriteTag(token, map[string]string{"lang": boot.Locale, "data-theme": boot.Theme}))
				continue
			case "body":
				if !bootDone {
					out.WriteString(script)
					bootDone = true
				}
				out.WriteString(rewriteTag(token, map[string]string{"data-locale": boot.Locale}))
				continue
			case "title":
				if !titleDone {
					inTitle = true
				}
			}
		case xhtml.TextToken:
			if inTitle {
				continue
			}
		case xhtml.EndTagToken:
			name, _ := tokenizer.TagName()
			switch string(name) {
			case "title":
				if inTitle {
					out.WriteString(html.EscapeString(title))
					inTitle, titleDone = false, true
				}
			case "head":
				if !bootDone {
					out.WriteString(script)
					bootDone = true
				}
			}
		}
		out.Write(raw)
	}
	return out.Bytes(), nil
}

// rewriteTag renders a start tag with attributes replaced or added, escaped.
func rewriteTag(token xhtml.Token, values map[string]string) string {
	var out strings.Builder
	out.WriteString("<" + token.Data)
	seen := map[string]bool{}
	for _, attribute := range token.Attr {
		value := attribute.Val
		if replacement, ok := values[attribute.Key]; ok {
			value = replacement
			seen[attribute.Key] = true
		}
		out.WriteString(" " + attribute.Key + `="` + html.EscapeString(value) + `"`)
	}
	for _, key := range sortedKeys(values) {
		if !seen[key] {
			out.WriteString(" " + key + `="` + html.EscapeString(values[key]) + `"`)
		}
	}
	out.WriteString(">")
	return out.String()
}

func (s *Server) handleI18n(w http.ResponseWriter, r *http.Request) {
	locale := localeOf(r)
	messages := s.catalogs.messages[locale]
	if messages == nil {
		messages = map[string]string{}
	}
	languages := map[string]string{}
	for code, language := range s.languages {
		languages[code] = s.tr(locale, language.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"locale": locale, "messages": messages, "languages": languages})
}

// handleStatic serves other files of the built UI. Hashed files under
// /assets/ may be cached for a year; everything else is revalidated.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.jsonError(w, r, http.StatusNotFound, "Not found")
		return
	}
	if s.frontend == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "index.html" || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	file, err := s.frontend.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if seeker, ok := file.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, info.ModTime(), seeker)
		return
	}
	data, err := io.ReadAll(file)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(data))
}
