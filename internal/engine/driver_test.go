package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func upper(s string) string { return strings.ToUpper(s) }

func TestCacheHitsSkipTheProviderAndAreReportedAsComplete(t *testing.T) {
	cache := NewCache()
	cache.Put("es", "Hello", "Hola")
	var progress [][2]int
	opt := testOptions()
	opt.Progress = func(done, total int) { progress = append(progress, [2]int{done, total}) }

	out, err := runTranslation(t, plainSegments("Hello"), providerFunc(func([]string) ([]string, error) {
		t.Fatal("provider should not be called")
		return nil, nil
	}), opt, cache)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"Hola"})
	equal(t, progress, [][2]int{{1, 1}})
}

func TestCacheKeyMatchesThePythonScheme(t *testing.T) {
	// sha256("es\x00Hello")[:24]
	equal(t, CacheKey("es", "Hello"), "1d4b57bb014074f7c7daa138")
	if len(CacheKey("ja", "x")) != 24 {
		t.Fatal("key must be 24 hex digits")
	}
	if CacheKey("es", "x") == CacheKey("de", "x") {
		t.Fatal("keys must include the target")
	}
}

func TestInvalidCachedTranslationIsNotReused(t *testing.T) {
	seg := SegmentCue(newCue("", "<i>Hello</i>"), 0)[0]
	cache := NewCache()
	cache.Put("es", seg.Text, "⟦0⟧Hola")
	var rec recorder

	out, err := runTranslation(t, []Segment{seg}, rec.provider(func(s string) string { return "T:" + s }), testOptions(), cache)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, rec.calls, [][]string{{seg.Text}})
	equal(t, out, []string{"T:⟦0⟧Hello⟦1⟧"})
}

func TestTranslationsAreCachedAndReusedByALaterRun(t *testing.T) {
	cache := NewCache()
	var rec recorder
	for i := 0; i < 2; i++ {
		if _, err := runTranslation(t, plainSegments("a", "b"), rec.provider(upper), testOptions(), cache); err != nil {
			t.Fatal(err)
		}
	}
	equal(t, len(rec.calls), 1)
	equal(t, cache.Len(), 2)
}

func TestProgressReportsEachCompletedBatch(t *testing.T) {
	segs := plainSegments("Line 0", "Line 1", "Line 2", "Line 3")
	var updates [][2]int
	opt := testOptions()
	opt.BatchSize, opt.Workers = 1, 2
	opt.Progress = func(done, total int) { updates = append(updates, [2]int{done, total}) }

	out, err := runTranslation(t, segs, NewEcho(), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, updates, [][2]int{{0, 4}, {1, 4}, {2, 4}, {3, 4}, {4, 4}})
	equal(t, out, []string{"[es] Line 0", "[es] Line 1", "[es] Line 2", "[es] Line 3"})
}

func TestBatchingAndOrderWithManyWorkers(t *testing.T) {
	texts := make([]string, 53)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}
	var rec recorder
	opt := testOptions()
	opt.BatchSize, opt.Workers = 5, 8

	out, err := runTranslation(t, plainSegments(texts...), rec.provider(upper), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range texts {
		equal(t, out[i], strings.ToUpper(text))
	}
	equal(t, len(rec.calls), 11) // ceil(53/5)
	for _, call := range rec.calls {
		if len(call) > 5 {
			t.Fatalf("batch of %d", len(call))
		}
	}
}

func TestTransientFailureIsRetried(t *testing.T) {
	var calls int
	opt := testOptions()
	opt.Retries = 2

	out, err := runTranslation(t, plainSegments("Hello"), providerFunc(func([]string) ([]string, error) {
		calls++
		if calls == 1 {
			return nil, transientf("temporary")
		}
		return []string{"Hola"}, nil
	}), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"Hola"})
	equal(t, calls, 2)
}

func TestRetryUsesTheConfiguredDelay(t *testing.T) {
	var delays []int
	opt := testOptions()
	opt.Retries = 4
	opt.RetryDelay = func(attempt int) time.Duration {
		delays = append(delays, attempt)
		return 0
	}
	var calls int
	_, err := runTranslation(t, plainSegments("a"), providerFunc(func([]string) ([]string, error) {
		calls++
		return nil, transientf("down")
	}), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, calls, 4)
	equal(t, delays, []int{1, 2, 3})
}

