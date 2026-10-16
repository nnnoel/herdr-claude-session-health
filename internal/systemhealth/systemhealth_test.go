package systemhealth

import (
	"strings"
	"testing"
	"time"

	"claude-session-health/internal/collect"
)

var t0 = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func sample(sec int, p collect.Pressure, swapMB uint64, freePct int) collect.SystemSample {
	return collect.SystemSample{
		TakenAt: t0.Add(time.Duration(sec) * time.Second), Pressure: p,
		SwapUsedBytes: ptr(swapMB << 20), FreePercent: ptr(freePct),
	}
}

var params = Params{Sustain: 2 * time.Minute, SwapGrowthBytes: 1 << 30, SwapWindow: 10 * time.Minute}

func TestLowFreeMemoryAloneIsNotAWarning(t *testing.T) {
	tr := NewTracker(params.SwapWindow, 5*time.Second)
	var a Assessment
	for i := 0; i < 120; i++ {
		a = tr.Update(sample(i*5, collect.PressureNormal, 100, 3), params)
	}
	if a.Status != OK {
		t.Errorf("status %s for low free memory with normal pressure", a.Status)
	}
	if !strings.Contains(strings.Join(a.Reasons, ";"), "normal on macOS") {
		t.Errorf("reasons should explain low free memory: %v", a.Reasons)
	}
}

func TestPressureMustPersist(t *testing.T) {
	tr := NewTracker(params.SwapWindow, 5*time.Second)
	a := tr.Update(sample(0, collect.PressureWarn, 100, 50), params)
	if a.Status != OK || a.Persistent {
		t.Fatalf("a single warn sample escalated: %+v", a)
	}
	for i := 1; i <= 24; i++ {
		a = tr.Update(sample(i*5, collect.PressureWarn, 100, 50), params)
	}
	if a.Status != Warn || !a.Persistent || a.PressureFor != 2*time.Minute {
		t.Errorf("after 2 min: %+v", a)
	}
	a = tr.Update(sample(125, collect.PressureCritical, 100, 50), params)
	if a.Status != Critical {
		t.Errorf("persistent pressure now critical: %s", a.Status)
	}
	a = tr.Update(sample(130, collect.PressureNormal, 100, 50), params)
	if a.Status != OK || a.Persistent {
		t.Errorf("pressure back to normal: %+v", a)
	}
}

func TestRapidSwapGrowth(t *testing.T) {
	tr := NewTracker(params.SwapWindow, 5*time.Second)
	var a Assessment
	for i := 0; i <= 72; i++ { // 6 minutes, +20 MB per sample = 1.44 GB
		a = tr.Update(sample(i*5, collect.PressureNormal, uint64(100+20*i), 50), params)
	}
	if !a.RapidSwap || a.Status != Warn {
		t.Errorf("rapid swap not detected: %+v", a)
	}
	slow := NewTracker(params.SwapWindow, 5*time.Second)
	for i := 0; i <= 120; i++ {
		a = slow.Update(sample(i*5, collect.PressureNormal, uint64(100+i), 50), params)
	}
	if a.RapidSwap {
		t.Errorf("slow swap flagged: %+v", a)
	}
}

func TestUnavailableMetricsReportedUnknown(t *testing.T) {
	tr := NewTracker(params.SwapWindow, 5*time.Second)
	a := tr.Update(collect.SystemSample{TakenAt: t0, Pressure: collect.PressureUnknown}, params)
	if a.Status != Unknown || a.SwapKnown {
		t.Errorf("unavailable metrics: %+v", a)
	}
}
