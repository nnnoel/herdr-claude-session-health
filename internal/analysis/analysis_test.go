package analysis_test

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"claude-session-health/internal/analysis"
	"claude-session-health/internal/config"
	"claude-session-health/internal/history"
)

var t0 = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)

type scenario struct {
	minutes float64
	rssMB   func(m float64) float64
	idle    func(m float64) bool
	skip    func(m float64) bool // true = no sample (monitoring gap)
}

type step struct {
	min   float64
	state analysis.State
	a     analysis.Assessment
	tr    analysis.Tracker
}

// simulate feeds a synthetic RSS curve through the real history, analysis
// and hysteresis pipeline at the default 5 s interval.
func simulate(t *testing.T, cfg config.Config, sc scenario) ([]step, *history.Run) {
	t.Helper()
	p := analysis.ParamsFrom(cfg)
	store := history.NewStore(cfg.RawCapacity(), cfg.MinuteCapacity(), 12)
	var tr analysis.Tracker
	var steps []step
	rng := rand.New(rand.NewSource(1))
	for el := time.Duration(0); el <= time.Duration(sc.minutes*float64(time.Minute)); el += cfg.Interval() {
		m := el.Minutes()
		now := t0.Add(el)
		if sc.skip != nil && sc.skip(m) {
			continue
		}
		noise := (rng.Float64()*2 - 1) * 3 // ±3 MB jitter
		idle := sc.idle != nil && sc.idle(m)
		sample := history.Sample{T: now, RSSKB: uint64(math.Max(1, sc.rssMB(m)+noise) * 1024), Idle: idle}
		if _, err := store.Observe("k", history.Meta{PaneID: "w1:p1"}, history.RunID{PID: 100, Start: t0}, sample); err != nil {
			t.Fatalf("observe: %v", err)
		}
		sess, _ := store.Session("k")
		a := analysis.Evaluate(analysis.Input{Now: now, Samples: sess.Run.Samples(), Buckets: sess.Run.Buckets()}, p)
		tr.Update(a, now, p.Recovery)
		steps = append(steps, step{min: m, state: tr.State, a: a, tr: tr})
	}
	sess, _ := store.Session("k")
	return steps, sess.Run
}

func firstAt(steps []step, states ...analysis.State) (float64, bool) {
	for _, s := range steps {
		for _, want := range states {
			if s.state == want {
				return s.min, true
			}
		}
	}
	return 0, false
}

func everIn(steps []step, states ...analysis.State) bool {
	_, ok := firstAt(steps, states...)
	return ok
}

func last(steps []step) step { return steps[len(steps)-1] }

var (
	growth   = []analysis.State{analysis.SustainedGrowth, analysis.SuspectedLeak}
	warnings = []analysis.State{analysis.SustainedGrowth, analysis.SuspectedLeak, analysis.Elevated}
)

// 1. Stable memory never warns.
func TestStableMemoryNoWarning(t *testing.T) {
	steps, _ := simulate(t, config.Default(), scenario{minutes: 120, rssMB: func(float64) float64 { return 800 }})
	if everIn(steps, warnings...) {
		t.Fatalf("stable memory produced a warning")
	}
	if got := last(steps).state; got != analysis.Healthy {
		t.Errorf("final state = %s, want healthy", got)
	}
	if !strings.Contains(last(steps).tr.Reasons[0], "flat") {
		t.Errorf("reason = %q", last(steps).tr.Reasons)
	}
}

// 1b. High but stable memory is "elevated", never growth or leak.
func TestHighButStableIsElevatedNotLeak(t *testing.T) {
	steps, _ := simulate(t, config.Default(), scenario{minutes: 90, rssMB: func(float64) float64 { return 3000 }})
	if everIn(steps, growth...) {
		t.Fatal("high but stable memory flagged as growth/leak")
	}
	at, ok := firstAt(steps, analysis.Elevated)
	if !ok || at < 5 {
		t.Errorf("elevated at %.1f min (ok=%t); must need 5 min above the threshold", at, ok)
	}
}

