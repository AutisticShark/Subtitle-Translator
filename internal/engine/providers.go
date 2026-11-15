package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Provider translates one batch of texts. It receives the source language
// name as configured (e.g. "English") and the target language code (a key of
// Languages). It must return exactly one translation per input, in order, and
// report failures as *TranslationError, *FatalTranslationError or
// *RateLimitError so the driver can apply its retry policy.
type Provider func(ctx context.Context, texts []string, source, target string) ([]string, error)

const (
	anthropicEndpoint = "https://api.anthropic.com/v1/messages"
	openAIDefaultBase = "https://api.openai.com/v1"
	deepLFreeHost     = "api-free.deepl.com"
	deepLProHost      = "api.deepl.com"
	googleEndpoint    = "https://translation.googleapis.com/language/translate/v2"
	// googleBatchLimit is the most strings Cloud Translation - Basic v2
	// accepts in one request.
	googleBatchLimit = 128
	// deepLBatchLimit is the most texts DeepL's /v2/translate accepts in one
	// request; larger batches are split by the adapter.
	deepLBatchLimit = 50

	defaultRequestTimeout = 120 * time.Second
	// maxResponseBytes bounds how much of a provider reply is read.
	maxResponseBytes = 64 << 20
)

// poster sends JSON requests and classifies every failure for the retry
// policy.
type poster struct {
	client   *http.Client
	timeout  time.Duration
	throttle *Throttle
}

// httpClient is the configured client (http.DefaultClient by default) with
// redirects disabled. Following one would re-send the subtitle text and, on a
// 307/308, credentials Go does not strip across hosts (x-api-key); Python's
// urllib refused POST 307/308 too. A 3xx is then a retryable HTTP error.
func (p *poster) httpClient() *http.Client {
	base := p.client
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (p *poster) requestTimeout() time.Duration {
	if p.timeout > 0 {
		return p.timeout
	}
	return defaultRequestTimeout
}

// httpError classifies an HTTP error response as retryable, rate limited or
// fatal.
func httpError(status int, header http.Header, body []byte) error {
	text := truncateRunes(strings.ToValidUTF8(string(body), "�"), 800)

	// 429 = rate limited, 529 = provider overloaded. Both mean "come back
	// later", and the response usually says how much later: use that rather
	// than guessing at a backoff curve.
	if status == 429 || status == 529 {
		after := ParseRetryAfter(header.Get("retry-after"))
		if after == nil {
			for _, name := range []string{
				"anthropic-ratelimit-input-tokens-reset",
				"anthropic-ratelimit-requests-reset",
				"x-ratelimit-reset-requests",
			} {
				if after = ParseRateLimitReset(header.Get(name)); after != nil {
					break
				}
			}
		}
		return rateLimitf(after, "HTTP %d: %s", status, truncateRunes(text, 200))
	}

	// 401/403 = bad or unauthorised key, 400 = malformed request, 404 = wrong
	// model id. Retrying any of these just wastes time and, on a 765-cue
	// file, a lot of it.
	switch status {
	case 400, 401, 403, 404:
		return fatalf("HTTP %d: %s", status, text)
	}
	return transientf("HTTP %d: %s", status, text)
}

// networkError describes a transport failure without ever echoing the request
// URL, which for Google carries the API key.
func networkError(err error, timeout time.Duration) *TranslationError {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &TranslationError{Msg: "network error: request timed out after " + timeout.String()}
	case errors.As(err, &netErr) && netErr.Timeout():
		return &TranslationError{Msg: "network error: timed out: " + err.Error(), Err: err}
	}
	return &TranslationError{Msg: "network error: " + err.Error(), Err: err}
}

// postJSON sends payload as JSON and returns the decoded JSON object.
func (p *poster) postJSON(ctx context.Context, endpoint string, headers map[string]string, payload any) (map[string]any, error) {
	if p.throttle != nil {
		if err := p.throttle.Wait(ctx); err != nil {
			return nil, err
		}
	}

	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, fatalf("could not encode the request: %v", err)
	}

	timeout := p.requestTimeout()
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, &data)
	if err != nil {
		// Not a url.Error here, but never echo the URL: it may hold a key.
		return nil, fatalf("invalid provider endpoint")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := p.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, canceled(ctx)
		}
		return nil, networkError(err, timeout)
	}
	defer resp.Body.Close()
	// Read errors cover timeouts while reading the body, dropped connections
	// and truncated bodies; they are transient too.
	limit := int64(maxResponseBytes)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limit = 8 << 10
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, canceled(ctx)
		}
		return nil, networkError(err, timeout)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpError(resp.StatusCode, resp.Header, body)
	}
	if int64(len(body)) > limit {
		return nil, transientf("provider response is too large")
	}

	// Proxies and gateways sometimes answer 200 with an HTML error page.
	var result any
	if !utf8.Valid(body) || json.Unmarshal(body, &result) != nil {
		return nil, transientf("provider returned a response that is not JSON")
	}
	object, ok := result.(map[string]any)
	if !ok {
		return nil, transientf("provider returned an unexpected JSON response")
	}
	return object, nil
}