func TestDefaultRetryDelayGrowsAndIsJittered(t *testing.T) {
	for attempt, base := range map[int]float64{1: 2, 2: 4, 3: 8, 4: 16, 5: 30, 9: 30} {
		d := defaultRetryDelay(attempt).Seconds()
		if d < base || d >= base+1 {
			t.Errorf("attempt %d: %v not in [%v, %v)", attempt, d, base, base+1)
		}
	}
}

func TestBatchFailureFallsBackPerLineAndPassesThroughABadLine(t *testing.T) {
	provider := providerFunc(func(texts []string) ([]string, error) {
		if len(texts) > 1 || texts[0] == "bad" {
			return nil, transientf("cannot translate")
		}
		return []string{strings.ToUpper(texts[0])}, nil
	})

	out, err := runTranslation(t, plainSegments("good", "bad"), provider, testOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"GOOD", "bad"})
}

func TestPassThroughLinesAreReportedAndNeverCached(t *testing.T) {
	provider := providerFunc(func(texts []string) ([]string, error) {
		if len(texts) > 1 || texts[0] == "bad" {
			return nil, transientf("cannot translate")
		}
		return []string{strings.ToUpper(texts[0])}, nil
	})
	var reported []int
	cache := NewCache()
	opt := testOptions()
	opt.Fallback = func(n int) { reported = append(reported, n) }

	if _, err := runTranslation(t, plainSegments("good", "bad"), provider, opt, cache); err != nil {
		t.Fatal(err)
	}
	equal(t, reported, []int{1})
	equal(t, cache.Len(), 1)
	if hit, ok := cache.Lookup("es", "good"); !ok || hit != "GOOD" {
		t.Fatal("good line not cached")
	}
	if _, ok := cache.Lookup("es", "bad"); ok {
		t.Fatal("passthrough must not be cached")
	}
}

func TestFailedSingleSegmentBatchIsNotRetriedAgain(t *testing.T) {
	var calls [][]string
	var reported []int
	opt := testOptions()
	opt.Retries = 2
	opt.Fallback = func(n int) { reported = append(reported, n) }

	out, err := runTranslation(t, plainSegments("Hello"), providerFunc(func(texts []string) ([]string, error) {
		calls = append(calls, texts)
		return nil, transientf("broken")
	}), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"Hello"})
	equal(t, len(calls), 2)
	equal(t, reported, []int{1})
}

func TestEmptyReplyForTextIsRetriedAndNeverCached(t *testing.T) {
	provider := providerFunc(func(texts []string) ([]string, error) {
		out := make([]string, len(texts))
		for i, text := range texts {
			if text != "bad" {
				out[i] = strings.ToUpper(text)
			}
		}
		return out, nil
	})
	var reported []int
	cache := NewCache()
	opt := testOptions()
	opt.Fallback = func(n int) { reported = append(reported, n) }

	out, err := runTranslation(t, plainSegments("good", "bad"), provider, opt, cache)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"GOOD", "bad"})
	equal(t, reported, []int{1})
	equal(t, cache.Len(), 1)

	// A reply that keeps only the markup is empty too.
	out, err = runTranslation(t, []Segment{{Text: "⟦0⟧bad too", Tags: []string{"<i>"}}},
		providerFunc(func([]string) ([]string, error) { return []string{"⟦0⟧ "}, nil }), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"⟦0⟧bad too"})
	equal(t, reported, []int{1, 1})
}

func TestDroppedPlaceholderIsRetriedThenPassedThrough(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	provider := providerFunc(func(texts []string) ([]string, error) {
		mu.Lock()
		calls = append(calls, append([]string(nil), texts...))
		mu.Unlock()
		out := make([]string, len(texts))
		for i, text := range texts {
			out[i] = strings.ReplaceAll(text, "⟦1⟧", "")
		}
		return out, nil
	})
	segs := segmentsFor(newCue("", "<i>Hello</i>"), Cue{Index: 2, Start: cueStart, End: cueEnd, Lines: []string{"Plain"}})
	var reported []int
	cache := NewCache()
	opt := testOptions()
	opt.Retries = 2
	opt.Fallback = func(n int) { reported = append(reported, n) }

	out, err := runTranslation(t, segs, provider, opt, cache)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"⟦0⟧Hello⟦1⟧", "Plain"})
	equal(t, reported, []int{1})
	equal(t, cache.Len(), 1)
	if _, ok := cache.Lookup("es", "Plain"); !ok {
		t.Fatal("Plain should be cached")
	}
	found := false
	for _, call := range calls {
		if len(call) == 1 && call[0] == segs[0].Text {
			found = true
		}
	}
	if !found {
		t.Fatalf("the marked-up line was never retried alone: %q", calls)
	}
}

