// Package systemhealth judges system-wide memory health from kernel pressure
// and swap growth. Low free memory alone is not a problem on macOS (the
// kernel uses spare RAM as cache), so free memory is reported but never
// triggers a warning by itself. System metrics are kept separate from
// session RSS totals.
package systemhealth

import (
	"fmt"
	"time"

	"claude-session-health/internal/analysis"
	"claude-session-health/internal/collect"
	"claude-session-health/internal/ring"
)

// Status is the overall system memory verdict.
type Status string

// System statuses.
const (
	OK       Status = "ok"
	Warn     Status = "warn"
	Critical Status = "critical"
	Unknown  Status = "unknown"
)

// Params are the system thresholds.
type Params struct {
	Sustain         time.Duration
	SwapGrowthBytes uint64
	SwapWindow      time.Duration
}

// Assessment is the system verdict for one sample.
type Assessment struct {
	Sample          collect.SystemSample `json:"sample"`
	Status          Status               `json:"status"`
	Reasons         []string             `json:"reasons"`
	PressureFor     time.Duration        `json:"pressure_for_ns"`
	Persistent      bool                 `json:"persistent_pressure"`
	SwapKnown       bool                 `json:"swap_known"`
	SwapGrowthBytes int64                `json:"swap_growth_bytes"`
	SwapCovered     time.Duration        `json:"swap_window_covered_ns"`
	RapidSwap       bool                 `json:"rapid_swap"`
}

type swapPoint struct {
	t    time.Time
	used uint64
}

// Tracker remembers pressure duration and recent swap usage.
type Tracker struct {
	since time.Time
	swap  *ring.Ring[swapPoint]
}

// NewTracker bounds swap history to the points one window can hold.
func NewTracker(window, interval time.Duration) *Tracker {
	capacity := 8
	if interval > 0 {
		capacity = int(window/interval) + 8
	}
	return &Tracker{swap: ring.New[swapPoint](capacity)}
}

// Update folds in one sample.
func (t *Tracker) Update(s collect.SystemSample, p Params) Assessment {
	a := Assessment{Sample: s, Status: OK}
	now := s.TakenAt

	if s.Pressure.Rank() >= collect.PressureWarn.Rank() {
		if t.since.IsZero() {
			t.since = now
		}
		a.PressureFor = now.Sub(t.since)
		a.Persistent = a.PressureFor >= p.Sustain
	} else {
		t.since = time.Time{}
	}

	if s.SwapUsedBytes != nil {
		t.swap.Push(swapPoint{t: now, used: *s.SwapUsedBytes})
		from := now.Add(-p.SwapWindow)
		for i := 0; i < t.swap.Len(); i++ {
			pt := t.swap.At(i)
			if pt.t.Before(from) {
				continue
			}
			a.SwapKnown = true
			a.SwapGrowthBytes = int64(*s.SwapUsedBytes) - int64(pt.used)
			a.SwapCovered = now.Sub(pt.t)
			a.RapidSwap = a.SwapCovered >= p.SwapWindow/2 && a.SwapGrowthBytes >= int64(p.SwapGrowthBytes)
			break
		}
	}

	switch s.Pressure {
	case collect.PressureUnknown:
		a.Reasons = append(a.Reasons, "kernel memory pressure is unavailable")
	case collect.PressureNormal:
		a.Reasons = append(a.Reasons, "kernel memory pressure is normal")
	default:
		a.Reasons = append(a.Reasons, fmt.Sprintf("kernel memory pressure is %s for %s", s.Pressure, analysis.Dur(a.PressureFor)))
	}
	if a.RapidSwap {
		a.Reasons = append(a.Reasons, fmt.Sprintf("swap grew %s in %s",
			analysis.MB(float64(a.SwapGrowthBytes)/1024), analysis.Dur(a.SwapCovered)))
	}
	if s.Pressure == collect.PressureNormal && s.FreePercent != nil && *s.FreePercent < 15 {
		a.Reasons = append(a.Reasons, "free memory is low, which is normal on macOS while pressure stays normal")
	}

	switch {
	case s.Pressure == collect.PressureUnknown && s.SwapUsedBytes == nil:
		a.Status = Unknown
	case a.Persistent && s.Pressure == collect.PressureCritical:
		a.Status = Critical
	case a.Persistent || a.RapidSwap:
		a.Status = Warn
	}
	return a
}
