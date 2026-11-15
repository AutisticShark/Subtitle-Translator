package app

import (
	"sync"
	"time"
)

const attemptPruneInterval = 60 * time.Second

// attemptLimiter is a process-local sliding-window limit per client address.
// reserve checks and records an attempt in one locked step, so parallel
// requests cannot all pass the check before any is recorded; expired
// addresses are pruned periodically so the map cannot grow without bound.
type attemptLimiter struct {
	limit     int
	window    time.Duration
	now       func() time.Time
	mu        sync.Mutex
	attempts  map[string][]time.Time
	nextPrune time.Time
}

func newAttemptLimiter(limit int, window time.Duration, now func() time.Time) *attemptLimiter {
	return &attemptLimiter{limit: limit, window: window, now: now, attempts: map[string][]time.Time{}}
}

func (l *attemptLimiter) recent(key string, current time.Time) []time.Time {
	cutoff := current.Add(-l.window)
	if l.nextPrune.IsZero() || !current.Before(l.nextPrune) {
		l.nextPrune = current.Add(attemptPruneInterval)
		for address, values := range l.attempts {
			kept := keepAfter(values, cutoff)
			if len(kept) == 0 {
				delete(l.attempts, address)
			} else {
				l.attempts[address] = kept
			}
		}
	}
	kept := keepAfter(l.attempts[key], cutoff)
	if len(kept) == 0 {
		delete(l.attempts, key)
	} else {
		l.attempts[key] = kept
	}
	return kept
}

func keepAfter(values []time.Time, cutoff time.Time) []time.Time {
	kept := values[:0:0]
	for _, value := range values {
		if value.After(cutoff) {
			kept = append(kept, value)
		}
	}
	return kept
}

func (l *attemptLimiter) limited(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, l.now())) >= l.limit
}

// reserve records an attempt and returns its marker, or false over the limit.
func (l *attemptLimiter) reserve(key string) (time.Time, bool) {
	current := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.recent(key, current)
	if len(recent) >= l.limit {
		return time.Time{}, false
	}
	l.attempts[key] = append(recent, current)
	return current, true
}

// release forgets a reserved attempt that turned out not to count (a successful login).
func (l *attemptLimiter) release(key string, marker time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	values := l.attempts[key]
	for i, value := range values {
		if value.Equal(marker) {
			values = append(values[:i:i], values[i+1:]...)
			break
		}
	}
	if len(values) == 0 {
		delete(l.attempts, key)
	} else {
		l.attempts[key] = values
	}
}