func TestWrongTranslationCountIsARetryableFormatError(t *testing.T) {
	provider := providerFunc(func(texts []string) ([]string, error) {
		if len(texts) > 1 {
			return []string{"only one"}, nil
		}
		return []string{strings.ToUpper(texts[0])}, nil
	})
	out, err := runTranslation(t, plainSegments("a", "b"), provider, testOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"A", "B"})
}

func TestExhaustedRateLimitAbortsWithoutPerLineFallback(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	provider := providerFunc(func(texts []string) ([]string, error) {
		mu.Lock()
		calls = append(calls, texts)
		mu.Unlock()
		return nil, &RateLimitError{Msg: "429", RetryAfter: f64(0)}
	})

	_, err := runTranslation(t, plainSegments("line 0", "line 1", "line 2", "line 3", "line 4"), provider, testOptions(), nil)

	var limited *RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("got %v", err)
	}
	contains(t, err.Error(), "rate limited 2x, giving up on batch of 5")
	// One batch attempt plus its single rate-limit retry; no per-line requests.
	equal(t, len(calls), 2)
	for _, call := range calls {
		equal(t, len(call), 5)
	}
}

func TestRateLimitRetriesRecover(t *testing.T) {
	var calls int
	opt := testOptions()
	opt.RateRetries = 3
	out, err := runTranslation(t, plainSegments("a"), providerFunc(func(texts []string) ([]string, error) {
		calls++
		if calls < 3 {
			return nil, &RateLimitError{Msg: "429", RetryAfter: f64(0)}
		}
		return []string{"A"}, nil
	}), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"A"})
	equal(t, calls, 3)
}

func TestRateLimitsDoNotConsumeTheTransientBudget(t *testing.T) {
	var calls int
	opt := testOptions()
	opt.Retries, opt.RateRetries = 2, 5
	out, err := runTranslation(t, plainSegments("a"), providerFunc(func(texts []string) ([]string, error) {
		calls++
		switch calls {
		case 1, 2, 3:
			return nil, &RateLimitError{Msg: "429", RetryAfter: f64(0)}
		case 4:
			return nil, transientf("blip")
		}
		return []string{"A"}, nil
	}), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"A"})
	equal(t, calls, 5)
}

func TestTooLongAWaitGivesUpWithoutParkingWorkers(t *testing.T) {
	for _, retryAfter := range []float64{MaxRateLimitWait + 1, 1e300, math.Inf(1), math.NaN()} {
		var calls int
		throttle := NewThrottle(0)
		opt := testOptions()
		opt.RateRetries, opt.Throttle = 5, throttle

		_, err := runTranslation(t, plainSegments("Hello"), providerFunc(func([]string) ([]string, error) {
			calls++
			return nil, &RateLimitError{Msg: "429", RetryAfter: f64(retryAfter)}
		}), opt, nil)

		var limited *RateLimitError
		if !errors.As(err, &limited) {
			t.Fatalf("retryAfter %v: got %v", retryAfter, err)
		}
		equal(t, calls, 1)
		equal(t, throttle.Penalise(0), 0.0)
	}
	// The limit itself is still waited out.
	var calls int
	opt := testOptions()
	opt.RateRetries = 1
	_, err := runTranslation(t, plainSegments("Hello"), providerFunc(func([]string) ([]string, error) {
		calls++
		return nil, &RateLimitError{Msg: "429", RetryAfter: f64(0)}
	}), opt, nil)
	if err == nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestRateLimitParksTheSharedThrottle(t *testing.T) {
	throttle := NewThrottle(0)
	opt := testOptions()
	opt.RateRetries, opt.Throttle = 1, throttle
	var calls int
	start := time.Now()
	_, err := runTranslation(t, plainSegments("a"), providerFunc(func([]string) ([]string, error) {
		calls++
		if calls == 1 {
			return nil, &RateLimitError{Msg: "429", RetryAfter: f64(0.1)}
		}
		return []string{"A"}, nil
	}), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("rate-limit delay was not honoured: %v", elapsed)
	}
}

func TestRateLimitDuringPerLineFallbackAborts(t *testing.T) {
	// One soft attempt and no rate-limit retries: nothing ever sleeps.
	var mu sync.Mutex
	var calls [][]string
	opt := testOptions()
	opt.RateRetries = -1
	provider := providerFunc(func(texts []string) ([]string, error) {
		mu.Lock()
		calls = append(calls, append([]string(nil), texts...))
		mu.Unlock()
		if len(texts) > 1 {
			return nil, transientf("malformed batch")
		}
		return nil, &RateLimitError{Msg: "429", RetryAfter: f64(0)}
	})

	_, err := runTranslation(t, plainSegments("line 0", "line 1", "line 2", "line 3", "line 4"), provider, opt, nil)

	var limited *RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("got %v", err)
	}
	equal(t, len(calls), 2)
}

