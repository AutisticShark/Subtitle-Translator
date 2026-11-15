package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// capture records the requests a test server received.
type capture struct {
	mu       sync.Mutex
	requests []capturedRequest
}

type capturedRequest struct {
	method, path, rawQuery string
	header                 http.Header
	body                   map[string]any
	rawBody                []byte
}

func (c *capture) last(t *testing.T) capturedRequest {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		t.Fatal("no request received")
	}
	return c.requests[len(c.requests)-1]
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// newServer starts a test server that records requests and answers with
// handler. The recorded body is decoded as a JSON object when possible.
func newServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := capturedRequest{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, header: r.Header.Clone(), rawBody: raw}
		_ = json.Unmarshal(raw, &req.body)
		cap.mu.Lock()
		cap.requests = append(cap.requests, req)
		cap.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func jsonHandler(status int, body string, headers ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func testPoster() *poster { return &poster{timeout: 5 * time.Second} }

func call(t *testing.T, p Provider, texts ...string) ([]string, error) {
	t.Helper()
	return p(context.Background(), texts, "English", "es")
}

func assertRetryable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var transient *TranslationError
	if !errors.As(err, &transient) {
		t.Fatalf("not a retryable TranslationError: %T %v", err, err)
	}
	if IsFatal(err) {
		t.Fatalf("fatal: %v", err)
	}
	if _, limited := asRateLimit(err); limited {
		t.Fatalf("rate limit: %v", err)
	}
}

func TestAnthropicBuildsTheNumberedRequestAndParsesTheReply(t *testing.T) {
	srv, cap := newServer(t, jsonHandler(200, `{"content":[{"type":"text","text":"1\tHola\n2\tAdiós"}]}`))
	provider := newAnthropic(testPoster(), srv.URL+"/v1/messages", "claude-test", "secret")

	out, err := call(t, provider, "Hello", "Goodbye")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"Hola", "Adiós"})

	req := cap.last(t)
	equal(t, req.method, "POST")
	equal(t, req.path, "/v1/messages")
	equal(t, req.header.Get("x-api-key"), "secret")
	equal(t, req.header.Get("anthropic-version"), "2023-06-01")
	equal(t, req.header.Get("content-type"), "application/json")
	equal(t, req.body["model"], "claude-test")
	equal(t, req.body["max_tokens"], float64(8000))
	messages := req.body["messages"].([]any)
	equal(t, len(messages), 1)
	message := messages[0].(map[string]any)
	equal(t, message["role"], "user")
	equal(t, message["content"], "1\tHello\n2\tGoodbye")
	equal(t, req.body["system"], SystemPrompt("English", "es"))
	// Markup characters are not HTML-escaped in the body.
	_, _ = call(t, provider, "<i>a</i> & b")
	contains(t, string(cap.last(t).rawBody), "<i>a</i> & b")
}

func TestAnthropicJoinsTextBlocksAndSkipsOthers(t *testing.T) {
	srv, _ := newServer(t, jsonHandler(200, `{"content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"1\tHo"},{"type":"text","text":"la"}]}`))
	out, err := call(t, newAnthropic(testPoster(), srv.URL, "m", "k"), "Hello")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"Hola"})
}

func TestOpenAIBuildsTheRequestAndNormalisesTheBaseURL(t *testing.T) {
	srv, cap := newServer(t, jsonHandler(200, `{"choices":[{"message":{"content":"1\tHallo\n2\tTschüss"}}]}`))
	provider := newOpenAI(testPoster(), "gpt-test", "secret", srv.URL+"/v1/")

	out, err := provider(context.Background(), []string{"Hello", "Bye"}, "English", "de")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"Hallo", "Tschüss"})

	req := cap.last(t)
	equal(t, req.path, "/v1/chat/completions")
	equal(t, req.header.Get("authorization"), "Bearer secret")
	equal(t, req.body["model"], "gpt-test")
	equal(t, req.body["temperature"], 0.2)
	messages := req.body["messages"].([]any)
	equal(t, len(messages), 2)
	system := messages[0].(map[string]any)
	equal(t, system["role"], "system")
	equal(t, system["content"], SystemPrompt("English", "de"))
	user := messages[1].(map[string]any)
	equal(t, user["role"], "user")
	equal(t, user["content"], "1\tHello\n2\tBye")
}

