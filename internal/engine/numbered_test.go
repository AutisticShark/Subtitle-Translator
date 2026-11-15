package engine

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestParseNumberedAcceptsCommonSeparatorsAndReorders(t *testing.T) {
	// Unnumbered text before the first entry is preamble; after an entry it
	// would be a continuation of that entry.
	got, err := ParseNumbered("ignored heading\n2) second\n1： first", 2)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"first", "second"})
}

func TestParseNumberedSeparators(t *testing.T) {
	for _, in := range []string{"1\ta\n2\tb", "1.a\n2.b", "1) a\n2) b", "1: a\n2: b", "1、a\n2、b", "1 - a\n2 - b", "1：a\n2：b", " 1\t a \n\n2\tb"} {
		got, err := ParseNumbered(in, 2)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		equal(t, got, []string{"a", "b"})
	}
}

func TestParseNumberedRejectsMissingLines(t *testing.T) {
	_, err := ParseNumbered("1\tone", 2)
	if err == nil {
		t.Fatal("expected an error")
	}
	contains(t, err.Error(), "missing [2]")
	contains(t, err.Error(), "1/2 lines")
	equal(t, ErrorTypeName(err), "TranslationError")
	if IsFatal(err) {
		t.Fatal("a short reply is retryable")
	}

	_, err = ParseNumbered("", 12)
	contains(t, err.Error(), "missing [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]")
}

func TestParseNumberedAcceptsAnEmptyTranslation(t *testing.T) {
	for _, in := range []string{"1\tHola\n2\t\n3\tAdiós", "1\tHola\n2\n3\tAdiós", "1. Hola\n2.\n3. Adiós"} {
		got, err := ParseNumbered(in, 3)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		equal(t, got, []string{"Hola", "", "Adiós"})
	}
}

func TestParseNumberedUnnumberedLinesContinueThePreviousEntry(t *testing.T) {
	got, err := ParseNumbered("1\tHe said that he would\ncome back tomorrow.\n2\tBye", 2)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"He said that he would come back tomorrow.", "Bye"})
}

func TestParseNumberedCJKContinuationHasNoSpace(t *testing.T) {
	got, err := ParseNumbered("1\t他說他明天\n會回來。", 1)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"他說他明天會回來。"})
}

func TestParseNumberedOutOfRangeNumberIsContinuationText(t *testing.T) {
	got, err := ParseNumbered("1\tThe year was\n1999. Then\n2\tOk", 2)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"The year was 1999. Then", "Ok"})
}

func TestParseNumberedSkipsCodeFencesAndDuplicatesOverwrite(t *testing.T) {
	got, err := ParseNumbered("```\n1\tfirst\n2\tsecond\n1\toverwritten\n```", 2)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"overwritten", "second"})
}

func TestParseNumberedBareRepeatedNumberContinuesTheEntry(t *testing.T) {
	// A bare "2" already seen is continuation text, not a new empty entry.
	got, err := ParseNumbered("1\tfirst\n2\tsecond\n2", 2)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"first", "second 2"})
}

func TestParseNumberedSplitsOnUnicodeLineBreaks(t *testing.T) {
	got, err := ParseNumbered("1\ta \u00852\tb\r3\tc\v4\td", 4)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"a", "b", "c", "d"})
}

func TestParseNumberedHugeNumberIsOutOfRange(t *testing.T) {
	got, err := ParseNumbered("1\ta\n99999999999999999999999 more", 1)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"a 99999999999999999999999 more"})
}

func f64(v float64) *float64 { return &v }

