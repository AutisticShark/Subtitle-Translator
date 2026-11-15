package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func echoConfig() Config { return Config{Provider: "echo"} }

func TestLanguageTable(t *testing.T) {
	equal(t, len(Languages), 15)
	equal(t, len(LanguageOrder), 15)
	for _, code := range LanguageOrder {
		language, ok := Languages[code]
		if !ok {
			t.Fatalf("%s missing from Languages", code)
		}
		if language.Suffix == "" || language.Name == "" || language.DeepL == "" {
			t.Errorf("%s incomplete: %+v", code, language)
		}
		if !strings.HasPrefix(language.Suffix, ".") {
			t.Errorf("%s suffix %q", code, language.Suffix)
		}
	}
	equal(t, Languages["zh-TW"], Language{
		Suffix: ".zh.tw", Name: "Traditional Chinese (Taiwan)", DeepL: "ZH-HANT",
		Style: "Use Taiwan Mandarin vocabulary and idiom, not Chinese vocabulary " +
			"mechanically converted to traditional glyphs. Use Taiwan full-width " +
			"punctuation conventions.",
	})
	equal(t, Languages["zh-CN"].Style, "Use Chinese Mandarin vocabulary and standard simplified punctuation.")
	equal(t, Languages["pt-BR"], Language{Suffix: ".pt.br", Name: "Brazilian Portuguese", DeepL: "PT-BR"})
	for code, language := range Languages {
		if code != "zh-TW" && code != "zh-CN" && language.Style != "" {
			t.Errorf("%s has unexpected style", code)
		}
	}
	for code, want := range map[string]bool{"zh-TW": true, "zh-CN": true, "ja": true, "ko": false, "es": false, "": false} {
		equal(t, IsCJKLanguage(code), want)
	}
}

func TestTranslatedFilename(t *testing.T) {
	for _, tc := range []struct{ name, target, want string }{
		{"show.en.ass", "zh-TW", "show.zh.tw.ass"},
		{"SHOW.ENGLISH.VTT", "ja", "SHOW.ja.vtt"},
		{"movie.eng.srt", "pt-BR", "movie.pt.br.srt"},
		{"movie.srt", "de", "movie.de.srt"},
		{"movie.EN.SRT", "fr", "movie.fr.srt"},
		{"a.b.c.srt", "es", "a.b.c.es.srt"},
		{"The.Men.srt", "es", "The.Men.es.srt"},
		{"french.srt", "ko", "french.ko.srt"},
		{"dir/sub/show.en.srt", "it", "show.it.srt"},
		{"noext", "ru", "noext.ru"},
		{".srt", "nl", ".srt.nl"},
		{"x.en.srt", "unknown-XX", "x.unknown.xx.srt"},
		{"字幕.en.srt", "zh-CN", "字幕.zh.cn.srt"},
		{"x.srt.", "pl", "x.srt..pl"},
	} {
		if got := TranslatedFilename(tc.name, tc.target); got != tc.want {
			t.Errorf("TranslatedFilename(%q, %q) = %q, want %q", tc.name, tc.target, got, tc.want)
		}
	}
	equal(t, TranslatedFilenameSuffix("show.en.ass", ".zh.tw"), "show.zh.tw.ass")
}

func TestTranslateSRTWithEcho(t *testing.T) {
	source := "1\r\n00:00:01,000 --> 00:00:03,000\r\n<i>Hello</i> world\r\n\r\n2\r\n00:00:04,000 --> 00:00:05,000\r\n- Hi\r\n- Bye\r\n"
	var progress [][2]int

	out, stats, err := Translate(context.Background(), []byte(source), ".srt", "es", echoConfig(),
		func(done, total int) { progress = append(progress, [2]int{done, total}) })
	if err != nil {
		t.Fatal(err)
	}
	equal(t, string(out), "1\r\n00:00:01,000 --> 00:00:03,000\r\n[es] <i>Hello</i> world\r\n\r\n"+
		"2\r\n00:00:04,000 --> 00:00:05,000\r\n- [es] Hi\r\n- [es] Bye\r\n")
	equal(t, stats, Stats{Cues: 2, Segments: 3, Translated: 3})
	equal(t, progress[0], [2]int{0, 3})
	equal(t, progress[len(progress)-1], [2]int{3, 3})
}