func TestOpenAIDefaultBaseURL(t *testing.T) {
	var seen string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.URL.String()
		return cannedResponse(200, `{"choices":[{"message":{"content":"1\tx"}}]}`), nil
	})
	provider, err := NewProvider(Config{Provider: "openai", APIKey: "k", Model: "m", HTTPClient: &http.Client{Transport: transport}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, provider, "a"); err != nil {
		t.Fatal(err)
	}
	equal(t, seen, "https://api.openai.com/v1/chat/completions")
}

func TestOpenAIRejectsAMalformedReply(t *testing.T) {
	srv, _ := newServer(t, jsonHandler(200, `{"choices":[]}`))
	_, err := call(t, newOpenAI(testPoster(), "m", "k", srv.URL), "Hello")
	assertRetryable(t, err)
	contains(t, err.Error(), "unexpected response")
}

func TestMalformedLLMRepliesAreRetryable(t *testing.T) {
	anthropic := []string{
		`{"content":null}`, `{"content":"1\tHola"}`, `{"content":["x"]}`,
		`{"content":[{"type":"text","text":null}]}`, `{"content":[{"type":"text","text":["1\tHola"]}]}`,
		`{"content":[{"type":"text"}]}`, `{}`, `{"content":[]}`, `{"content":[{"type":"text","text":"no numbers here"}]}`,
	}
	for _, body := range anthropic {
		srv, _ := newServer(t, jsonHandler(200, body))
		_, err := call(t, newAnthropic(testPoster(), srv.URL, "m", "k"), "Hello")
		assertRetryable(t, err)
	}
	openai := []string{
		`{"choices":[{"message":{"content":null,"refusal":"No."}}]}`,
		`{"choices":[{"message":{"content":[{"type":"text","text":"1\tHola"}]}}]}`,
		`{"choices":[{"message":{"content":7}}]}`,
		`{"choices":[{"message":{}}]}`, `{"choices":[{}]}`, `{"choices":[7]}`, `{"choices":"x"}`, `{}`,
		`{"choices":[{"message":{"content":"garbage"}}]}`,
	}
	for _, body := range openai {
		srv, _ := newServer(t, jsonHandler(200, body))
		_, err := call(t, newOpenAI(testPoster(), "m", "k", srv.URL), "Hello")
		assertRetryable(t, err)
	}
}

func TestRefusalFallsBackToSourceInsteadOfFailingTheJob(t *testing.T) {
	srv, _ := newServer(t, jsonHandler(200, `{"choices":[{"message":{"content":null}}]}`))
	var reported []int
	opt := testOptions()
	opt.Fallback = func(n int) { reported = append(reported, n) }

	out, err := runTranslation(t, plainSegments("Hello"), newOpenAI(testPoster(), "m", "k", srv.URL), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"Hello"})
	equal(t, reported, []int{1})
}

func TestDeepLSelectsTheEndpointAndTargetCode(t *testing.T) {
	var urls []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		urls = append(urls, r.URL.String()+"|"+r.Header.Get("authorization"))
		return cannedResponse(200, `{"translations":[{"text":"Hallo"}]}`), nil
	})
	client := &http.Client{Transport: transport}
	for _, key := range []string{"secret:fx", "secret"} {
		provider, err := NewProvider(Config{Provider: "deepl", APIKey: key, HTTPClient: client}, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := provider(context.Background(), []string{"Hello"}, "English", "de")
		if err != nil {
			t.Fatal(err)
		}
		equal(t, out, []string{"Hallo"})
	}
	equal(t, urls, []string{
		"https://api-free.deepl.com/v2/translate|DeepL-Auth-Key secret:fx",
		"https://api.deepl.com/v2/translate|DeepL-Auth-Key secret",
	})
}

