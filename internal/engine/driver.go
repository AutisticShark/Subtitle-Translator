package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"
)

// Default retry budgets: 4 attempts for genuine errors (5xx, network,
// malformed replies) and 10 retries for rate limits.
const (
	DefaultRetries     = 4
	DefaultRateRetries = 10
)

// Cache holds translations by content hash. It is safe for concurrent use. A
// later TranslateSegments call with the same cache and target reuses entries
// instead of sending those segments again. Nothing is persisted.
type Cache struct {
	mu      sync.Mutex
	entries map[string]string
}

// NewCache returns an empty cache.
func NewCache() *Cache { return &Cache{entries: map[string]string{}} }

func (c *Cache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.entries[key]
	return value, ok
}

func (c *Cache) set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = value
}

// Len is the number of cached translations.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// CacheKey is the cache key of a masked segment text for a target: the first
// 24 hex digits of SHA-256 over "<target>\x00<text>".
func CacheKey(target, text string) string {
	sum := sha256.Sum256([]byte(target + "\x00" + text))
	return hex.EncodeToString(sum[:])[:24]
}

// Put stores a translation under the key of source text for target.
func (c *Cache) Put(target, text, translated string) { c.set(CacheKey(target, text), translated) }

// Lookup returns the stored translation of source text for target.
func (c *Cache) Lookup(target, text string) (string, bool) { return c.get(CacheKey(target, text)) }

// DriverOptions tunes TranslateSegments.
type DriverOptions struct {
	// BatchSize is the number of segments per request (default 20).
	BatchSize int
	// Workers is the number of concurrent requests (default 4).
	Workers int
	// Retries is the attempt budget for transient errors per request
	// (DefaultRetries when zero).
	Retries int
	// RateRetries is the retry budget for rate limits (DefaultRateRetries when
	// zero; negative means none).
	RateRetries int
	// Throttle is shared by all workers; a throttle without pacing is created
	// when nil.
	Throttle *Throttle
	// RetryDelay is the pause before retrying a transient failure. The default
	// is min(2^attempt, 30) seconds plus up to a second of jitter.
	RetryDelay func(attempt int) time.Duration
	// Progress receives (completed, total) segments at cache discovery and
	// after every completed batch. It is called from one goroutine at a time.
	Progress func(done, total int)
	// Fallback receives the number of segments in each finished batch that
	// could not be translated and were passed through as source text.
	Fallback func(count int)
}

func defaultRetryDelay(attempt int) time.Duration {
	seconds := math.Min(math.Pow(2, float64(attempt)), 30) + rand.Float64()
	return time.Duration(seconds * float64(time.Second))
}