// 2. Brief spikes never warn.
func TestBriefSpikeNoWarning(t *testing.T) {
	steps, _ := simulate(t, config.Default(), scenario{minutes: 120, rssMB: func(m float64) float64 {
		switch {
		case m >= 40 && m < 40.5:
			return 3000 // 30 s spike over the warning threshold
		case m >= 70 && m < 72:
			return 1700 // 2 min plateau
		default:
			return 800
		}
	}})
	if everIn(steps, warnings...) {
		at, _ := firstAt(steps, warnings...)
		t.Fatalf("spike produced a warning at %.1f min", at)
	}
}

// 2b. A one-time step up followed by a plateau is not growth.
func TestSingleStepIsNotGrowth(t *testing.T) {
	steps, _ := simulate(t, config.Default(), scenario{minutes: 90, rssMB: func(m float64) float64 {
		if m < 30 {
			return 800
		}
		return 1400
	}})
	if everIn(steps, growth...) {
		t.Fatal("single step flagged as growth")
	}
}

// 2c. Growth that levels off is not sustained.
func TestLevellingOffIsNotGrowth(t *testing.T) {
	steps, _ := simulate(t, config.Default(), scenario{minutes: 90, rssMB: func(m float64) float64 {
		return 800 + 600*(1-math.Exp(-m/4))
	}})
	if everIn(steps, growth...) {
		at, _ := firstAt(steps, growth...)
		t.Fatalf("levelling-off curve flagged at %.1f min", at)
	}
}

// 3. Sustained growth warns only after the criteria are met, then escalates.
func TestSustainedGrowthWarnsOnlyAfterCriteria(t *testing.T) {
	cfg := config.Default()
	steps, _ := simulate(t, cfg, scenario{minutes: 90, rssMB: func(m float64) float64 { return 800 + 15*m }})
	at, ok := firstAt(steps, analysis.SustainedGrowth)
	if !ok {
		t.Fatal("sustained growth never detected")
	}
	minSpan := 0.7 * float64(cfg.GrowthWindowMinutes)
	if at < minSpan || at > float64(cfg.GrowthWindowMinutes)+3 {
		t.Errorf("growth detected at %.1f min; want between %.0f and %d min", at, minSpan, cfg.GrowthWindowMinutes+3)
	}
	for _, s := range steps {
		if s.min < at && (s.state == analysis.SustainedGrowth || s.state == analysis.SuspectedLeak) {
			t.Fatalf("warned at %.1f before criteria", s.min)
		}
	}
	reason := strings.Join(steps[int(at*12)].tr.Reasons, " ")
	for _, want := range []string{"rose", "steadily", "still rising", "min"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q lacks %q", reason, want)
		}
	}
	leakAt, ok := firstAt(steps, analysis.SuspectedLeak)
	if !ok || leakAt < 0.7*float64(cfg.LeakWindowMinutes) {
		t.Errorf("leak at %.1f (ok=%t); needs the %d min window", leakAt, ok, cfg.LeakWindowMinutes)
	}
	if r := last(steps).tr.Reasons[0]; !strings.HasPrefix(r, "possible leak") {
		t.Errorf("leak reason must be hedged: %q", r)
	}
}

// 3a. A transient spike inside a steady leak must not hide the leak.
func TestTransientSpikeDoesNotMaskLeak(t *testing.T) {
	steps, _ := simulate(t, config.Default(), scenario{minutes: 90, rssMB: func(m float64) float64 {
		base := 800 + 8*m
		if m >= 70 && m < 73 {
			return base + 400 // e.g. a test run in a child process
		}
		return base
	}})
	if _, ok := firstAt(steps, analysis.SuspectedLeak); !ok {
		t.Fatal("precondition: leak detected")
	}
	for _, s := range steps {
		if s.min > 74 && s.state != analysis.SuspectedLeak {
			t.Fatalf("leak verdict dropped at %.1f after a transient spike: %s %v", s.min, s.state, s.tr.Reasons)
		}
	}
}

