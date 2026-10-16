// Package analysis turns a session's memory history into a health verdict.
//
// The heuristics require sustained evidence. One snapshot, a brief spike, a
// one-time step up, or high-but-stable usage never produce a growth or leak
// verdict. Growth is measured on one-minute median buckets (spike resistant)
// with a Theil–Sen slope (outlier resistant), and must pass absolute and
// relative thresholds, be steady, and still be rising at the end of the
// window. A verdict is a suspicion, never a certainty.
package analysis

import (
	"fmt"
	"math"
	"sort"
	"time"

	"claude-session-health/internal/config"
	"claude-session-health/internal/history"
)

// State is a session health verdict.
type State string

// Health states.
const (
	Healthy          State = "healthy"
	Elevated         State = "elevated"
	SustainedGrowth  State = "sustained_growth"
	SuspectedLeak    State = "suspected_leak"
	InsufficientData State = "insufficient_data"
)

// Severity orders states for sorting and escalation.
func (s State) Severity() int {
	switch s {
	case SuspectedLeak:
		return 4
	case SustainedGrowth:
		return 3
	case Elevated:
		return 2
	case InsufficientData:
		return 1
	default:
		return 0
	}
}

// Label is the human-readable state name.
func (s State) Label() string {
	switch s {
	case Healthy:
		return "Healthy"
	case Elevated:
		return "Elevated memory"
	case SustainedGrowth:
		return "Sustained growth"
	case SuspectedLeak:
		return "Suspected leak"
	case InsufficientData:
		return "Insufficient data"
	default:
		return string(s)
	}
}

// Trend is the short-window direction of RSS.
type Trend string

// Trend directions.
const (
	Rising  Trend = "rising"
	Falling Trend = "falling"
	Flat    Trend = "flat"
	Unknown Trend = "unknown"
)

// Params are the analysis thresholds.
type Params struct {
	Interval         time.Duration
	WarnKB           uint64
	CriticalKB       uint64
	ElevatedMin      time.Duration
	GrowthWindow     time.Duration
	LeakWindow       time.Duration
	MinGrowthKB      float64
	MinGrowthPct     float64
	MinUpFraction    float64
	MinCoverage      float64
	MaxStepShare     float64
	IdleLeakFraction float64
	Recovery         time.Duration
}

// ParamsFrom derives analysis thresholds from validated config.
func ParamsFrom(c config.Config) Params {
	return Params{
		Interval:         c.Interval(),
		WarnKB:           uint64(c.WarnRSSMB) * 1024,
		CriticalKB:       uint64(c.CriticalRSSMB) * 1024,
		ElevatedMin:      config.Minutes(c.ElevatedMinMinutes),
		GrowthWindow:     time.Duration(c.GrowthWindowMinutes) * time.Minute,
		LeakWindow:       time.Duration(c.LeakWindowMinutes) * time.Minute,
		MinGrowthKB:      float64(c.MinGrowthMB) * 1024,
		MinGrowthPct:     c.MinGrowthPercent,
		MinUpFraction:    c.MinUpFraction,
		MinCoverage:      0.7,
		MaxStepShare:     0.5,
		IdleLeakFraction: 0.8,
		Recovery:         config.Minutes(c.RecoveryMinutes),
	}
}

// Window is the growth analysis over one time window.
type Window struct {
	Minutes           int     `json:"window_min"`
	Points            int     `json:"points"`
	Expected          int     `json:"expected"`
	Coverage          float64 `json:"coverage"`
	Enough            bool    `json:"enough"`
	StartKB           float64 `json:"start_kb"`
	EndKB             float64 `json:"end_kb"`
	RiseKB            float64 `json:"rise_kb"`
	RisePct           float64 `json:"rise_pct"`
	SlopeKBPerMin     float64 `json:"slope_kb_per_min"`
	LateSlopeKBPerMin float64 `json:"late_slope_kb_per_min"`
	UpFraction        float64 `json:"up_fraction"`
	MaxStepShare      float64 `json:"max_step_share"`
	IdleFraction      float64 `json:"idle_fraction"`
	Sustained         bool    `json:"sustained"`
	Why               string  `json:"why"`
}