func (o DriverOptions) withDefaults() DriverOptions {
	if o.BatchSize <= 0 {
		o.BatchSize = 20
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.Retries == 0 {
		o.Retries = DefaultRetries
	}
	if o.RateRetries == 0 {
		o.RateRetries = DefaultRateRetries
	} else if o.RateRetries < 0 {
		o.RateRetries = 0
	}
	if o.Throttle == nil {
		o.Throttle = NewThrottle(0)
	}
	if o.RetryDelay == nil {
		o.RetryDelay = defaultRetryDelay
	}
	return o
}

// validateOutput rejects provider output that cannot be mapped back onto the
// input: the wrong number of strings, an empty translation of a line with
// text, or translations whose tag placeholders were dropped, duplicated or
// invented. It is reported as a retryable format error so the retry and
// per-line fallback path handles it.
func validateOutput(texts, output []string) error {
	if len(output) != len(texts) {
		return transientf("provider returned %d translations for %d lines", len(output), len(texts))
	}
	for position, source := range texts {
		translated := output[position]
		if hasText(source) && !hasText(translated) {
			return transientf("provider returned an empty line %d", position+1)
		}
		if !PlaceholdersMatch(source, translated) {
			return transientf("line %d lost or altered its markup placeholders", position+1)
		}
	}
	return nil
}

// errWorkerFailed reports a batch whose worker panicked or exited without a
// result. It is fatal, so the run ends instead of retrying into the same bug.
var errWorkerFailed = &FatalTranslationError{Msg: "internal error: a translation worker stopped unexpectedly"}

// maxPenaltySleep caps how long one worker waits out a parked throttle.
const maxPenaltySleep = 120.0

type batchResult struct {
	batch  []int
	out    []string
	passed map[int]bool
	err    error
}

// TranslateSegments translates segments in batches and returns one
// translation per segment. Segments with no text besides markup pass through
// without being sent. cache may be nil.
//
// Each batch gets its own retry budgets. A batch that keeps failing with a
// transient error is retried line by line, and a line that still fails passes
// through as source text, which is never cached. Fatal errors, exhausted rate
// limits and cancellation end the whole run: remaining workers stop calling
// the provider and in-flight requests are canceled.
func TranslateSegments(ctx context.Context, segs []Segment, provider Provider, target, source string,
	opt DriverOptions, cache *Cache) ([]string, error) {
	opt = opt.withDefaults()
	if cache == nil {
		cache = NewCache()
	}
	results := make([]string, len(segs))
	if ctx.Err() != nil {
		return nil, canceled(ctx)
	}

	// Cache hits first.
	var todo []int
	for i, seg := range segs {
		if !NeedsTranslation(seg) {
			// Empty text or markup only (e.g. an ASS drawing): nothing to send.
			results[i] = seg.Text
			continue
		}
		if hit, ok := cache.Lookup(target, seg.Text); ok && PlaceholdersMatch(seg.Text, hit) {
			results[i] = hit
		} else {
			todo = append(todo, i)
		}
	}
	var batches [][]int
	for start := 0; start < len(todo); start += opt.BatchSize {
		batches = append(batches, todo[start:min(start+opt.BatchSize, len(todo))])
	}
	cached := len(segs) - len(todo)
	if opt.Progress != nil {
		opt.Progress(cached, len(segs))
	}
	if len(batches) == 0 {
		return results, nil
	}

	// runCtx ends when the run ends abnormally (fatal error, exhausted rate
	// limit, cancellation), so in-flight workers stop before every further
	// request and abandon their current one.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	// The first failure wins and stops every other worker at once, before the
	// coordinating goroutine has even seen the result.
	var failMu sync.Mutex
	var firstErr error
	fail := func(err error) {
		failMu.Lock()
		defer failMu.Unlock()
		if firstErr == nil {
			firstErr = err
			stop()
		}
	}

	callWithRetry := func(texts []string, label string) ([]string, error) {
		// Two independent budgets. Rate limits are not failures: they are the
		// API telling us to slow down, so they get their own generous
		// allowance and do not consume the budget reserved for genuine errors.
		soft, limited := 0, 0
		for {
			if runCtx.Err() != nil {
				return nil, canceled(runCtx)
			}
			output, err := provider(runCtx, texts, source, target)
			if err == nil {
				if runCtx.Err() != nil {
					return nil, canceled(runCtx)
				}
				if err = validateOutput(texts, output); err == nil {
					return output, nil
				}
			}
			if runCtx.Err() != nil {
				return nil, canceled(runCtx)
			}
			if IsFatal(err) {
				return nil, err
			}
			if limit, ok := asRateLimit(err); ok {
				limited++
				if limited > opt.RateRetries {
					// Stay a RateLimitError so the run aborts instead of
					// retrying every line individually while over quota.
					return nil, rateLimitf(limit.RetryAfter, "rate limited %dx, giving up on %s", limited, label)
				}
				// Prefer the server's own number; otherwise exponential with
				// jitter so parallel workers do not resynchronise on retry.
				var delay float64
				if limit.RetryAfter != nil {
					delay = *limit.RetryAfter
					// `!(<=)` also catches NaN. The throttle would otherwise
					// park every worker for as long as the provider asked.
					if !(delay <= MaxRateLimitWait) {
						return nil, rateLimitf(limit.RetryAfter,
							"rate limited for %ss, giving up on %s", pythonFloat(delay), label)
					}
				} else {
					delay = math.Min(math.Pow(2, float64(limited)), 60)
				}
				delay += rand.Float64() * math.Min(delay*0.25, 5)
				waited := opt.Throttle.Penalise(delay)
				if err := sleepContext(runCtx, time.Duration(math.Min(waited, maxPenaltySleep)*float64(time.Second))); err != nil {
					return nil, err
				}
				continue
			}
			if !isTransient(err) {
				// Unknown error classes are not guessed at: end the run.
				return nil, err
			}
			soft++
			if soft >= opt.Retries {
				return nil, err
			}
			if err := sleepContext(runCtx, opt.RetryDelay(soft)); err != nil {
				return nil, err
			}
		}
	}

	runBatch := func(batch []int) batchResult {
		texts := make([]string, len(batch))
		for k, i := range batch {
			texts[k] = segs[i].Text
		}
		out, err := callWithRetry(texts, "batch of "+strconv.Itoa(len(texts)))
		if err == nil {
			return batchResult{batch: batch, out: out}
		}
		if _, limited := asRateLimit(err); limited || IsFatal(err) || !isTransient(err) {
			return batchResult{batch: batch, err: err}
		}
		if len(texts) == 1 {
			// Retrying one line alone would repeat the request that just used
			// up its retries; pass the source through instead.
			return batchResult{batch: batch, out: texts, passed: map[int]bool{0: true}}
		}
		// Genuine batch failure, usually one malformed line breaking the
		// numbered-output contract. Retry individually so one bad cue cannot
		// sink nineteen good ones. Never do this for rate limits: it turns
		// one rejected request into twenty while already over quota.
		lines := make([]string, len(texts))
		passed := map[int]bool{}
		for position, text := range texts {
			single, err := callWithRetry([]string{text}, "single line")
			switch {
			case err == nil:
				lines[position] = single[0]
			case IsFatal(err), !isTransient(err):
				return batchResult{batch: batch, err: err}
			default:
				if _, limited := asRateLimit(err); limited {
					return batchResult{batch: batch, err: err}
				}
				lines[position] = text // last resort: source passes through
				passed[position] = true
			}
		}
		return batchResult{batch: batch, out: lines, passed: passed}
	}

	jobs := make(chan []int, len(batches))
	for _, batch := range batches {
		jobs <- batch
	}
	close(jobs)
	// Buffered for every batch, so abandoned workers never block on send.
	done := make(chan batchResult, len(batches))
	// work runs one batch and always reports it, even when the provider (or
	// a RetryDelay) panics or calls runtime.Goexit: a panic in a goroutine
	// started here could not be recovered by the caller and would end the
	// whole process. It becomes a fixed fatal error that stops the other
	// workers; the panic value is dropped, as it may quote request data or
	// credentials.
	work := func(batch []int) {
		res := batchResult{batch: batch, err: errWorkerFailed}
		defer func() {
			if recover() != nil {
				res = batchResult{batch: batch, err: errWorkerFailed}
			}
			if res.err != nil {
				fail(res.err)
			}
			done <- res
		}()
		res = runBatch(batch)
	}
	for w := 0; w < min(opt.Workers, len(batches)); w++ {
		go func() {
			for batch := range jobs {
				if runCtx.Err() != nil {
					return
				}
				work(batch)
			}
		}()
	}

	completed := 0
	for received := 0; received < len(batches); received++ {
		var res batchResult
		select {
		case res = <-done:
		case <-ctx.Done():
			return nil, canceled(ctx)
		}
		if res.err != nil {
			failMu.Lock()
			defer failMu.Unlock()
			return nil, firstErr
		}
		for position, i := range res.batch {
			results[i] = res.out[position]
			if res.passed[position] {
				continue // never cache untranslated source text
			}
			cache.Put(target, segs[i].Text, res.out[position])
		}
		if len(res.passed) > 0 && opt.Fallback != nil {
			opt.Fallback(len(res.passed))
		}
		completed += len(res.batch)
		if opt.Progress != nil {
			opt.Progress(cached+completed, len(segs))
		}
	}
	if ctx.Err() != nil {
		return nil, canceled(ctx)
	}
	return results, nil
}