// 3b. Absolute and relative thresholds must both pass.
func TestGrowthNeedsAbsoluteAndRelativeRise(t *testing.T) {
	slow, _ := simulate(t, config.Default(), scenario{minutes: 90, rssMB: func(m float64) float64 { return 800 + 2*m }})
	if everIn(slow, growth...) {
		t.Error("growth below the absolute minimum was flagged")
	}
	bigBase, _ := simulate(t, config.Default(), scenario{minutes: 50, rssMB: func(m float64) float64 { return 6000 + 8*m }})
	if everIn(bigBase, analysis.SustainedGrowth) {
		t.Error("growth below the relative minimum was flagged")
	}
}

// 3c. Steady growth while idle is a suspected leak on the short window.
func TestGrowthWhileIdleIsSuspectedLeak(t *testing.T) {
	steps, _ := simulate(t, config.Default(), scenario{
		minutes: 30, rssMB: func(m float64) float64 { return 800 + 12*m }, idle: func(float64) bool { return true },
	})
	at, ok := firstAt(steps, analysis.SuspectedLeak)
	if !ok || at > 25 {
		t.Fatalf("idle growth leak at %.1f (ok=%t)", at, ok)
	}
	if !strings.Contains(last(steps).tr.Reasons[0], "idle") {
		t.Errorf("reason should mention idle: %q", last(steps).tr.Reasons[0])
	}
}

// 4. Declining memory updates the state promptly instead of waiting out
// the full recovery hold.
func TestDecliningMemoryUpdatesState(t *testing.T) {
	cfg := config.Default()
	steps, _ := simulate(t, cfg, scenario{minutes: 70, rssMB: func(m float64) float64 {
		switch {
		case m < 40:
			return 800 + 15*m // reaches 1400 MB
		case m < 43:
			return 1400 - (m-40)*170 // released over 3 minutes
		default:
			return 890
		}
	}})
	if !everIn(steps, growth...) {
		t.Fatal("precondition: growth should have been detected")
	}
	clearedAt := -1.0
	for _, s := range steps {
		if s.min >= 40 && s.state != analysis.SustainedGrowth && s.state != analysis.SuspectedLeak {
			clearedAt = s.min
			break
		}
	}
	if clearedAt < 0 || clearedAt > 40+cfg.RecoveryMinutes-2 {
		t.Fatalf("state cleared at %.1f; a real decline must not wait out the %.0f min hold", clearedAt, cfg.RecoveryMinutes)
	}
	found := false
	for _, s := range steps {
		if s.min >= 40 && s.min <= 50 && s.a.Declining {
			found = true
		}
	}
	if !found {
		t.Error("assessment never reported the decline")
	}
	if got := last(steps).state; got != analysis.Healthy {
		t.Errorf("final state = %s, want healthy", got)
	}
}

// Recovery without a decline waits for the hysteresis hold.
func TestRecoveryHysteresis(t *testing.T) {
	cfg := config.Default()
	steps, _ := simulate(t, cfg, scenario{minutes: 80, rssMB: func(m float64) float64 {
		if m < 35 {
			return 800 + 15*m
		}
		return 800 + 15*35 // plateau, no release
	}})
	growthAt, ok := firstAt(steps, analysis.SustainedGrowth)
	if !ok {
		t.Fatal("precondition: growth")
	}
	clearedAt := -1.0
	sawHold := false
	for _, s := range steps {
		if s.min <= growthAt {
			continue
		}
		if strings.HasPrefix(s.tr.Note, "recovering") {
			sawHold = true
		}
		if s.state != analysis.SustainedGrowth && s.state != analysis.SuspectedLeak {
			clearedAt = s.min
			break
		}
	}
	if !sawHold {
		t.Error("expected a 'recovering' hold note")
	}
	if clearedAt < 35+cfg.RecoveryMinutes {
		t.Errorf("cleared at %.1f, before the %.0f min recovery hold after growth stopped", clearedAt, cfg.RecoveryMinutes)
	}
}

