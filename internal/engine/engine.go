package engine

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Provider names accepted in Config.Provider.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderDeepL     = "deepl"
	ProviderGoogle    = "google"
	// ProviderEcho is the offline development provider. The engine always
	// offers it; the backend gates it behind debug mode.
	ProviderEcho = "echo"
)

// Defaults applied by Translate for zero Config fields.
const (
	DefaultSourceLang = "English"
	DefaultBatchSize  = 20
	DefaultWorkers    = 4
	DefaultMaxLines   = 2
	DefaultWidth      = 16.0
)

// Config describes one translation run.
type Config struct {
	// Provider is one of the Provider* constants.
	Provider string
	APIKey   string
	// Model is the LLM model id (anthropic, openai).
	Model string
	// BaseURL is the OpenAI-compatible API base URL (openai only; default
	// https://api.openai.com/v1). Other providers ignore it.
	BaseURL string
	// SourceLang is the source language name as shown to the model, e.g.
	// "English" (the default). Google maps a recognised name or BCP-47 code
	// to its source parameter and otherwise auto-detects.
	SourceLang string

	// BatchSize is segments per request (default 20), Workers the concurrent
	// requests (default 4), MaxLines the most lines of a single-speaker cue
	// (default 2).
	BatchSize, Workers, MaxLines int
	// Width is the line width limit in full-width characters (default 16);
	// Latin text gets twice as many half-width cells.
	Width float64
	// RPM paces requests across all workers; zero means unlimited.
	RPM float64

	// Encoding is a Python-style codec name for the input ("utf-8" by
	// default; utf-16, gbk, big5, shift_jis, latin-1, cp1252 and so on). A
	// UTF-8 byte order mark overrides it. The output is always UTF-8.
	Encoding string

	// HTTPClient is used for provider requests (default http.DefaultClient).
	HTTPClient *http.Client
	// RequestTimeout bounds one provider request (default 120s).
	RequestTimeout time.Duration
	// Retries is the attempt budget per request for transient errors
	// (default 4); RateRetries the retry budget for rate limits (default 10,
	// negative for none).
	Retries, RateRetries int
	// RetryDelay overrides the pause before a transient retry.
	RetryDelay func(attempt int) time.Duration

	// Cache optionally shares translations between runs; nil uses a fresh one.
	Cache *Cache
	// Throttle optionally shares pacing and rate-limit back-off between runs;
	// nil creates one from RPM.
	Throttle *Throttle
}

// Stats summarises one Translate call.
type Stats struct {
	// Cues is the number of subtitle cues in the file.
	Cues int
	// Segments is the number of translatable units for this target
	// (speaker pairs count twice).
	Segments int
	// Translated counts segments that were translated or served from the
	// cache; segments without text and fallbacks are excluded.
	Translated int
	// Fallbacks counts segments passed through untranslated after a provider
	// failure.
	Fallbacks int
	// Warning is non-empty when part of the file was left untranslated.
	Warning string
}

// NewProvider builds the provider selected by cfg.Provider. A missing API key
// or an unknown provider is a *FatalTranslationError.
func NewProvider(cfg Config, throttle *Throttle) (Provider, error) {
	p := &poster{client: cfg.HTTPClient, timeout: cfg.RequestTimeout, throttle: throttle}
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case ProviderEcho:
		return NewEcho(), nil
	case ProviderAnthropic:
		if cfg.APIKey == "" {
			return nil, fatalf("Anthropic API key is not configured")
		}
		if cfg.Model == "" {
			return nil, fatalf("Anthropic model is not configured")
		}
		return newAnthropic(p, anthropicEndpoint, cfg.Model, cfg.APIKey), nil
	case ProviderOpenAI:
		if cfg.APIKey == "" {
			return nil, fatalf("OpenAI-compatible API key is not configured")
		}
		if cfg.Model == "" {
			return nil, fatalf("OpenAI-compatible model is not configured")
		}
		return newOpenAI(p, cfg.Model, cfg.APIKey, cfg.BaseURL), nil
	case ProviderDeepL:
		if cfg.APIKey == "" {
			return nil, fatalf("DeepL API key is not configured")
		}
		return newDeepL(p, cfg.APIKey, ""), nil
	case ProviderGoogle:
		if cfg.APIKey == "" {
			return nil, fatalf("Google Cloud Translation API key is not configured")
		}
		return newGoogle(p, cfg.APIKey, ""), nil
	}
	return nil, fatalf("Unknown provider: %s", cfg.Provider)
}

