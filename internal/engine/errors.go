// Package engine is the Go port of the subtitle translation engine
// (srt_translate.py) and the subtitle format adapters (subtitle_formats.py).
//
// The pipeline is: ParseSubtitle -> SegmentCue -> TranslateSegments ->
// RebuildCues -> Document.ToBytes. Translate wires those steps together;
// the individual stages are exported for the backend and for tests.
package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Python exception class names, so the backend can keep the job error format
// ("<ClassName>: message") and the submitter-visible error classification of
// the Python application.
const (
	TypeSubtitleFormatError    = "SubtitleFormatError"
	TypeTranslationError       = "TranslationError"
	TypeFatalTranslationError  = "FatalTranslationError"
	TypeRateLimitError         = "RateLimitError"
	TypeTranslationCanceled    = "TranslationCanceled"
	typeUnknownErrorTypeString = "Error"
)

// ErrCanceled is matched (errors.Is) by every error that reports a stopped
// translation. The underlying context error is wrapped as well, so
// errors.Is(err, context.Canceled) also holds when the caller canceled.
var ErrCanceled = errors.New("translation canceled")

type canceledError struct{ cause error }

func (e *canceledError) Error() string {
	if e.cause == nil {
		return ErrCanceled.Error()
	}
	return ErrCanceled.Error() + ": " + e.cause.Error()
}

func (e *canceledError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrCanceled}
	}
	return []error{ErrCanceled, e.cause}
}

func (e *canceledError) ErrorType() string { return TypeTranslationCanceled }

// canceled returns the cancellation error for a finished context.
func canceled(ctx context.Context) error {
	return &canceledError{cause: context.Cause(ctx)}
}

// SubtitleFormatError reports a file that cannot be decoded or parsed. Its
// message is safe to show to the submitter.
type SubtitleFormatError struct {
	Msg string
	Err error
}

func (e *SubtitleFormatError) Error() string     { return e.Msg }
func (e *SubtitleFormatError) Unwrap() error     { return e.Err }
func (e *SubtitleFormatError) ErrorType() string { return TypeSubtitleFormatError }

func formatErrorf(format string, args ...any) *SubtitleFormatError {
	return &SubtitleFormatError{Msg: fmt.Sprintf(format, args...)}
}

// TranslationError is a transient failure worth retrying (5xx, network blips,
// malformed provider replies).
type TranslationError struct {
	Msg string
	Err error
}

func (e *TranslationError) Error() string     { return e.Msg }
func (e *TranslationError) Unwrap() error     { return e.Err }
func (e *TranslationError) ErrorType() string { return TypeTranslationError }

func transientf(format string, args ...any) *TranslationError {
	return &TranslationError{Msg: fmt.Sprintf(format, args...)}
}

// FatalTranslationError is not worth retrying: bad key, forbidden, bad
// request. It aborts the whole run.
type FatalTranslationError struct {
	Msg string
}

func (e *FatalTranslationError) Error() string     { return e.Msg }
func (e *FatalTranslationError) ErrorType() string { return TypeFatalTranslationError }

func fatalf(format string, args ...any) *FatalTranslationError {
	return &FatalTranslationError{Msg: fmt.Sprintf(format, args...)}
}

// RateLimitError is a 429 or 529. It is retried on its own budget and never
// fanned out to per-line requests. RetryAfter is the delay in seconds the
// server asked for, or nil when it gave none; it may be NaN or infinite when
// a caller constructs one by hand, and the driver then gives up.
type RateLimitError struct {
	Msg        string
	RetryAfter *float64
}

func (e *RateLimitError) Error() string     { return e.Msg }
func (e *RateLimitError) ErrorType() string { return TypeRateLimitError }

func rateLimitf(retryAfter *float64, format string, args ...any) *RateLimitError {
	return &RateLimitError{Msg: fmt.Sprintf(format, args...), RetryAfter: retryAfter}
}

// ErrorTypeName returns the Python-style class name of an engine error, or
// "Error" for errors from elsewhere.
func ErrorTypeName(err error) string {
	var typed interface{ ErrorType() string }
	if errors.As(err, &typed) {
		return typed.ErrorType()
	}
	return typeUnknownErrorTypeString
}

// IsCanceled reports whether err stems from a canceled translation.
func IsCanceled(err error) bool {
	return errors.Is(err, ErrCanceled) || errors.Is(err, context.Canceled)
}

// IsFatal reports whether err is a FatalTranslationError.
func IsFatal(err error) bool {
	var fatal *FatalTranslationError
	return errors.As(err, &fatal)
}

func asRateLimit(err error) (*RateLimitError, bool) {
	var limited *RateLimitError
	if errors.As(err, &limited) {
		return limited, true
	}
	return nil, false
}

func isTransient(err error) bool {
	var transient *TranslationError
	return errors.As(err, &transient)
}

// truncateRunes returns at most n code points of s.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

func pythonFloat(v float64) string {
	switch {
	case v != v:
		return "nan"
	case v > 1e308:
		return "inf"
	case v < -1e308:
		return "-inf"
	}
	return fmt.Sprintf("%.0f", v)
}

// joinLines is strings.Join, used where the Python code joins with spaces.
func joinSpace(parts []string) string { return strings.Join(parts, " ") }