func TestDeepLRequestBody(t *testing.T) {
	srv, cap := newServer(t, jsonHandler(200, `{"translations":[{"text":"你好"},{"text":"再見"}]}`))
	provider := newDeepL(testPoster(), "k", srv.URL)

	for code, want := range map[string]string{"zh-TW": "ZH-HANT", "zh-CN": "ZH-HANS", "pt-BR": "PT-BR", "ja": "JA"} {
		if _, err := provider(context.Background(), []string{"Hello", "Bye"}, "English", code); err != nil {
			t.Fatal(err)
		}
		req := cap.last(t)
		equal(t, req.body["target_lang"], want)
		equal(t, req.body["preserve_formatting"], true)
		equal(t, req.body["text"], []any{"Hello", "Bye"})
	}
}

// DeepL takes at most 50 texts per request; a 100-segment batch (the UI
// maximum) must be split into ordered slices whose replies keep count and order.
func TestDeepLSplitsBatchesAboveFiftyTexts(t *testing.T) {
	var mu sync.Mutex // the handler runs on the server's goroutines
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Text []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sizes = append(sizes, len(body.Text))
		mu.Unlock()
		var items []string
		for _, text := range body.Text {
			items = append(items, fmt.Sprintf(`{"text":"de:%s"}`, text))
		}
		_, _ = io.WriteString(w, `{"translations":[`+strings.Join(items, ",")+`]}`)
	}))
	t.Cleanup(srv.Close)
	provider := newDeepL(testPoster(), "k", srv.URL)
	for _, n := range []int{1, 50, 51, 100, 128} {
		mu.Lock()
		sizes = nil
		mu.Unlock()
		texts := make([]string, n)
		want := make([]string, n)
		for i := range texts {
			texts[i] = fmt.Sprintf("line %d", i)
			want[i] = "de:" + texts[i]
		}
		out, err := provider(context.Background(), texts, "English", "de")
		if err != nil {
			t.Fatalf("%d texts: %v", n, err)
		}
		equal(t, out, want)
		mu.Lock()
		got := append([]int(nil), sizes...)
		mu.Unlock()
		total := 0
		for _, size := range got {
			if size < 1 || size > 50 {
				t.Fatalf("%d texts: request of %d", n, size)
			}
			total += size
		}
		equal(t, total, n)
		equal(t, len(got), (n+49)/50)
	}

	// A malformed reply to any slice fails the whole batch as retryable.
	var calls atomic.Int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			_, _ = io.WriteString(w, `{"translations":[{"text":"only one"}]}`)
			return
		}
		var body struct{ Text []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"translations":[`+strings.Repeat(`{"text":"x"},`, len(body.Text)-1)+`{"text":"x"}]}`)
	}))
	t.Cleanup(srv2.Close)
	texts := make([]string, 100)
	for i := range texts {
		texts[i] = "t"
	}
	_, err := newDeepL(testPoster(), "k", srv2.URL)(context.Background(), texts, "English", "de")
	assertRetryable(t, err)
}

func TestDeepLMalformedResponsesAreRetryable(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"translations":[{"text":"Eins"}]}`, `{"translations":[{"text":"Eins"},{"text":null}]}`,
		`{"translations":"nope"}`, `{"translations":[1,2]}`, `{"translations":[{"text":"a"},{}]}`,
	} {
		srv, _ := newServer(t, jsonHandler(200, body))
		_, err := call(t, newDeepL(testPoster(), "k", srv.URL), "One", "Two")
		assertRetryable(t, err)
		contains(t, err.Error(), "DeepL API returned an unexpected response")
	}
}

func TestGoogleTranslatesABatchAndPreservesOrder(t *testing.T) {
	srv, cap := newServer(t, jsonHandler(200, `{"data":{"translations":[{"translatedText":"你好 &amp; 再見"},{"translatedText":"第二行"}]}}`))
	provider := newGoogle(testPoster(), "key with /?", srv.URL)

	out, err := provider(context.Background(), []string{"Hello & goodbye", "Second line"}, "English", "zh-TW")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"你好 & 再見", "第二行"})

	req := cap.last(t)
	equal(t, req.rawQuery, "key=key+with+%2F%3F")
	equal(t, req.header.Get("content-type"), "application/json; charset=utf-8")
	equal(t, req.body, map[string]any{
		"q": []any{"Hello & goodbye", "Second line"}, "target": "zh-TW", "format": "text", "source": "en",
	})
}

