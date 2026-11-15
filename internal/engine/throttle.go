package engine

import (
	"context"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxRateLimitWait is the longest server-requested pause (seconds) worth
// waiting out. A longer one (a daily quota, say) ends the run with the final
// RateLimitError instead.
const MaxRateLimitWait = 300.0

// Throttle is the shared pacing gate.
//
// It has two jobs. First, when any worker is told to back off, every worker
// waits: otherwise the other workers walk straight back into the limit and
// the backoff accomplishes nothing. Second, optional client-side pacing so a
// known RPM can be respected without discovering the ceiling by hitting it.
type Throttle struct {
	mu           sync.Mutex
	blockedUntil time.Time
	nextSlot     time.Time
	interval     time.Duration
}

// NewThrottle creates a throttle spacing requests 60/rpm seconds apart; an
// rpm of zero or less disables pacing (back-off parking still works).
func NewThrottle(rpm float64) *Throttle {
	t := &Throttle{}
	if rpm > 0 && !math.IsNaN(rpm) && !math.IsInf(rpm, 0) {
		// A tiny rate overflows time.Duration, and converting an out-of-range
		// float is implementation-defined (MinInt64 on amd64), which disabled
		// pacing. Clamp to the longest Duration instead: Python waited
		// practically forever too, and Wait stays interruptible by ctx.
		if interval := float64(time.Minute) / rpm; interval >= float64(math.MaxInt64) {
			t.interval = math.MaxInt64
		} else {
			t.interval = time.Duration(interval)
		}
	}
	return t
}

// Wait blocks until a request slot is free, or returns the cancellation error
// when ctx ends first.
func (t *Throttle) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return canceled(ctx)
		}
		t.mu.Lock()
		now := time.Now()
		target := t.blockedUntil
		if t.nextSlot.After(target) {
			target = t.nextSlot
		}
		if !now.Before(target) {
			slot := t.nextSlot
			if now.After(slot) {
				slot = now
			}
			t.nextSlot = slot.Add(t.interval)
			t.mu.Unlock()
			return nil
		}
		delay := target.Sub(now)
		t.mu.Unlock()
		if err := sleepContext(ctx, min(delay, 5*time.Second)); err != nil {
			return err
		}
	}
}

// Penalise parks every worker for the given seconds and returns the
// effective wait in seconds (an earlier, longer park may still apply).
func (t *Throttle) Penalise(seconds float64) float64 {
	if math.IsNaN(seconds) || seconds < 0 {
		seconds = 0
	}
	// Clamp to what a time.Duration can hold; callers bound this far lower.
	seconds = math.Min(seconds, 1e9)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	until := now.Add(time.Duration(seconds * float64(time.Second)))
	if until.After(t.blockedUntil) {
		t.blockedUntil = until
	}
	return t.blockedUntil.Sub(now).Seconds()
}

// sleepContext sleeps for d, or returns the cancellation error when ctx ends
// first.
func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return canceled(ctx)
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return canceled(ctx)
	case <-timer.C:
		return nil
	}
}

// finiteSeconds clamps a parsed delay to >= 0 and rejects infinity and NaN as
// malformed.
func finiteSeconds(value float64) *float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	value = math.Max(0, value)
	return &value
}

// pythonFloatRE is what Python's float() accepts that matters for headers:
// decimal numbers with an optional exponent, and inf/nan spellings.
var pythonFloatRE = regexp.MustCompile(`(?i)^[+-]?(?:(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:e[+-]?[0-9]+)?|inf(?:inity)?|nan)$`)

func parseFloatLoose(s string) (float64, bool) {
	if !pythonFloatRE.MatchString(s) {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil && !math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// ParseRetryAfter parses a retry-after header: delta-seconds or an HTTP
// date. Non-finite values are rejected as malformed.
func ParseRetryAfter(value string) *float64 {
	value = pyStrip(value)
	if value == "" {
		return nil
	}
	if v, ok := parseFloatLoose(value); ok {
		return finiteSeconds(v)
	}
	if moment, err := http.ParseTime(value); err == nil {
		seconds := moment.Sub(time.Now()).Seconds()
		return finiteSeconds(seconds)
	}
	return nil
}

var (
	goDurationRE     = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?(?:h|ms|us|µs|μs|ns|m|s))+$`)
	goDurationPartRE = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)(h|ms|us|µs|μs|ns|m|s)`)
	goDurationUnits  = map[string]float64{
		"h": 3600, "m": 60, "s": 1, "ms": 1e-3,
		"us": 1e-6, "µs": 1e-6, "μs": 1e-6, "ns": 1e-9,
	}
	isoLayouts = []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04Z07:00",
		"2006-01-02T15:04",
		"2006-01-02",
	}
)

// ParseRateLimitReset returns the seconds until a rate-limit reset header's
// moment.
//
// It accepts what retry-after accepts, RFC 3339 timestamps
// (anthropic-ratelimit-*-reset) and Go durations such as 6m0s or 250ms
// (x-ratelimit-reset-*). Past moments clamp to zero; nil means unparseable.
func ParseRateLimitReset(value string) *float64 {
	if after := ParseRetryAfter(value); after != nil || value == "" {
		return after
	}
	value = pyStrip(value)
	if value == "" {
		return nil
	}
	if goDurationRE.MatchString(value) {
		total := 0.0
		for _, part := range goDurationPartRE.FindAllStringSubmatch(value, -1) {
			// A long enough digit run overflows to +Inf, which is rejected.
			amount, _ := strconv.ParseFloat(part[1], 64)
			total += amount * goDurationUnits[part[2]]
		}
		return finiteSeconds(total)
	}
	iso := value
	if strings.HasSuffix(iso, "z") {
		iso = iso[:len(iso)-1] + "Z"
	}
	if len(iso) > 10 && iso[10] == 't' {
		iso = iso[:10] + "T" + iso[11:]
	}
	for _, layout := range isoLayouts {
		if moment, err := time.ParseInLocation(layout, iso, time.UTC); err == nil {
			return finiteSeconds(time.Until(moment).Seconds())
		}
	}
	return nil
}
