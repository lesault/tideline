// Package decay holds the pure core of Tideline's TTL mechanic: given when a
// link was captured and when it expires, it reports how urgent the link has
// become. It deliberately takes "now" as an argument so the logic is
// deterministic and testable — no wall clock inside.
package decay

import "time"

// Level is an escalating urgency bucket used for sorting the inbox, painting
// the nudge badge, and deciding what belongs in the due feed.
type Level int

const (
	Fresh   Level = iota // plenty of time left
	Aging                // past the halfway point of its life
	DueSoon              // in the final stretch before expiry
	Expired              // TTL elapsed; belongs in the graveyard
)

func (l Level) String() string {
	switch l {
	case Fresh:
		return "fresh"
	case Aging:
		return "aging"
	case DueSoon:
		return "due_soon"
	case Expired:
		return "expired"
	default:
		return "unknown"
	}
}

// Thresholds are fractions of a link's total lifetime. They are TTL-length
// agnostic: a 2-day link and a 30-day link escalate through the same stages.
const (
	agingFraction   = 0.5
	dueSoonFraction = 0.8
)

// Assess reports the urgency Level of a link captured at created, expiring at
// expires, evaluated at now. A link at or past its expiry is always Expired,
// even if the window is degenerate (expires <= created).
func Assess(created, expires, now time.Time) Level {
	if IsExpired(expires, now) {
		return Expired
	}
	life := expires.Sub(created)
	if life <= 0 {
		// Not yet expired (handled above) but no positive lifetime to grade.
		return Fresh
	}
	elapsed := now.Sub(created).Seconds() / life.Seconds()
	switch {
	case elapsed >= dueSoonFraction:
		return DueSoon
	case elapsed >= agingFraction:
		return Aging
	default:
		return Fresh
	}
}

// IsExpired reports whether now is at or past the expiry instant.
func IsExpired(expires, now time.Time) bool {
	return !now.Before(expires)
}