func TestFatalFailureAbortsWithoutPerLineFallback(t *testing.T) {
	var calls int
	_, err := runTranslation(t, plainSegments("a", "b"), providerFunc(func([]string) ([]string, error) {
		calls++
		return nil, fatalf("bad credentials")
	}), testOptions(), nil)

	if !IsFatal(err) {
		t.Fatalf("got %v", err)
	}
	contains(t, err.Error(), "bad credentials")
	equal(t, calls, 1)
	equal(t, ErrorTypeName(err), "FatalTranslationError")
}

func TestFatalDuringPerLineFallbackAborts(t *testing.T) {
	provider := providerFunc(func(texts []string) ([]string, error) {
		if len(texts) > 1 {
			return nil, transientf("malformed batch")
		}
		return nil, fatalf("forbidden")
	})
	_, err := runTranslation(t, plainSegments("a", "b"), provider, testOptions(), nil)
	if !IsFatal(err) {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownProviderErrorsEndTheRun(t *testing.T) {
	boom := errors.New("boom")
	var calls int
	opt := testOptions()
	opt.Retries = 5
	_, err := runTranslation(t, plainSegments("a"), providerFunc(func([]string) ([]string, error) {
		calls++
		return nil, boom
	}), opt, nil)
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestWorkersStopCallingTheProviderAfterAFatalError(t *testing.T) {
	otherStarted := make(chan struct{})
	release := make(chan struct{})
	var otherCalls atomic.Int32
	provider := providerFunc(func(texts []string) ([]string, error) {
		if texts[0] == "fatal" {
			<-otherStarted
			return nil, fatalf("bad key")
		}
		otherCalls.Add(1)
		select {
		case <-otherStarted:
		default:
			close(otherStarted)
		}
		<-release
		return nil, &RateLimitError{Msg: "429", RetryAfter: f64(0)}
	})
	opt := testOptions()
	opt.BatchSize, opt.Workers, opt.RateRetries = 1, 2, 5

	_, err := runTranslation(t, plainSegments("fatal", "other"), provider, opt, nil)
	if !IsFatal(err) {
		t.Fatalf("got %v", err)
	}
	close(release)
	time.Sleep(300 * time.Millisecond)

	// Without the stop signal the other worker retries straight away.
	equal(t, otherCalls.Load(), int32(1))
}

func TestCancellationIsCheckedBeforeWorkStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := TranslateSegments(ctx, plainSegments("Hello"), providerFunc(func([]string) ([]string, error) {
		t.Fatal("provider must not run")
		return nil, nil
	}), "es", "English", testOptions(), nil)

	if !IsCanceled(err) || !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	equal(t, ErrorTypeName(err), "TranslationCanceled")
}

func TestCancellationStopsInFlightRequestsAndRetrySleeps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	provider := func(ctx context.Context, _ []string, _, _ string) ([]string, error) {
		close(entered)
		<-ctx.Done() // a request that only ends when canceled
		return nil, canceled(ctx)
	}
	opt := testOptions()
	go func() {
		<-entered
		cancel()
	}()

	start := time.Now()
	_, err := TranslateSegments(ctx, plainSegments("a"), provider, "es", "English", opt, nil)
	if !IsCanceled(err) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation was not prompt")
	}

	// A long back-off sleep is interrupted too.
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	opt = testOptions()
	opt.Retries = 3
	opt.RetryDelay = func(int) time.Duration { return time.Hour }
	start = time.Now()
	_, err = TranslateSegments(ctx, plainSegments("a"), providerFunc(func([]string) ([]string, error) {
		return nil, transientf("down")
	}), "es", "English", opt, nil)
	if !IsCanceled(err) || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestCancellationAbandonsABlockedProviderWithoutWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{})
	provider := providerFunc(func([]string) ([]string, error) {
		close(entered)
		<-release // ignores the context, like a stuck third-party call
		return []string{"x"}, nil
	})
	go func() {
		<-entered
		cancel()
	}()
	start := time.Now()
	_, err := TranslateSegments(ctx, plainSegments("a"), provider, "es", "English", testOptions(), nil)
	if !IsCanceled(err) || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestProgressCallbacksNeverOverlap(t *testing.T) {
	var active, overlaps atomic.Int32
	opt := testOptions()
	opt.BatchSize, opt.Workers = 1, 8
	opt.Progress = func(done, total int) {
		if active.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
	}
	texts := make([]string, 40)
	for i := range texts {
		texts[i] = "line"
	}
	if _, err := runTranslation(t, plainSegments(texts...), NewEcho(), opt, nil); err != nil {
		t.Fatal(err)
	}
	equal(t, overlaps.Load(), int32(0))
}

