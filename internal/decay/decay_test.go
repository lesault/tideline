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