func TestTranslateAcceptsBareAndUppercaseExtensions(t *testing.T) {
	const source = "1\n00:00:01,000 --> 00:00:02,000\nHi\n"
	for _, ext := range []string{".srt", "srt", ".SRT", "movie.en.srt"} {
		out, _, err := Translate(context.Background(), []byte(source), ext, "de", echoConfig(), nil)
		if err != nil {
			t.Fatalf("%q: %v", ext, err)
		}
		contains(t, string(out), "[de] Hi")
	}
}

func TestTranslateKeepsBOMAndConvertsEncodingToUTF8(t *testing.T) {
	out, _, err := Translate(context.Background(), []byte("\xef\xbb\xbf1\n00:00:01,000 --> 00:00:02,000\nHi\n"), "srt", "de", echoConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "\xef\xbb\xbf1\n") {
		t.Fatalf("%q", out)
	}

	cfg := echoConfig()
	cfg.Encoding = "gbk"
	gbk := append([]byte("1\n00:00:01,000 --> 00:00:02,000\n"), 0xc4, 0xe3, 0xba, 0xc3, '\n')
	out, _, err = Translate(context.Background(), gbk, "srt", "ja", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	contains(t, string(out), "[ja] 你好")
}

func TestTranslateVTTAndASS(t *testing.T) {
	vtt := "WEBVTT\n\nid\n00:01.000 --> 00:02.000 align:start\nTom &amp; Jerry\n"
	out, _, err := Translate(context.Background(), []byte(vtt), "vtt", "fr", echoConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, string(out), "WEBVTT\n\nid\n00:01.000 --> 00:02.000 align:start\n[fr] Tom &amp; Jerry\n")

	ass := assHeader + assLine(`{\an8}Hello\Nthere`)
	out, _, err = Translate(context.Background(), []byte(ass), "ass", "fr", echoConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	contains(t, string(out), `,,[fr] {\an8}Hello there`+"\n")
}

func TestTranslateWrapsByWidthAndMaxLines(t *testing.T) {
	const source = "1\n00:00:01,000 --> 00:00:02,000\none two three four five six seven eight\n"
	cfg := echoConfig()
	cfg.Width, cfg.MaxLines = 8, 3 // 16 cells per line

	out, _, err := Translate(context.Background(), []byte(source), "srt", "es", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")[2:]
	if len(lines) < 2 || len(lines) > 3 {
		t.Fatalf("%q", lines)
	}
	equal(t, strings.Join(lines, " "), "[es] one two three four five six seven eight")
}

func TestTranslateAppliesDefaults(t *testing.T) {
	// Zero Width/MaxLines/BatchSize/Workers/SourceLang use the defaults.
	const source = "1\n00:00:01,000 --> 00:00:02,000\nHi\n"
	cfg := Config{Provider: "openai", APIKey: "k", Model: "m", HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return cannedResponse(200, `{"choices":[{"message":{"content":"1\tHola"}}]}`), nil
	})}}
	out, stats, err := Translate(context.Background(), []byte(source), "srt", "es", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, string(out), "1\n00:00:01,000 --> 00:00:02,000\nHola\n")
	equal(t, stats.Translated, 1)
}

func TestTranslateSendsTheSourceLanguageToTheProvider(t *testing.T) {
	var systemPrompts []string
	var mu sync.Mutex
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw := make([]byte, 1<<16)
		n, _ := r.Body.Read(raw)
		mu.Lock()
		systemPrompts = append(systemPrompts, string(raw[:n]))
		mu.Unlock()
		return cannedResponse(200, `{"content":[{"type":"text","text":"1\tHola"}]}`), nil
	})}
	const source = "1\n00:00:01,000 --> 00:00:02,000\nHi\n"
	for _, lang := range []string{"", "Japanese"} {
		cfg := Config{Provider: "anthropic", APIKey: "k", Model: "m", SourceLang: lang, HTTPClient: client}
		if _, _, err := Translate(context.Background(), []byte(source), "srt", "es", cfg, nil); err != nil {
			t.Fatal(err)
		}
	}
	contains(t, systemPrompts[0], "from English into Spanish")
	contains(t, systemPrompts[1], "from Japanese into Spanish")
}

func TestTranslateReportsFallbacksInStatsAndWarning(t *testing.T) {
	const source = "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:03,000 --> 00:00:04,000\n<i>Bye</i>\n"
	cfg := Config{Provider: "openai", APIKey: "k", Model: "m", Retries: 1, RetryDelay: noDelay,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return cannedResponse(200, `{"choices":[{"message":{"content":null}}]}`), nil
		})}}

	out, stats, err := Translate(context.Background(), []byte(source), "srt", "es", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, string(out), source)
	equal(t, stats, Stats{Cues: 2, Segments: 2, Translated: 0, Fallbacks: 2, Warning: "Left untranslated: 2 of 2 segments"})
}