// AnalyzeWindow evaluates growth over the buckets in (end-window, end].
func AnalyzeWindow(buckets []history.Bucket, end time.Time, window time.Duration, p Params) Window {
	from := end.Add(-window)
	var pts []history.Bucket
	for _, b := range buckets {
		if b.T.After(from) && !b.T.After(end) {
			pts = append(pts, b)
		}
	}
	w := Window{Minutes: int(window / time.Minute), Points: len(pts)}
	w.Expected = max(1, w.Minutes)
	w.Coverage = math.Min(1, float64(len(pts))/float64(w.Expected))
	if len(pts) < 2 {
		w.Why = fmt.Sprintf("collecting history: %d of %d minutes sampled", len(pts), w.Expected)
		return w
	}
	span := pts[len(pts)-1].T.Sub(pts[0].T)
	w.Enough = len(pts) >= 5 && w.Coverage >= p.MinCoverage && span >= time.Duration(float64(window)*0.7)

	xs := make([]float64, len(pts))
	ys := make([]float64, len(pts))
	var idle, n int
	for i, b := range pts {
		xs[i] = b.T.Sub(pts[0].T).Minutes()
		ys[i] = float64(b.MedKB)
		idle += b.Idle
		n += b.N
	}
	if n > 0 {
		w.IdleFraction = float64(idle) / float64(n)
	}
	w.SlopeKBPerMin = TheilSen(xs, ys)
	k := max(1, min(3, len(pts)/3))
	w.StartKB, w.EndKB = medianF(ys[:k]), medianF(ys[len(ys)-k:])
	w.RiseKB = w.EndKB - w.StartKB
	if w.StartKB > 0 {
		w.RisePct = w.RiseKB / w.StartKB * 100
	}

	// A step only counts against steadiness if it persisted: the part of a
	// jump that later samples kept. A transient spike that falls back is
	// ignored, so it cannot mask an underlying steady leak.
	noise := math.Max(1024, 0.002*w.StartKB)
	suffixMin := make([]float64, len(ys))
	suffixMin[len(ys)-1] = ys[len(ys)-1]
	for i := len(ys) - 2; i >= 0; i-- {
		suffixMin[i] = math.Min(ys[i], suffixMin[i+1])
	}
	var up, maxStep float64
	for i := 1; i < len(ys); i++ {
		if ys[i]-ys[i-1] > noise {
			up++
		}
		maxStep = math.Max(maxStep, suffixMin[i]-ys[i-1])
	}
	w.UpFraction = up / float64(len(ys)-1)
	if w.RiseKB > 0 {
		w.MaxStepShare = maxStep / w.RiseKB
	}
	half := pts[0].T.Add(span / 2)
	var lx, ly []float64
	for i, b := range pts {
		if !b.T.Before(half) {
			lx, ly = append(lx, xs[i]), append(ly, ys[i])
		}
	}
	if len(lx) >= 3 {
		w.LateSlopeKBPerMin = TheilSen(lx, ly)
	}

	fitted := w.SlopeKBPerMin * span.Minutes()
	switch {
	case !w.Enough:
		w.Why = fmt.Sprintf("collecting history: %d of %d minutes sampled", len(pts), w.Expected)
	case w.SlopeKBPerMin <= 0 || w.RiseKB <= 0:
		w.Why = fmt.Sprintf("not rising over %d min", w.Minutes)
	case w.RiseKB < p.MinGrowthKB:
		w.Why = fmt.Sprintf("rise %s is below the %s minimum", MB(w.RiseKB), MB(p.MinGrowthKB))
	case w.RisePct < p.MinGrowthPct:
		w.Why = fmt.Sprintf("rise +%.0f%% is below the %.0f%% minimum", w.RisePct, p.MinGrowthPct)
	case fitted < 0.8*p.MinGrowthKB:
		w.Why = "fitted trend is below the minimum; the change is mostly noise"
	case w.UpFraction < p.MinUpFraction:
		w.Why = fmt.Sprintf("rise is irregular: only %.0f%% of minutes went up", w.UpFraction*100)
	case w.MaxStepShare > p.MaxStepShare:
		w.Why = fmt.Sprintf("rise is mostly a single step (%.0f%% in one minute), not steady growth", w.MaxStepShare*100)
	case w.LateSlopeKBPerMin <= 0 || w.LateSlopeKBPerMin < 0.4*w.SlopeKBPerMin:
		w.Why = "growth is levelling off"
	default:
		w.Sustained = true
		w.Why = fmt.Sprintf("RSS rose %s (+%.0f%%) steadily over %d min (%.0f%% of minutes up) and is still rising at %s/min",
			MB(w.RiseKB), w.RisePct, int(span.Round(time.Minute)/time.Minute), w.UpFraction*100, MB(w.LateSlopeKBPerMin))
	}
	return w
}

