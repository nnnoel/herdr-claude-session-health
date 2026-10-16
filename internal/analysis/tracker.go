package analysis

import (
	"fmt"
	"time"
)

// Tracker applies hysteresis to candidate verdicts. Escalation is immediate
// (the candidate already required sustained evidence). De-escalation needs
// the lower verdict to persist for the recovery period, except when memory
// is clearly being released. A data gap never counts as recovery.
type Tracker struct {
	State   State
	Since   time.Time
	Reasons []string
	Note    string // why the displayed state is being held, if it is

	pendingSince time.Time
}

// Transition is a change of displayed state.
type Transition struct {
	From    State
	To      State
	At      time.Time
	Reasons []string
}

// Update feeds one assessment and reports a state change, if any.
func (t *Tracker) Update(a Assessment, now time.Time, recovery time.Duration) (Transition, bool) {
	cand := a.State
	t.Note = ""
	switch {
	case t.State == "" || t.State == InsufficientData:
		if cand == t.State {
			t.Reasons = a.Reasons
			return Transition{}, false
		}
		return t.set(cand, a, now)
	case cand == t.State:
		t.pendingSince = time.Time{}
		t.Reasons = a.Reasons
		return Transition{}, false
	case cand == InsufficientData:
		// A gap is neither escalation nor recovery: restart the recovery clock.
		t.pendingSince = time.Time{}
		t.Note = "holding last verdict: " + first(a.Reasons)
		return Transition{}, false
	case cand.Severity() > t.State.Severity():
		return t.set(cand, a, now)
	case a.Declining && (t.State == SustainedGrowth || t.State == SuspectedLeak):
		return t.set(cand, a, now)
	default:
		if t.pendingSince.IsZero() {
			t.pendingSince = now
		}
		held := now.Sub(t.pendingSince)
		if held >= recovery {
			return t.set(cand, a, now)
		}
		t.Note = fmt.Sprintf("recovering: criteria clear for %s of %s", Dur(held), Dur(recovery))
		return Transition{}, false
	}
}

func (t *Tracker) set(to State, a Assessment, now time.Time) (Transition, bool) {
	tr := Transition{From: t.State, To: to, At: now, Reasons: a.Reasons}
	t.State, t.Since, t.Reasons, t.pendingSince, t.Note = to, now, a.Reasons, time.Time{}, ""
	return tr, true
}

// Reset forgets all state (used when a session's process restarts).
func (t *Tracker) Reset() { *t = Tracker{} }

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