func (c Config) driverOptions(throttle *Throttle) DriverOptions {
	opt := DriverOptions{
		BatchSize:   c.BatchSize,
		Workers:     c.Workers,
		Retries:     c.Retries,
		RateRetries: c.RateRetries,
		Throttle:    throttle,
		RetryDelay:  c.RetryDelay,
	}
	if opt.BatchSize <= 0 {
		opt.BatchSize = DefaultBatchSize
	}
	if opt.Workers <= 0 {
		opt.Workers = DefaultWorkers
	}
	return opt
}

// Translate translates a subtitle file into one target language and returns
// the translated file as UTF-8 bytes in the same format. extension is ".srt",
// ".vtt", ".ass" or ".ssa" (or a bare "srt"/file name); target is a key of
// Languages. progress, when not nil, receives (completed, total) segments at
// the start and after every completed batch; calls never overlap.
//
// Errors are *SubtitleFormatError (unreadable file), *FatalTranslationError,
// *RateLimitError (exhausted), *TranslationError (retryable failures that
// escaped the driver) or a cancellation error matching ErrCanceled and the
// context error.
func Translate(ctx context.Context, source []byte, extension, target string, cfg Config,
	progress func(done, total int)) ([]byte, Stats, error) {
	var stats Stats
	if _, ok := Languages[target]; !ok {
		return nil, stats, fatalf("Unsupported target language: %s", target)
	}
	doc, err := ParseSubtitle(source, extension, cfg.Encoding)
	if err != nil {
		return nil, stats, err
	}
	stats.Cues = len(doc.Cues)
	if ctx.Err() != nil {
		return nil, stats, canceled(ctx)
	}
	segments := SegmentDocument(doc.Cues)
	stats.Segments = len(segments)

	throttle := cfg.Throttle
	if throttle == nil {
		throttle = NewThrottle(cfg.RPM)
	}
	provider, err := NewProvider(cfg, throttle)
	if err != nil {
		return nil, stats, err
	}
	sourceLang := cfg.SourceLang
	if strings.TrimSpace(sourceLang) == "" {
		sourceLang = DefaultSourceLang
	}

	opt := cfg.driverOptions(throttle)
	opt.Progress = progress
	opt.Fallback = func(count int) { stats.Fallbacks += count }
	translated, err := TranslateSegments(ctx, segments, provider, target, sourceLang, opt, cfg.Cache)
	if err != nil {
		return nil, stats, err
	}
	for _, seg := range segments {
		if NeedsTranslation(seg) {
			stats.Translated++
		}
	}
	stats.Translated -= stats.Fallbacks
	if stats.Fallbacks > 0 {
		stats.Warning = fmt.Sprintf("Left untranslated: %d of %d segments", stats.Fallbacks, stats.Segments)
	}

	width := cfg.Width
	if !(width > 0) {
		width = DefaultWidth
	}
	maxLines := cfg.MaxLines
	if maxLines < 1 {
		maxLines = DefaultMaxLines
	}
	cues, err := RebuildCues(doc.Cues, segments, translated, target, width, maxLines)
	if err != nil {
		return nil, stats, err
	}
	out, err := doc.CloneWithCues(cues).ToBytes()
	if err != nil {
		return nil, stats, err
	}
	return out, stats, nil
}