// Input is everything Evaluate needs about one session run.
type Input struct {
	Now     time.Time
	Samples []history.Sample
	Buckets []history.Bucket
}

// Assessment is the instantaneous (pre-hysteresis) verdict.
type Assessment struct {
	State          State
	Reasons        []string
	Trend          Trend
	Short          Window
	Long           Window
	CurrentKB      uint64
	AboveWarnFor   time.Duration
	AboveCritFor   time.Duration
	BudgetExceeded bool
	Declining      bool
}

// Evaluate computes the candidate state for a session run.
func Evaluate(in Input, p Params) Assessment {
	var a Assessment
	if len(in.Samples) == 0 {
		a.State, a.Trend = InsufficientData, Unknown
		a.Reasons = []string{"no samples yet"}
		return a
	}
	last := in.Samples[len(in.Samples)-1]
	a.CurrentKB = last.RSSKB
	a.AboveWarnFor = aboveFor(in.Samples, p.WarnKB, p.Interval)
	a.AboveCritFor = aboveFor(in.Samples, p.CriticalKB, p.Interval)
	a.BudgetExceeded = a.AboveCritFor > 0 && a.AboveCritFor >= p.ElevatedMin
	a.Short = AnalyzeWindow(in.Buckets, in.Now, p.GrowthWindow, p)
	a.Long = AnalyzeWindow(in.Buckets, in.Now, p.LeakWindow, p)
	a.Trend = trend(a.Short, p)
	// A real decline ends below the level at the start of the growth window;
	// falling back after a transient spike does not count.
	drop, declining := decline(in.Samples, in.Now, p)
	declining = declining && a.Short.Points >= 2 && float64(a.CurrentKB) < a.Short.StartKB
	a.Declining = declining

	switch {
	case a.Long.Sustained && !declining:
		a.State = SuspectedLeak
		a.Reasons = append(a.Reasons, "possible leak: "+a.Long.Why)
	case a.Short.Sustained && !declining && a.Short.IdleFraction >= p.IdleLeakFraction:
		a.State = SuspectedLeak
		a.Reasons = append(a.Reasons, fmt.Sprintf("possible leak: grew while the session was idle (%.0f%% of samples idle); %s",
			a.Short.IdleFraction*100, a.Short.Why))
	case a.Short.Sustained && !declining:
		a.State = SustainedGrowth
		a.Reasons = append(a.Reasons, a.Short.Why)
	case a.BudgetExceeded:
		a.State = Elevated
		a.Reasons = append(a.Reasons, fmt.Sprintf("RSS %s has exceeded the %s critical budget for %s",
			MB(float64(a.CurrentKB)), MB(float64(p.CriticalKB)), Dur(a.AboveCritFor)))
	case a.AboveWarnFor > 0 && a.AboveWarnFor >= p.ElevatedMin:
		a.State = Elevated
		a.Reasons = append(a.Reasons, fmt.Sprintf("RSS %s has been above the %s warning threshold for %s (trend %s)",
			MB(float64(a.CurrentKB)), MB(float64(p.WarnKB)), Dur(a.AboveWarnFor), a.Trend))
	case !a.Short.Enough:
		a.State = InsufficientData
		a.Reasons = append(a.Reasons, a.Short.Why)
	default:
		a.State = Healthy
		a.Reasons = append(a.Reasons, fmt.Sprintf("RSS %s, %s over %d min", MB(float64(a.CurrentKB)), a.Trend, a.Short.Minutes))
	}
	if declining {
		a.Reasons = append(a.Reasons, fmt.Sprintf("memory declining: %s released from the recent peak", MB(drop)))
	}
	if a.BudgetExceeded && a.State != Elevated {
		a.Reasons = append(a.Reasons, fmt.Sprintf("also above the %s critical budget for %s", MB(float64(p.CriticalKB)), Dur(a.AboveCritFor)))
	}
	return a
}

