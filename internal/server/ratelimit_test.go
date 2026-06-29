package server

import (
	"testing"
	"time"
)

func TestRateLimiterAllowsUpToMaxThenDenies(t *testing.T) {
	l := newRateLimiter(3, time.Minute)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return base }

	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("4th request within the window should be denied")
	}
}

func TestRateLimiterIsPerKey(t *testing.T) {
	l := newRateLimiter(1, time.Minute)
	if !l.allow("a") {
		t.Fatal("first for a should pass")
	}
	if l.allow("a") {
		t.Fatal("second for a should be denied")
	}
	if !l.allow("b") {
		t.Fatal("a different key has its own budget")
	}
}

func TestRateLimiterWindowResets(t *testing.T) {
	l := newRateLimiter(2, time.Minute)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return base }
	l.allow("k")
	l.allow("k")
	if l.allow("k") {
		t.Fatal("over limit within window")
	}
	l.now = func() time.Time { return base.Add(2 * time.Minute) }
	if !l.allow("k") {
		t.Fatal("after the window elapses, requests are allowed again")
	}
}