func TestGoogleSourceLanguageMapping(t *testing.T) {
	for source, want := range map[string]string{
		"English":                      "en",
		"english":                      "en",
		" ENGLISH ":                    "en",
		"Japanese":                     "ja",
		"Traditional Chinese (Taiwan)": "zh-TW",
		"Brazilian Portuguese":         "pt-BR",
		"en":                           "en",
		"zh-Hant-TW":                   "zh-Hant-TW",
		"fil":                          "fil",
		"Detect automatically":         "",
		"":                             "",
		"x":                            "",
		"en_US":                        "",
		"1234":                         "",
	} {
		if got := googleSourceCode(source); got != want {
			t.Errorf("googleSourceCode(%q) = %q, want %q", source, got, want)
		}
	}
	srv, cap := newServer(t, jsonHandler(200, `{"data":{"translations":[{"translatedText":"Hola"}]}}`))
	provider := newGoogle(testPoster(), "key", srv.URL)
	if _, err := provider(context.Background(), []string{"Hello"}, "Detect automatically", "es"); err != nil {
		t.Fatal(err)
	}
	if _, present := cap.last(t).body["source"]; present {
		t.Fatal("an unrecognised source must be left to auto-detection")
	}
}

func TestGoogleRejectsWrongCountsAndShapes(t *testing.T) {
	for _, body := range []string{
		`{"data":{"translations":[{"translatedText":"Only one"}]}}`,
		`{"data":{"translations":"x"}}`, `{"data":"x"}`, `{}`,
		`{"data":{"translations":[{"translatedText":"a"},{"translatedText":7}]}}`,
		`{"data":{"translations":[{"translatedText":"a"},{}]}}`,
	} {
		srv, _ := newServer(t, jsonHandler(200, body))
		_, err := call(t, newGoogle(testPoster(), "key", srv.URL), "One", "Two")
		assertRetryable(t, err)
		contains(t, err.Error(), "unexpected response")
	}
}

func TestGoogleRejectsMoreThanTheBatchLimit(t *testing.T) {
	srv, cap := newServer(t, jsonHandler(200, `{}`))
	_, err := call(t, newGoogle(testPoster(), "key", srv.URL), make([]string, 129)...)
	if !IsFatal(err) {
		t.Fatalf("got %v", err)
	}
	contains(t, err.Error(), "at most 128")
	equal(t, cap.count(), 0)

	texts := make([]string, 128)
	srv2, _ := newServer(t, jsonHandler(200, `{"data":{"translations":[`+strings.TrimSuffix(strings.Repeat(`{"translatedText":"x"},`, 128), ",")+`]}}`))
	if _, err = call(t, newGoogle(testPoster(), "key", srv2.URL), texts...); err != nil {
		t.Fatalf("128 strings must be accepted: %v", err)
	}
}

func TestEveryProviderUsesItsRealEndpoint(t *testing.T) {
	var seen []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.URL.String())
		switch {
		case strings.Contains(r.URL.Host, "anthropic"):
			return cannedResponse(200, `{"content":[{"type":"text","text":"1\tx"}]}`), nil
		case strings.Contains(r.URL.Host, "googleapis"):
			return cannedResponse(200, `{"data":{"translations":[{"translatedText":"x"}]}}`), nil
		case strings.Contains(r.URL.Host, "deepl"):
			return cannedResponse(200, `{"translations":[{"text":"x"}]}`), nil
		}
		return cannedResponse(200, `{"choices":[{"message":{"content":"1\tx"}}]}`), nil
	})}
	for _, cfg := range []Config{
		{Provider: "anthropic", APIKey: "k", Model: "m"},
		{Provider: "openai", APIKey: "k", Model: "m", BaseURL: "https://llm.example/v1///"},
		{Provider: "deepl", APIKey: "k"},
		{Provider: "google", APIKey: "a b"},
	} {
		cfg.HTTPClient = client
		provider, err := NewProvider(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = call(t, provider, "a"); err != nil {
			t.Fatalf("%s: %v", cfg.Provider, err)
		}
	}
	equal(t, seen, []string{
		"https://api.anthropic.com/v1/messages",
		"https://llm.example/v1/chat/completions",
		"https://api.deepl.com/v2/translate",
		"https://translation.googleapis.com/language/translate/v2?key=a+b",
	})
}