// aboveFor is how long RSS has stayed at or above threshold, counting back
// from the newest sample. A gap longer than three intervals ends the run,
// so missing data never counts as time above the threshold.
func aboveFor(samples []history.Sample, threshold uint64, interval time.Duration) time.Duration {
	if threshold == 0 || len(samples) == 0 {
		return 0
	}
	maxGap := 3 * interval
	end := len(samples) - 1
	if samples[end].RSSKB < threshold {
		return 0
	}
	first := end
	for i := end - 1; i >= 0; i-- {
		if samples[i].RSSKB < threshold || samples[i+1].T.Sub(samples[i].T) > maxGap {
			break
		}
		first = i
	}
	return samples[end].T.Sub(samples[first].T)
}

func trend(w Window, p Params) Trend {
	if w.Points < 3 {
		return Unknown
	}
	projected := w.SlopeKBPerMin * float64(w.Minutes)
	switch {
	case projected >= p.MinGrowthKB/2:
		return Rising
	case projected <= -p.MinGrowthKB/2:
		return Falling
	default:
		return Flat
	}
}

// decline reports a real release of memory: RSS has dropped well below its
// recent peak and the last five minutes are not rising.
func decline(samples []history.Sample, now time.Time, p Params) (float64, bool) {
	from := now.Add(-p.GrowthWindow)
	recentFrom := now.Add(-5 * time.Minute)
	var peak float64
	var rx, ry []float64
	for _, s := range samples {
		if s.T.Before(from) {
			continue
		}
		peak = math.Max(peak, float64(s.RSSKB))
		if !s.T.Before(recentFrom) {
			rx = append(rx, s.T.Sub(recentFrom).Minutes())
			ry = append(ry, float64(s.RSSKB))
		}
	}
	if len(ry) < 3 {
		return 0, false
	}
	drop := peak - ry[len(ry)-1]
	return drop, drop >= math.Max(p.MinGrowthKB/2, 0.1*peak) && TheilSen(rx, ry) <= 0
}

// TheilSen returns the median of pairwise slopes, robust to outliers.
func TheilSen(xs, ys []float64) float64 {
	n := len(xs)
	if n < 2 || n != len(ys) {
		return 0
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	// Bound the O(n²) cost by sampling evenly when n is large.
	const maxPoints = 150
	if n > maxPoints {
		idx = idx[:0]
		for i := 0; i < maxPoints; i++ {
			idx = append(idx, i*(n-1)/(maxPoints-1))
		}
	}
	slopes := make([]float64, 0, len(idx)*(len(idx)-1)/2)
	for a := 0; a < len(idx); a++ {
		for b := a + 1; b < len(idx); b++ {
			i, j := idx[a], idx[b]
			if dx := xs[j] - xs[i]; dx > 0 {
				slopes = append(slopes, (ys[j]-ys[i])/dx)
			}
		}
	}
	return medianF(slopes)
}

func medianF(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

// MB formats kilobytes compactly (1 MB = 1024 KB).
func MB(kb float64) string {
	sign := ""
	if kb < 0 {
		sign, kb = "-", -kb
	}
	if kb >= 1024*1024 {
		return fmt.Sprintf("%s%.1f GB", sign, kb/(1024*1024))
	}
	return fmt.Sprintf("%s%.0f MB", sign, kb/1024)
}

// Dur formats a duration compactly (e.g. 1h05m, 12m, 40s).
func Dur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