// systemPrompt is the instruction sent to the LLM providers.
const systemPromptTemplate = "You are a professional subtitle translator. Translate each " +
	"numbered line from {src} into {tgt}.\n" +
	"\n" +
	"{style}\n" +
	"\n" +
	"Rules, all mandatory:\n" +
	"1. Output EXACTLY one line per input number, in the form `<number>\t<translation>`,\n" +
	"   tab-separated. Same count, same numbers, same order. No preamble, no commentary,\n" +
	"   no blank lines, no markdown.\n" +
	"2. Never merge, split, drop or reorder lines. A line that is a sentence fragment\n" +
	"   stays a fragment — later lines continue it.\n" +
	"3. Copy any ⟦N⟧ sentinel through verbatim and in the same relative position.\n" +
	"   They are markup placeholders, not text.\n" +
	"4. Keep it tight. Subtitles are read in about two seconds; prefer the shorter\n" +
	"   natural phrasing over the literal one.\n" +
	"5. Preserve register, profanity strength, humour and wordplay. Localise idioms\n" +
	"   rather than translating them word for word. If a pun cannot survive, write a\n" +
	"   line that lands the same joke.\n" +
	"6. Do not add honorifics, names or explanations that are not in the source.\n" +
	"7. Leave lines that are purely numbers, timecodes or sound-effect symbols unchanged.\n" +
	"8. Never use full-width Latin letters or digits."

// SystemPrompt renders the LLM system prompt for a source language name and a
// target language code.
func SystemPrompt(source, target string) string {
	language := Languages[target]
	return strings.NewReplacer(
		"{src}", source, "{tgt}", language.Name, "{style}", language.Style,
	).Replace(systemPromptTemplate)
}

func numberedPrompt(texts []string) string {
	var out strings.Builder
	for i, text := range texts {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(strconv.Itoa(i + 1))
		out.WriteByte('\t')
		out.WriteString(text)
	}
	return out.String()
}

func newAnthropic(p *poster, endpoint, model, apiKey string) Provider {
	return func(ctx context.Context, texts []string, source, target string) ([]string, error) {
		resp, err := p.postJSON(ctx, endpoint,
			map[string]string{
				"content-type":      "application/json",
				"x-api-key":         apiKey,
				"anthropic-version": "2023-06-01",
			},
			map[string]any{
				"model":      model,
				"max_tokens": 8000,
				"system":     SystemPrompt(source, target),
				"messages": []map[string]string{
					{"role": "user", "content": numberedPrompt(texts)},
				},
			})
		if err != nil {
			return nil, err
		}
		// `content` can be null, and a text block's `text` too; both must
		// become a retryable error rather than a crash.
		content, ok := resp["content"].([]any)
		if !ok {
			return nil, transientf("Anthropic API returned an unexpected response")
		}
		var text strings.Builder
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				return nil, transientf("Anthropic API returned an unexpected response")
			}
			if block["type"] != "text" {
				continue
			}
			part, ok := block["text"].(string)
			if !ok {
				return nil, transientf("Anthropic API returned an unexpected response")
			}
			text.WriteString(part)
		}
		return ParseNumbered(text.String(), len(texts))
	}
}

func newOpenAI(p *poster, model, apiKey, baseURL string) Provider {
	if baseURL == "" {
		baseURL = openAIDefaultBase
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/chat/completions"
	return func(ctx context.Context, texts []string, source, target string) ([]string, error) {
		resp, err := p.postJSON(ctx, endpoint,
			map[string]string{
				"content-type":  "application/json",
				"authorization": "Bearer " + apiKey,
			},
			map[string]any{
				"model": model,
				"messages": []map[string]string{
					{"role": "system", "content": SystemPrompt(source, target)},
					{"role": "user", "content": numberedPrompt(texts)},
				},
				"temperature": 0.2,
			})
		if err != nil {
			return nil, err
		}
		content, shapeOK := openAIContent(resp)
		if !shapeOK {
			return nil, transientf("OpenAI-compatible API returned an unexpected response")
		}
		// A refusal has `content: null`; some servers return a list of parts.
		text, ok := content.(string)
		if !ok {
			return nil, transientf("OpenAI-compatible API returned no text content")
		}
		return ParseNumbered(text, len(texts))
	}
}

// openAIContent digs choices[0].message.content out of a reply.
func openAIContent(resp map[string]any) (any, bool) {
	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, false
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil, false
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return nil, false
	}
	content, present := message["content"]
	return content, present
}