func TestNewProviderValidation(t *testing.T) {
	for _, cfg := range []Config{
		{Provider: "anthropic", Model: "m"}, {Provider: "anthropic", APIKey: "k"},
		{Provider: "openai", Model: "m"}, {Provider: "openai", APIKey: "k"},
		{Provider: "deepl"}, {Provider: "google"}, {Provider: "nope"}, {},
	} {
		_, err := NewProvider(cfg, nil)
		if !IsFatal(err) {
			t.Errorf("%+v: got %v", cfg, err)
		}
	}
	_, err := NewProvider(Config{Provider: "nope"}, nil)
	contains(t, err.Error(), "Unknown provider: nope")
	if _, err = NewProvider(Config{Provider: "echo"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = NewProvider(Config{Provider: " Anthropic ", APIKey: "k", Model: "m"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestEchoPrefixesTheTarget(t *testing.T) {
	out, err := NewEcho()(context.Background(), []string{"a", "⟦0⟧b"}, "English", "zh-TW")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"[zh-TW] a", "[zh-TW] ⟦0⟧b"})
}

func TestSystemPrompt(t *testing.T) {
	prompt := SystemPrompt("English", "zh-TW")
	for _, want := range []string{
		"Translate each numbered line from English into Traditional Chinese (Taiwan).",
		"Use Taiwan Mandarin vocabulary and idiom",
		"in the form `<number>\t<translation>`,",
		"3. Copy any ⟦N⟧ sentinel through verbatim",
		"8. Never use full-width Latin letters or digits.",
		"Rules, all mandatory:",
		"2. Never merge, split, drop or reorder lines.",
	} {
		contains(t, prompt, want)
	}
	notContains(t, prompt, "{src}")
	notContains(t, prompt, "{tgt}")
	notContains(t, prompt, "{style}")
	// The Python template glues the first two lines with a line continuation.
	contains(t, prompt, "Translate each numbered line")
	// A language without style guidance leaves an empty paragraph.
	contains(t, SystemPrompt("French", "de"), "from French into German.\n\n\n\nRules, all mandatory:")
	equal(t, strings.Count(prompt, "\n"), 19)
}

// ---------------------------------------------------------------------------
// HTTP behaviour: error classification, malformed bodies, timeouts

func postTo(t *testing.T, p *poster, srv *httptest.Server) (map[string]any, error) {
	t.Helper()
	return p.postJSON(context.Background(), srv.URL, map[string]string{"content-type": "application/json"}, map[string]any{"q": "x"})
}

func TestPostJSONSuccess(t *testing.T) {
	srv, _ := newServer(t, jsonHandler(200, `{"ok": true}`))
	got, err := postTo(t, testPoster(), srv)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, map[string]any{"ok": true})
}

func TestHTTPStatusClassification(t *testing.T) {
	for status, kind := range map[int]string{
		400: "fatal", 401: "fatal", 403: "fatal", 404: "fatal",
		402: "transient", 408: "transient", 409: "transient", 500: "transient", 502: "transient", 503: "transient", 504: "transient",
		429: "limit", 529: "limit",
		301: "transient", 100 + 99: "transient",
	} {
		srv, _ := newServer(t, jsonHandler(status, `{"error":"x"}`))
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		_, err := (&poster{client: client, timeout: 5 * time.Second}).postJSON(context.Background(), srv.URL, nil, map[string]any{})
		var limited *RateLimitError
		switch kind {
		case "fatal":
			if !IsFatal(err) {
				t.Errorf("%d: got %v", status, err)
			}
		case "limit":
			if !errors.As(err, &limited) {
				t.Errorf("%d: got %v", status, err)
			}
		default:
			if status == 199 {
				continue // 1xx cannot be produced by a handler
			}
			func() {
				defer func() { _ = recover() }()
				assertRetryable(t, err)
			}()
		}
	}
}

func TestHTTPErrorsCarryTheStatusAndABoundedBody(t *testing.T) {
	long := strings.Repeat("é", 2000)
	srv, _ := newServer(t, jsonHandler(400, long))
	_, err := postTo(t, testPoster(), srv)
	if !IsFatal(err) {
		t.Fatalf("got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "HTTP 400: é") {
		t.Fatal(err.Error())
	}
	equal(t, len([]rune(strings.TrimPrefix(err.Error(), "HTTP 400: "))), 800)

	srv, _ = newServer(t, jsonHandler(429, long, "retry-after", "7"))
	_, err = postTo(t, testPoster(), srv)
	var limited *RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("got %v", err)
	}
	equal(t, len([]rune(strings.TrimPrefix(err.Error(), "HTTP 429: "))), 200)
	equal(t, limited.RetryAfter, f64(7))

	srv, _ = newServer(t, jsonHandler(500, "bad \xff bytes"))
	_, err = postTo(t, testPoster(), srv)
	assertRetryable(t, err)
	if !strings.Contains(err.Error(), "HTTP 500: bad") {
		t.Fatal(err)
	}
}

func TestRateLimitHeaders(t *testing.T) {
	cases := []struct {
		name    string
		headers []string
		want    *float64
	}{
		{"retry-after seconds", []string{"retry-after", "12"}, f64(12)},
		{"retry-after wins", []string{"retry-after", "3", "x-ratelimit-reset-requests", "6m0s"}, f64(3)},
		{"go duration", []string{"x-ratelimit-reset-requests", "6m0s"}, f64(360)},
		{"anthropic requests reset", []string{"anthropic-ratelimit-requests-reset", time.Now().UTC().Add(45 * time.Second).Format("2006-01-02T15:04:05Z")}, nil},
		{"anthropic token reset first", []string{
			"anthropic-ratelimit-input-tokens-reset", "2020-01-01T00:00:00Z",
			"anthropic-ratelimit-requests-reset", "2099-01-01T00:00:00Z",
		}, f64(0)},
		{"garbage retry-after falls through", []string{"retry-after", "soon", "x-ratelimit-reset-requests", "250ms"}, f64(0.25)},
		{"infinite retry-after falls through", []string{"retry-after", "inf", "x-ratelimit-reset-requests", "1s"}, f64(1)},
		{"none", nil, nil},
		{"all garbage", []string{"retry-after", "x", "x-ratelimit-reset-requests", "y"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newServer(t, jsonHandler(529, "overloaded", tc.headers...))
			_, err := postTo(t, testPoster(), srv)
			var limited *RateLimitError
			if !errors.As(err, &limited) {
				t.Fatalf("got %v", err)
			}
			if tc.name == "anthropic requests reset" {
				if limited.RetryAfter == nil || *limited.RetryAfter < 40 || *limited.RetryAfter > 46 {
					t.Fatalf("got %v", limited.RetryAfter)
				}
				return
			}
			equal(t, limited.RetryAfter, tc.want)
			contains(t, err.Error(), "HTTP 529: overloaded")
		})
	}
}

func TestMalformedBodiesAreRetryable(t *testing.T) {
	cases := map[string]string{
		"html":          "<html>Bad gateway</html>",
		"empty":         "",
		"list":          "[]",
		"string":        `"ok"`,
		"number":        "7",
		"null":          "null",
		"truncated":     `{"a":`,
		"trailing junk": `{"a":1} x`,
		"invalid utf-8": "{\"a\":\"\xff\"}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := newServer(t, jsonHandler(200, body))
			_, err := postTo(t, testPoster(), srv)
			assertRetryable(t, err)
		})
	}
	srv, _ := newServer(t, jsonHandler(200, "<html>"))
	_, err := postTo(t, testPoster(), srv)
	contains(t, err.Error(), "not JSON")
}