// 5. Missing samples are gaps, never zeros.
func TestMissingSamplesAreNotZero(t *testing.T) {
	steps, run := simulate(t, config.Default(), scenario{
		minutes: 45,
		rssMB:   func(m float64) float64 { return 900 },
		skip:    func(m float64) bool { return m > 5 && m < 35 },
	})
	for _, b := range run.Buckets() {
		if b.MedKB < 800*1024 || b.MinKB < 800*1024 {
			t.Fatalf("bucket %v contains a zero-like value: %+v", b.T, b)
		}
	}
	if run.PeakKB < 890*1024 || run.BaselineKB < 890*1024 {
		t.Errorf("peak %d / baseline %d corrupted by the gap", run.PeakKB, run.BaselineKB)
	}
	for _, s := range steps {
		if s.min > 35 && s.min < 40 && s.state == analysis.Healthy && s.a.Short.Enough {
			t.Errorf("at %.1f the 20 min window is mostly a gap but was treated as enough data", s.min)
		}
		if s.state == analysis.SustainedGrowth || s.state == analysis.SuspectedLeak {
			t.Fatalf("gap produced %s at %.1f", s.state, s.min)
		}
	}
}

// 5b. Time above a threshold never counts a gap.
func TestAboveThresholdIgnoresGaps(t *testing.T) {
	steps, _ := simulate(t, config.Default(), scenario{
		minutes: 27, // samples resume at 20 min; 5 continuous minutes end at 25
		rssMB:   func(float64) float64 { return 3000 },
		skip:    func(m float64) bool { return m > 1 && m < 20 },
	})
	for _, s := range steps {
		if s.min >= 20 && s.min < 23 && s.state == analysis.Elevated {
			t.Fatalf("elevated at %.1f: the gap was counted as time above the threshold (%s)", s.min, s.a.AboveWarnFor)
		}
	}
	if s := last(steps); s.state != analysis.Elevated {
		t.Errorf("after 5 continuous minutes above the threshold the state should be elevated, got %s", s.state)
	}
}

func TestTrackerHoldsVerdictThroughDataGap(t *testing.T) {
	var tr analysis.Tracker
	tr.Update(analysis.Assessment{State: analysis.SustainedGrowth, Reasons: []string{"grew"}}, t0, time.Minute)
	if _, changed := tr.Update(analysis.Assessment{State: analysis.InsufficientData, Reasons: []string{"gap"}}, t0.Add(time.Hour), time.Minute); changed {
		t.Fatal("a data gap must not count as recovery")
	}
	if tr.State != analysis.SustainedGrowth || !strings.Contains(tr.Note, "holding") {
		t.Errorf("state %s note %q", tr.State, tr.Note)
	}
}

func TestDataGapRestartsRecoveryClock(t *testing.T) {
	var tr analysis.Tracker
	rec := 10 * time.Minute
	tr.Update(analysis.Assessment{State: analysis.SuspectedLeak}, t0, rec)
	tr.Update(analysis.Assessment{State: analysis.Healthy}, t0.Add(time.Minute), rec)
	tr.Update(analysis.Assessment{State: analysis.InsufficientData}, t0.Add(2*time.Minute), rec)
	if _, changed := tr.Update(analysis.Assessment{State: analysis.Healthy}, t0.Add(40*time.Minute), rec); changed {
		t.Fatal("time spent in a data gap was counted as recovery")
	}
	if _, changed := tr.Update(analysis.Assessment{State: analysis.Healthy}, t0.Add(50*time.Minute), rec); !changed {
		t.Error("a full recovery period after the gap should de-escalate")
	}
}

func TestTheilSenResistsOutliers(t *testing.T) {
	xs := []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	ys := []float64{0, 1, 2, 3, 400, 5, 6, 7, 8, 9}
	if got := analysis.TheilSen(xs, ys); math.Abs(got-1) > 0.01 {
		t.Errorf("slope = %f, want 1", got)
	}
	if analysis.TheilSen(nil, nil) != 0 || analysis.TheilSen([]float64{1}, []float64{1}) != 0 {
		t.Error("degenerate inputs should give 0")
	}
}

func TestEvaluateWithoutSamples(t *testing.T) {
	a := analysis.Evaluate(analysis.Input{Now: t0}, analysis.ParamsFrom(config.Default()))
	if a.State != analysis.InsufficientData || a.Trend != analysis.Unknown {
		t.Errorf("got %s/%s", a.State, a.Trend)
	}
}