// stringList validates a list of {key: string} objects as a provider reply.
func stringList(value any, key string, want int) ([]string, bool) {
	items, ok := value.([]any)
	if !ok || len(items) != want {
		return nil, false
	}
	out := make([]string, len(items))
	for i, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		text, ok := object[key].(string)
		if !ok {
			return nil, false
		}
		out[i] = text
	}
	return out, true
}

func newDeepL(p *poster, apiKey string, endpointOverride string) Provider {
	endpoint := endpointOverride
	if endpoint == "" {
		host := deepLProHost
		if strings.HasSuffix(apiKey, ":fx") {
			host = deepLFreeHost
		}
		endpoint = "https://" + host + "/v2/translate"
	}
	return func(ctx context.Context, texts []string, _source, target string) ([]string, error) {
		// DeepL accepts at most 50 texts per request, while a batch may hold
		// up to 100 segments. Send consecutive slices and join the replies in
		// order; any failing slice fails the whole batch for the retry policy.
		out := make([]string, 0, len(texts))
		for start := 0; start < len(texts); start += deepLBatchLimit {
			chunk := texts[start:min(start+deepLBatchLimit, len(texts))]
			resp, err := p.postJSON(ctx, endpoint,
				map[string]string{
					"content-type":  "application/json",
					"authorization": "DeepL-Auth-Key " + apiKey,
				},
				map[string]any{
					"text":                chunk,
					"target_lang":         Languages[target].DeepL,
					"preserve_formatting": true,
				})
			if err != nil {
				return nil, err
			}
			part, ok := stringList(resp["translations"], "text", len(chunk))
			if !ok {
				return nil, transientf("DeepL API returned an unexpected response")
			}
			out = append(out, part...)
		}
		return out, nil
	}
}

var googleLanguageCodeRE = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)

// googleSourceCode maps a source language label to a Google source code: a
// recognised language name or a BCP-47 code. Anything else returns "" so
// Google can auto-detect.
func googleSourceCode(source string) string {
	source = pyStrip(source)
	folded := strings.ToLower(source)
	for code, language := range Languages {
		if strings.ToLower(language.Name) == folded {
			return code
		}
	}
	if folded == "english" {
		return "en"
	}
	if googleLanguageCodeRE.MatchString(source) {
		return source
	}
	return ""
}

func newGoogle(p *poster, apiKey, endpointOverride string) Provider {
	endpoint := endpointOverride
	if endpoint == "" {
		endpoint = googleEndpoint
	}
	endpoint += "?" + url.Values{"key": {apiKey}}.Encode()
	return func(ctx context.Context, texts []string, source, target string) ([]string, error) {
		if len(texts) > googleBatchLimit {
			return nil, fatalf("Google Cloud Translation accepts at most 128 strings per request")
		}
		payload := map[string]any{"q": texts, "target": target, "format": "text"}
		if code := googleSourceCode(source); code != "" {
			payload["source"] = code
		}
		resp, err := p.postJSON(ctx, endpoint,
			map[string]string{"content-type": "application/json; charset=utf-8"}, payload)
		if err != nil {
			return nil, err
		}
		data, _ := resp["data"].(map[string]any)
		out, ok := stringList(data["translations"], "translatedText", len(texts))
		if !ok {
			return nil, transientf("Google Cloud Translation API returned an unexpected response")
		}
		// Google returns HTML-escaped text.
		for i := range out {
			out[i] = html.UnescapeString(out[i])
		}
		return out, nil
	}
}

// NewEcho is the offline provider for exercising the pipeline without an API
// key. The web application only exposes it in debug mode.
func NewEcho() Provider {
	return func(_ context.Context, texts []string, _source, target string) ([]string, error) {
		out := make([]string, len(texts))
		for i, text := range texts {
			out[i] = "[" + target + "] " + text
		}
		return out, nil
	}
}
