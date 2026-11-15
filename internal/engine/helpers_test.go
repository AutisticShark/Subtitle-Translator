package engine

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	cueStart = "00:00:01,000"
	cueEnd   = "00:00:02,000"
)

func newCue(dialect string, lines ...string) Cue {
	return Cue{Index: 1, Start: cueStart, End: cueEnd, Lines: lines, Dialect: dialect}
}

func equal(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func mustParse(t *testing.T, source, extension string) *Document {
	t.Helper()
	doc, err := ParseSubtitle([]byte(source), extension, "")
	if err != nil {
		t.Fatalf("ParseSubtitle: %v", err)
	}
	return doc
}

func mustBytes(t *testing.T, doc *Document) []byte {
	t.Helper()
	out, err := doc.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}
	return out
}

func mustRender(t *testing.T, doc *Document) string {
	t.Helper()
	out, err := doc.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out
}

// providerFunc adapts a simple per-batch function to Provider.
func providerFunc(f func(texts []string) ([]string, error)) Provider {
	return func(_ context.Context, texts []string, _, _ string) ([]string, error) {
		return f(texts)
	}
}

// recorder wraps a transform into a Provider that records every batch.
type recorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recorder) provider(transform func(string) string) Provider {
	return providerFunc(func(texts []string) ([]string, error) {
		r.mu.Lock()
		r.calls = append(r.calls, append([]string(nil), texts...))
		r.mu.Unlock()
		out := make([]string, len(texts))
		for i, text := range texts {
			out[i] = transform(text)
		}
		return out, nil
	})
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var flat []string
	for _, call := range r.calls {
		flat = append(flat, call...)
	}
	return flat
}

func identity(s string) string { return s }

// noDelay makes retry backoff instantaneous.
func noDelay(int) time.Duration { return 0 }

// testOptions are driver options with the single-attempt budgets the Python
// tests default to.
func testOptions() DriverOptions {
	return DriverOptions{BatchSize: 20, Workers: 1, Retries: 1, RateRetries: 1, RetryDelay: noDelay}
}

func segmentsFor(cues ...Cue) []Segment { return SegmentDocument(cues) }

func plainSegments(texts ...string) []Segment {
	segs := make([]Segment, len(texts))
	for i, text := range texts {
		segs[i] = Segment{CueIndex: i, Text: text}
	}
	return segs
}

func runTranslation(t *testing.T, segs []Segment, provider Provider, opt DriverOptions, cache *Cache) ([]string, error) {
	t.Helper()
	return TranslateSegments(context.Background(), segs, provider, "es", "English", opt, cache)
}

// translateDocument runs a document through segmenting, the provider and
// rebuilding, like Translate does, and returns the rendered bytes.
func translateDocument(t *testing.T, doc *Document, provider Provider, target string, width float64, maxLines int) []byte {
	t.Helper()
	segs := SegmentDocument(doc.Cues)
	out, err := TranslateSegments(context.Background(), segs, provider, target, "English", testOptions(), nil)
	if err != nil {
		t.Fatalf("TranslateSegments: %v", err)
	}
	cues, err := RebuildCues(doc.Cues, segs, out, target, width, maxLines)
	if err != nil {
		t.Fatalf("RebuildCues: %v", err)
	}
	return mustBytes(t, doc.CloneWithCues(cues))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func cannedResponse(status int, body string, headers ...string) *http.Response {
	resp := &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	for i := 0; i+1 < len(headers); i += 2 {
		resp.Header.Set(headers[i], headers[i+1])
	}
	return resp
}

func contains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%q does not contain %q", haystack, needle)
	}
}

func notContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%q unexpectedly contains %q", haystack, needle)
	}
}