func TestRetryAfterParsesSecondsAndRejectsGarbage(t *testing.T) {
	equal(t, ParseRetryAfter(" 2.5 "), f64(2.5))
	equal(t, ParseRetryAfter("-1"), f64(0))
	equal(t, ParseRetryAfter("12"), f64(12))
	equal(t, ParseRetryAfter("1e2"), f64(100))
	equal(t, ParseRetryAfter("1e-400"), f64(0))
	if ParseRetryAfter("not a date") != nil || ParseRetryAfter("") != nil || ParseRetryAfter("0x10") != nil {
		t.Fatal("garbage must be rejected")
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	soon := time.Now().UTC().Add(30 * time.Second).Format(http.TimeFormat)
	got := ParseRetryAfter(soon)
	if got == nil || math.Abs(*got-30) > 2 {
		t.Fatalf("got %v", got)
	}
	equal(t, ParseRetryAfter("Wed, 21 Oct 2015 07:28:00 GMT"), f64(0))
}

func TestNonFiniteHeaderValuesAreRejected(t *testing.T) {
	for _, value := range []string{"inf", "Infinity", "-inf", "1e400", "nan", "NaN"} {
		if ParseRetryAfter(value) != nil {
			t.Errorf("ParseRetryAfter(%q) accepted", value)
		}
		if ParseRateLimitReset(value) != nil {
			t.Errorf("ParseRateLimitReset(%q) accepted", value)
		}
	}
	if ParseRateLimitReset("9999999999h") == nil {
		t.Error("large finite duration rejected")
	}
	huge := make([]byte, 400)
	for i := range huge {
		huge[i] = '9'
	}
	if ParseRateLimitReset(string(huge)+"h") != nil {
		t.Error("overflowing duration accepted")
	}
}

func TestRateLimitResetRFC3339IsRelativeToNow(t *testing.T) {
	stamp := time.Now().UTC().Add(30 * time.Second).Format("2006-01-02T15:04:05Z")
	got := ParseRateLimitReset(stamp)
	if got == nil || math.Abs(*got-30) > 2 {
		t.Fatalf("got %v", got)
	}
	equal(t, ParseRateLimitReset("2020-01-01T00:00:00Z"), f64(0))
	equal(t, ParseRateLimitReset("2020-01-01t00:00:00z"), f64(0))
	equal(t, ParseRateLimitReset("2020-01-01T00:00:00+02:00"), f64(0))
	got = ParseRateLimitReset(time.Now().UTC().Add(time.Minute).Format("2006-01-02T15:04:05")) // naive = UTC
	if got == nil || math.Abs(*got-60) > 2 {
		t.Fatalf("naive timestamp: %v", got)
	}
}

func TestRateLimitResetGoDurations(t *testing.T) {
	equal(t, ParseRateLimitReset("6m0s"), f64(360))
	equal(t, ParseRateLimitReset("1s"), f64(1))
	equal(t, ParseRateLimitReset("250ms"), f64(0.25))
	equal(t, ParseRateLimitReset("1h2m3.5s"), f64(3723.5))
	equal(t, ParseRateLimitReset("500µs"), f64(0.0005))
}

func TestRateLimitResetPlainSecondsAndGarbage(t *testing.T) {
	equal(t, ParseRateLimitReset("12"), f64(12))
	if ParseRateLimitReset("soon") != nil || ParseRateLimitReset("") != nil || ParseRateLimitReset("  ") != nil {
		t.Fatal("garbage must be rejected")
	}
}

func TestThrottlePacesRequests(t *testing.T) {
	throttle := NewThrottle(6000) // one request every 10 ms
	start := time.Now()
	for i := 0; i < 6; i++ {
		if err := throttle.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 45*time.Millisecond {
		t.Fatalf("5 gaps of 10 ms took only %v", elapsed)
	}
}

func TestThrottleWithoutRPMDoesNotWait(t *testing.T) {
	throttle := NewThrottle(0)
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if err := throttle.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("an unpaced throttle waited")
	}
	for _, rpm := range []float64{-5, math.NaN(), math.Inf(1)} {
		if NewThrottle(rpm).interval != 0 {
			t.Errorf("rpm %v must disable pacing", rpm)
		}
	}
}

func TestThrottlePenaliseParksEveryWaiter(t *testing.T) {
	throttle := NewThrottle(0)
	equal(t, throttle.Penalise(0), 0.0)
	if waited := throttle.Penalise(0.05); waited < 0.04 || waited > 0.06 {
		t.Fatalf("waited %v", waited)
	}
	// A shorter park does not shorten the existing one.
	if waited := throttle.Penalise(0.001); waited < 0.03 {
		t.Fatalf("shorter park shortened the wait: %v", waited)
	}
	start := time.Now()
	if err := throttle.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("Wait ignored the park")
	}
	// Garbage never parks.
	fresh := NewThrottle(0)
	equal(t, fresh.Penalise(math.NaN()), 0.0)
	equal(t, fresh.Penalise(-3), 0.0)
	if fresh.Penalise(math.Inf(1)) <= 0 {
		t.Fatal("infinite park must be clamped, not ignored")
	}
}

func TestThrottleWaitStopsOnCancel(t *testing.T) {
	throttle := NewThrottle(0)
	throttle.Penalise(60)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := throttle.Wait(ctx)
	if !IsCanceled(err) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Wait did not stop")
	}
}