func TestDroppedConnectionIsRetryable(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	_, err := postTo(t, testPoster(), srv)
	assertRetryable(t, err)
	contains(t, err.Error(), "network error")
}

func TestTimeoutWaitingForHeadersIsRetryable(t *testing.T) {
	release := make(chan struct{})
	srv, _ := newServer(t, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)
	start := time.Now()
	_, err := postTo(t, &poster{timeout: 200 * time.Millisecond}, srv)
	assertRetryable(t, err)
	contains(t, err.Error(), "timed out")
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not applied")
	}
}

func TestTimeoutWhileReadingTheBodyIsRetryable(t *testing.T) {
	release := make(chan struct{})
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"partial"`)
		w.(http.Flusher).Flush()
		<-release
	})
	defer close(release)
	_, err := postTo(t, &poster{timeout: 200 * time.Millisecond}, srv)
	assertRetryable(t, err)
}

func TestTruncatedBodyIsRetryable(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"partial"`)
		// The handler returns with the body short of Content-Length.
	})
	_, err := postTo(t, testPoster(), srv)
	assertRetryable(t, err)
}

func TestConnectionRefusedIsRetryableAndNeverEchoesTheURL(t *testing.T) {
	srv, _ := newServer(t, jsonHandler(200, `{}`))
	endpoint := srv.URL + "?key=SECRET-KEY"
	srv.Close()
	_, err := testPoster().postJSON(context.Background(), endpoint, nil, map[string]any{})
	assertRetryable(t, err)
	notContains(t, err.Error(), "SECRET-KEY")
}

