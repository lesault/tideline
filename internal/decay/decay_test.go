package decay

import (
	"testing"
	"time"
)

// base is a fixed reference point so tests are deterministic.
var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestAssess(t *testing.T) {
	created := base
	// A 10-day TTL window: expires 10 days after creation.
	expires := base.Add(10 * 24 * time.Hour)

	cases := []struct {
		name string
		now  time.Time
		want Level
	}{
		{"just captured is fresh", created, Fresh},
		{"day 4 still fresh", created.Add(4 * 24 * time.Hour), Fresh},
		{"day 5 (halfway) is aging", created.Add(5 * 24 * time.Hour), Aging},
		{"day 7 is aging", created.Add(7 * 24 * time.Hour), Aging},
		{"day 8 (80%) is due soon", created.Add(8 * 24 * time.Hour), DueSoon},
		{"day 9 is due soon", created.Add(9 * 24 * time.Hour), DueSoon},
		{"exactly at expiry is expired", expires, Expired},
		{"past expiry is expired", expires.Add(time.Hour), Expired},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Assess(created, expires, tc.now)
			if got != tc.want {
				t.Fatalf("Assess(now=%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

func TestAssessDegenerateWindow(t *testing.T) {
	// expires == created: any now at/after that instant is expired.
	if got := Assess(base, base, base); got != Expired {
		t.Fatalf("zero-length window at base = %v, want Expired", got)
	}
	if got := Assess(base, base, base.Add(-time.Second)); got != Fresh {
		t.Fatalf("zero-length window before base = %v, want Fresh", got)
	}
}

func TestLifeRemaining(t *testing.T) {
	created := base
	expires := base.Add(10 * 24 * time.Hour)

	cases := []struct {
		name string
		now  time.Time
		want float64
	}{
		{"at creation is full", created, 1.0},
		{"before creation clamps to full", created.Add(-time.Hour), 1.0},
		{"midpoint is half", created.Add(5 * 24 * time.Hour), 0.5},
		{"day 8 leaves a fifth", created.Add(8 * 24 * time.Hour), 0.2},
		{"at expiry is empty", expires, 0.0},
		{"past expiry clamps to empty", expires.Add(time.Hour), 0.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LifeRemaining(created, expires, tc.now)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("LifeRemaining(now=%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

func TestLifeRemainingDegenerateWindow(t *testing.T) {
	// Non-positive window: before the instant -> full, at/after -> empty.
	if got := LifeRemaining(base, base, base); got != 0.0 {
		t.Fatalf("zero-length window at base = %v, want 0", got)
	}
	if got := LifeRemaining(base, base, base.Add(-time.Second)); got != 1.0 {
		t.Fatalf("zero-length window before base = %v, want 1", got)
	}
	// Inverted window (expires before created) is still sane and clamped.
	if got := LifeRemaining(base, base.Add(-time.Hour), base); got != 0.0 {
		t.Fatalf("inverted window = %v, want 0", got)
	}
}

func TestBarnacleCount(t *testing.T) {
	cases := []struct {
		level Level
		want  int
	}{
		{Fresh, 0},
		{Aging, 7},
		{DueSoon, 14},
		{Expired, 24},
	}
	for _, tc := range cases {
		if got := BarnacleCount(tc.level); got != tc.want {
			t.Fatalf("BarnacleCount(%v) = %d, want %d", tc.level, got, tc.want)
		}
	}
}

func TestExpired(t *testing.T) {
	expires := base.Add(24 * time.Hour)
	if IsExpired(expires, base) {
		t.Fatal("not yet at expiry should not be expired")
	}
	if !IsExpired(expires, expires) {
		t.Fatal("at expiry instant should be expired")
	}
	if !IsExpired(expires, expires.Add(time.Minute)) {
		t.Fatal("past expiry should be expired")
	}
}
