package server

import (
	"sync"
	"time"
)

// rateLimiter is a simple in-memory sliding-window limiter keyed by a string
// (e.g. client IP). Suitable for a single-instance self-hosted app.
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
	now    func() time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		hits:   make(map[string][]time.Time),
		max:    max,
		window: window,
		now:    time.Now,
	}
}

// allow records an attempt for key and reports whether it is within the limit.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, l.now())
	return true
}