func TestTranslateStatsCountCachedAndTextlessSegments(t *testing.T) {
	const source = "1\n00:00:01,000 --> 00:00:02,000\n\n\n2\n00:00:03,000 --> 00:00:04,000\nHello\n"
	cache := NewCache()
	cfg := echoConfig()
	cfg.Cache = cache
	for run := 0; run < 2; run++ {
		_, stats, err := Translate(context.Background(), []byte(source), "srt", "es", cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, stats, Stats{Cues: 2, Segments: 2, Translated: 1})
	}
	equal(t, cache.Len(), 1)
}

func TestTranslateErrors(t *testing.T) {
	ctx := context.Background()
	const source = "1\n00:00:01,000 --> 00:00:02,000\nHi\n"

	_, _, err := Translate(ctx, []byte(source), "srt", "xx", echoConfig(), nil)
	if !IsFatal(err) {
		t.Fatalf("unknown target: %v", err)
	}
	_, _, err = Translate(ctx, []byte("junk"), "srt", "es", echoConfig(), nil)
	var formatErr *SubtitleFormatError
	if !errors.As(err, &formatErr) {
		t.Fatalf("junk: %v", err)
	}
	_, stats, err := Translate(ctx, []byte(source), "srt", "es", Config{Provider: "anthropic", Model: "m"}, nil)
	if !IsFatal(err) || stats.Cues != 1 {
		t.Fatalf("missing key: %v %+v", err, stats)
	}
	_, _, err = Translate(ctx, []byte(source), "srt", "es", Config{Provider: "bogus"}, nil)
	contains(t, err.Error(), "Unknown provider: bogus")
	_, _, err = Translate(ctx, []byte(source), "txt", "es", echoConfig(), nil)
	contains(t, err.Error(), "Unsupported subtitle type .txt")
}

func TestTranslateFatalProviderErrorStopsTheRun(t *testing.T) {
	var calls atomic.Int32
	cfg := Config{Provider: "openai", APIKey: "bad", Model: "m", BatchSize: 1, Workers: 1,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return cannedResponse(401, `{"error":"invalid api key"}`), nil
		})}}
	source := "1\n00:00:01,000 --> 00:00:02,000\nA\n\n2\n00:00:03,000 --> 00:00:04,000\nB\n"

	_, _, err := Translate(context.Background(), []byte(source), "srt", "es", cfg, nil)
	if !IsFatal(err) {
		t.Fatalf("got %v", err)
	}
	contains(t, err.Error(), "HTTP 401")
	equal(t, calls.Load(), int32(1))
}

func TestTranslateExhaustedRateLimit(t *testing.T) {
	var calls atomic.Int32
	cfg := Config{Provider: "openai", APIKey: "k", Model: "m", RateRetries: 2,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return cannedResponse(429, `{"error":"slow down"}`, "retry-after", "0"), nil
		})}}
	source := "1\n00:00:01,000 --> 00:00:02,000\nA\n\n2\n00:00:03,000 --> 00:00:04,000\nB\n"

	_, _, err := Translate(context.Background(), []byte(source), "srt", "es", cfg, nil)
	var limited *RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("got %v", err)
	}
	equal(t, calls.Load(), int32(3)) // first attempt + 2 retries, no per-line fallback
	equal(t, ErrorTypeName(err), "RateLimitError")
}

func TestTranslateStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	var once sync.Once
	cfg := Config{Provider: "openai", APIKey: "k", Model: "m",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			once.Do(func() { close(entered) })
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}}
	go func() {
		<-entered
		cancel()
	}()
	start := time.Now()
	_, _, err := Translate(ctx, []byte("1\n00:00:01,000 --> 00:00:02,000\nA\n"), "srt", "es", cfg, nil)
	if !IsCanceled(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation was not prompt")
	}

	canceledCtx, stop := context.WithCancel(context.Background())
	stop()
	_, _, err = Translate(canceledCtx, []byte("1\n00:00:01,000 --> 00:00:02,000\nA\n"), "srt", "es", echoConfig(), nil)
	if !IsCanceled(err) {
		t.Fatalf("got %v", err)
	}
}

func TestTranslateSharesTheConfiguredThrottle(t *testing.T) {
	throttle := NewThrottle(0)
	throttle.Penalise(0.15)
	cfg := Config{Provider: "openai", APIKey: "k", Model: "m", Throttle: throttle,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return cannedResponse(200, `{"choices":[{"message":{"content":"1\tHola"}}]}`), nil
		})}}
	start := time.Now()
	if _, _, err := Translate(context.Background(), []byte("1\n00:00:01,000 --> 00:00:02,000\nA\n"), "srt", "es", cfg, nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 120*time.Millisecond {
		t.Fatal("the shared throttle was ignored")
	}
}