func TestInvalidEndpointIsFatalAndNeverEchoesTheURL(t *testing.T) {
	_, err := testPoster().postJSON(context.Background(), "http://bad host/?key=SECRET-KEY", nil, map[string]any{})
	if !IsFatal(err) {
		t.Fatalf("got %v", err)
	}
	notContains(t, err.Error(), "SECRET-KEY")
}

func TestGoogleErrorsNeverLeakTheAPIKey(t *testing.T) {
	srv, _ := newServer(t, jsonHandler(200, `{}`))
	provider := newGoogle(testPoster(), "SECRET-KEY", srv.URL)
	srv.Close()
	_, err := call(t, provider, "a")
	assertRetryable(t, err)
	notContains(t, err.Error(), "SECRET-KEY")
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		t.Fatal("the url.Error (which holds the key) must not be wrapped")
	}
}

// Redirects are never followed: Go would re-POST the subtitles and copy the
// x-api-key header to another host. A 3xx is a retryable HTTP error, as the
// 307/308 HTTPError was in Python.
func TestRedirectsAreNeverFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	other, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"1\tstolen"}]}`)
	})
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1) // a different host
	for _, code := range []int{301, 302, 303, 307, 308} {
		srv, cap := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, otherURL+"/v1/messages", code)
		})
		// A caller-supplied client keeps its own settings but still never redirects.
		p := &poster{client: &http.Client{Timeout: 5 * time.Second}, timeout: 5 * time.Second}
		_, err := newAnthropic(p, srv.URL, "m", "SECRET-KEY")(context.Background(), []string{"Hi"}, "English", "es")
		assertRetryable(t, err)
		contains(t, err.Error(), fmt.Sprintf("HTTP %d", code))
		equal(t, cap.count(), 1)
	}
	equal(t, elsewhere.Load(), int32(0))
}

func TestPostJSONStopsWhenTheContextIsCanceled(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	srv, _ := newServer(t, func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()
	start := time.Now()
	_, err := testPoster().postJSON(ctx, srv.URL, nil, map[string]any{})
	if !IsCanceled(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("request not abandoned")
	}
	if _, retryable := err.(*TranslationError); retryable {
		t.Fatal("cancellation must not look retryable")
	}
}

func TestPostJSONHonoursTheThrottle(t *testing.T) {
	srv, cap := newServer(t, jsonHandler(200, `{}`))
	throttle := NewThrottle(0)
	throttle.Penalise(0.15)
	p := &poster{timeout: 5 * time.Second, throttle: throttle}
	start := time.Now()
	if _, err := postTo(t, p, srv); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 120*time.Millisecond {
		t.Fatal("request ignored the parked throttle")
	}
	equal(t, cap.count(), 1)

	// A canceled wait never sends.
	throttle.Penalise(30)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.postJSON(ctx, srv.URL, nil, map[string]any{})
	if !IsCanceled(err) {
		t.Fatalf("got %v", err)
	}
	equal(t, cap.count(), 1)
}

func TestRequestTimeoutDefault(t *testing.T) {
	equal(t, (&poster{}).requestTimeout(), 120*time.Second)
	equal(t, (&poster{timeout: time.Second}).requestTimeout(), time.Second)
}