func TestWorkersRunConcurrently(t *testing.T) {
	var running, peak atomic.Int32
	provider := providerFunc(func(texts []string) ([]string, error) {
		n := running.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		running.Add(-1)
		return texts, nil
	})
	opt := testOptions()
	opt.BatchSize, opt.Workers = 1, 4
	if _, err := runTranslation(t, plainSegments("a", "b", "c", "d"), provider, opt, nil); err != nil {
		t.Fatal(err)
	}
	if peak.Load() < 2 {
		t.Fatalf("workers did not overlap (peak %d)", peak.Load())
	}
}

func TestDriverOptionDefaults(t *testing.T) {
	opt := DriverOptions{}.withDefaults()
	equal(t, []int{opt.BatchSize, opt.Workers, opt.Retries, opt.RateRetries}, []int{20, 4, 4, 10})
	if opt.Throttle == nil || opt.RetryDelay == nil {
		t.Fatal("defaults missing")
	}
	equal(t, DriverOptions{RateRetries: -1}.withDefaults().RateRetries, 0)
}

func TestNoSegmentsNeedTranslation(t *testing.T) {
	var progress [][2]int
	opt := testOptions()
	opt.Progress = func(done, total int) { progress = append(progress, [2]int{done, total}) }
	out, err := runTranslation(t, []Segment{{Text: ""}, {Text: "⟦0⟧", Tags: []string{"<i>"}}}, NewEcho(), opt, nil)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, out, []string{"", "⟦0⟧"})
	equal(t, progress, [][2]int{{2, 2}})
	out, err = runTranslation(t, nil, NewEcho(), opt, nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("%v %v", out, err)
	}
}

// A tiny RPM once overflowed the interval to a negative Duration, which
// turned pacing off. It now clamps to the longest wait, still interruptible.
func TestTinyRPMClampsInsteadOfDisablingPacing(t *testing.T) {
	for _, rpm := range []float64{1e-12, 1e-300, math.SmallestNonzeroFloat64} {
		throttle := NewThrottle(rpm)
		if throttle.interval != time.Duration(math.MaxInt64) {
			t.Fatalf("rpm %g: interval %v", rpm, throttle.interval)
		}
		if err := throttle.Wait(context.Background()); err != nil {
			t.Fatalf("first request must not wait: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		err := throttle.Wait(ctx)
		cancel()
		if !IsCanceled(err) {
			t.Fatalf("rpm %g: second request was not paced: %v", rpm, err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("cancellation took %v", elapsed)
		}
	}
	if got := NewThrottle(60).interval; got != time.Second {
		t.Fatalf("ordinary rpm interval %v", got)
	}
}

// A provider panic (or runtime.Goexit) inside a worker goroutine must not kill
// the process: the run ends promptly with a fatal error that stops the other
// workers and never quotes the panic value.
func TestProviderPanicEndsTheRunWithoutCrashing(t *testing.T) {
	for _, mode := range []string{"panic", "goexit"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			release := make(chan struct{})
			provider := func(ctx context.Context, texts []string, _, _ string) ([]string, error) {
				if calls.Add(1) == 2 {
					if mode == "panic" {
						panic("boom with SECRET-KEY and the subtitle text")
					}
					runtime.Goexit()
				}
				select { // other workers block until the run is stopped
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-release:
					return texts, nil
				}
			}
			defer close(release)
			texts := make([]string, 40)
			for i := range texts {
				texts[i] = fmt.Sprintf("line %d", i)
			}
			opt := testOptions()
			opt.BatchSize, opt.Workers = 1, 4
			start := time.Now()
			_, err := TranslateSegments(context.Background(), plainSegments(texts...), provider, "es", "English", opt, nil)
			if !IsFatal(err) || !errors.Is(err, errWorkerFailed) {
				t.Fatalf("got %v", err)
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "boom") {
				t.Fatalf("panic value leaked: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("run ended after %v", elapsed)
			}
			time.Sleep(50 * time.Millisecond)
			if n := calls.Load(); n > 8 {
				t.Fatalf("workers kept calling the provider: %d calls", n)
			}
		})
	}
}