func TestTranslateRPMPacesWorkers(t *testing.T) {
	var times []time.Time
	var mu sync.Mutex
	cfg := Config{Provider: "openai", APIKey: "k", Model: "m", RPM: 1200, BatchSize: 1, Workers: 4,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			mu.Lock()
			times = append(times, time.Now())
			mu.Unlock()
			return cannedResponse(200, `{"choices":[{"message":{"content":"1\tx"}}]}`), nil
		})}}
	// Five distinct segments (5 requests at 1200 rpm = 50 ms apart).
	text := "1\n00:00:01,000 --> 00:00:02,000\na\n\n2\n00:00:01,000 --> 00:00:02,000\nb\n\n3\n00:00:01,000 --> 00:00:02,000\nc\n\n4\n00:00:01,000 --> 00:00:02,000\nd\n\n5\n00:00:01,000 --> 00:00:02,000\ne\n"
	start := time.Now()
	if _, _, err := Translate(context.Background(), []byte(text), "srt", "es", cfg, nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 180*time.Millisecond {
		t.Fatalf("5 requests at 1200 rpm finished in %v", elapsed)
	}
	equal(t, len(times), 5)
}

func TestTranslateLargeFileThroughBatchedWorkers(t *testing.T) {
	var source strings.Builder
	const cues = 250
	for i := 1; i <= cues; i++ {
		fmt.Fprintf(&source, "%d\n00:00:01,000 --> 00:00:02,000\nLine number %d\n\n", i, i)
	}
	var requests atomic.Int32
	cfg := Config{Provider: "openai", APIKey: "k", Model: "m", BatchSize: 20, Workers: 4,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests.Add(1)
			var body struct {
				Messages []struct{ Content string }
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
				return cannedResponse(400, "bad request"), nil
			}
			reply, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"message": map[string]any{"content": strings.ToUpper(body.Messages[1].Content)},
			}}})
			return cannedResponse(200, string(reply)), nil
		})}}
	var last [2]int
	out, stats, err := Translate(context.Background(), []byte(source.String()), "srt", "es", cfg,
		func(done, total int) { last = [2]int{done, total} })
	if err != nil {
		t.Fatal(err)
	}
	equal(t, requests.Load(), int32(13)) // ceil(250/20)
	equal(t, stats, Stats{Cues: cues, Segments: cues, Translated: cues})
	equal(t, last, [2]int{cues, cues})
	contains(t, string(out), "\n250\n00:00:01,000 --> 00:00:02,000\nLINE NUMBER 250\n")
	contains(t, string(out), "1\n00:00:01,000 --> 00:00:02,000\nLINE NUMBER 1\n")
}

func TestErrorTypeNames(t *testing.T) {
	for err, want := range map[error]string{
		formatErrorf("x"):           "SubtitleFormatError",
		transientf("x"):             "TranslationError",
		fatalf("x"):                 "FatalTranslationError",
		rateLimitf(nil, "x"):        "RateLimitError",
		canceled(canceledContext()): "TranslationCanceled",
		errors.New("plain"):         "Error",
	} {
		equal(t, ErrorTypeName(err), want)
	}
	wrapped := errors.Join(errors.New("ctx"), fatalf("inner"))
	equal(t, ErrorTypeName(wrapped), "FatalTranslationError")
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestErrorMessagesAndUnwrapping(t *testing.T) {
	cause := errors.New("root")
	wrapped := &TranslationError{Msg: "outer", Err: cause}
	if !errors.Is(wrapped, cause) || wrapped.Error() != "outer" {
		t.Fatal("TranslationError must unwrap")
	}
	formatErr := &SubtitleFormatError{Msg: "bad", Err: cause}
	if !errors.Is(formatErr, cause) {
		t.Fatal("SubtitleFormatError must unwrap")
	}
	err := canceled(canceledContext())
	contains(t, err.Error(), "translation canceled")
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) || !IsCanceled(err) {
		t.Fatal("cancellation must match ErrCanceled and context.Canceled")
	}
	equal(t, truncateRunes("héllo", 2), "hé")
	equal(t, truncateRunes("hi", 5), "hi")
	equal(t, truncateRunes("hi", 0), "")
	equal(t, pythonFloat(301), "301")
}
