package app

import (
	"strings"
	"time"
)

// isoSeconds formats like Python's datetime.isoformat(timespec="seconds") for an
// aware UTC value ("2026-11-15T08:00:00+00:00"), the format of every stored timestamp.
func isoSeconds(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05") + "+00:00"
}

var timestampLayouts = []string{
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04",
	"2006-01-02",
}

// parseTimestamp mirrors timeutil.parse_timestamp: ISO 8601 text, naive values
// read as UTC, and nil for anything unparsable.
func parseTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range timestampLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}
