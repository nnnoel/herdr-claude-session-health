// Package monitor runs the sampling loop: discover Claude sessions through
// Herdr, attribute and measure their process trees, record bounded history,
// evaluate health, raise alerts, report sidebar tokens and publish a
// snapshot for the UI.
//
// One goroutine drives everything, so samples can never overlap; a guard
// additionally rejects re-entrant Tick calls. Every external call carries a
// timeout derived from the sampling interval.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"claude-session-health/internal/alerting"
	"claude-session-health/internal/analysis"
	"claude-session-health/internal/attribution"
	"claude-session-health/internal/collect"
	"claude-session-health/internal/config"
	"claude-session-health/internal/fsutil"
	"claude-session-health/internal/herdr"
	"claude-session-health/internal/history"
	"claude-session-health/internal/logx"
	"claude-session-health/internal/resume"
	"claude-session-health/internal/state"
	"claude-session-health/internal/systemhealth"
)

// Stop reasons returned by Run and Tick.
var (
	ErrDisabled  = errors.New("plugin disabled or uninstalled")
	ErrHerdrGone = errors.New("herdr server unreachable for too long")
)

// TokenName is the sidebar token users add to Herdr's sidebar rows ($memhealth).
const TokenName = "memhealth"

const (
	rootRefresh     = 60 * time.Second
	unlistedGrace   = 60 * time.Second
	disabledAfter   = 3 // consecutive "not enabled" answers before exiting
	persistEvery    = 60 * time.Second
	pruneEvery      = 10 * time.Minute
	enabledEvery    = 30 * time.Second
	resumeEvery     = 5 * time.Minute
	herdrGoneAfter  = 5 * time.Minute
	failingAfter    = 3
	baselineSamples = 12
	recentAlertCap  = 50
	sparkMinutes    = 60
	sparkRawSamples = 60
	maxAlertBody    = 300
)

// HerdrAPI is the subset of the Herdr client the monitor uses.
type HerdrAPI interface {
	Agents(ctx context.Context) ([]herdr.Agent, error)
	ProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	ReportTokens(ctx context.Context, paneID string, tokens map[string]*string, ttl time.Duration) error
	PluginEnabled(ctx context.Context, pluginID string) (bool, error)
}

// ProcSource captures the process table.
type ProcSource interface {
	Snapshot(ctx context.Context) (*collect.ProcTable, error)
}

// SystemSource captures system memory metrics.
type SystemSource interface {
	Sample(ctx context.Context) collect.SystemSample
}

// Deps are the monitor's collaborators.
type Deps struct {
	Herdr        HerdrAPI
	Procs        ProcSource
	System       SystemSource
	Notifier     alerting.Notifier
	Log          *logx.Logger
	Now          func() time.Time
	StateDir     string // empty disables persistence (tests)
	PluginID     string
	Version      string
	HerdrVersion string
	Resume       func(sessionID, source, cwd string) resume.Info
}

type rootCache struct {
	root       attribution.Root
	group      []int
	verifiedAt time.Time
}

type session struct {
	key          string
	agent        herdr.Agent
	firstSeen    time.Time
	tracker      analysis.Tracker
	assessment   analysis.Assessment
	lastActive   time.Time
	cpuPrev      map[int]time.Duration
	cpuPrevAt    time.Time
	cpuPercent   float64
	cpuEstimated bool
	root         attribution.Root
	usage        attribution.Usage
	orphanCount  int
	orphanKB     uint64
	sampled      bool
	note         string
	missingSince time.Time
	unlistedAt   time.Time // when Herdr stopped listing the pane (grace period)
	resume       resume.Info
	resumeFor    string
	resumeAt     time.Time
	tokenText    string
	tokenSentAt  time.Time
	tokenErr     string
}

// Monitor is the sampling loop state.
type Monitor struct {
	cfg      config.Config
	warnings []string
	p        analysis.Params
	d        Deps

	store    *history.Store
	sessions map[string]*session
	roots    map[string]*rootCache
	sys      *systemhealth.Tracker
	sysA     *systemhealth.Assessment
	alerts   *alerting.Engine
	status   state.MonitorStatus

	lastPersist, lastPrune, lastEnabledCheck time.Time
	firstFailureAt                           time.Time
	herdrDownSince                           time.Time // first consecutive "socket unavailable" failure
	herdrDown                                bool
	disabledChecks                           int // consecutive "not enabled" answers
	disabled                                 bool
	lastErrLogged                            string
	ticking                                  atomic.Bool
}

// New builds a monitor from validated config.
func New(cfg config.Config, warnings []string, d Deps) *Monitor {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Resume == nil {
		d.Resume = func(string, string, string) resume.Info { return resume.Info{Note: "resume lookup disabled"} }
	}
	return &Monitor{
		cfg: cfg, warnings: warnings, p: analysis.ParamsFrom(cfg), d: d,
		store:    history.NewStore(cfg.RawCapacity(), cfg.MinuteCapacity(), baselineSamples),
		sessions: map[string]*session{},
		roots:    map[string]*rootCache{},
		sys:      systemhealth.NewTracker(time.Duration(cfg.SwapGrowthWindowMinutes)*time.Minute, cfg.Interval()),
		alerts: alerting.New(config.Minutes(cfg.AlertCooldownMinutes), cfg.Verbosity, cfg.Notifications,
			d.Notifier, d.Log, recentAlertCap),
		status: state.MonitorStatus{Status: state.StatusStarting, DaemonPID: os.Getpid(), HerdrVersion: d.HerdrVersion},
	}
}

// Run loads history, samples every interval until ctx is cancelled or a
// stop condition occurs, then persists and publishes a final snapshot.
func (m *Monitor) Run(ctx context.Context) error {
	m.status.StartedAt = m.d.Now()
	m.loadHistory()
	m.d.Log.Logf(logx.Lifecycle, "monitor started pid=%d version=%s interval=%s", os.Getpid(), m.d.Version, m.cfg.Interval())
	for _, w := range m.warnings {
		m.d.Log.Logf(logx.Config, "config warning: %s", w)
	}

	ticker := time.NewTicker(m.cfg.Interval())
	defer ticker.Stop()
	err := m.Tick(ctx)
	for err == nil {
		select {
		case <-ctx.Done():
			m.shutdown("context cancelled")
			return nil
		case <-ticker.C:
			err = m.Tick(ctx)
		}
	}
	m.shutdown(err.Error())
	return err
}

// Tick performs one sampling cycle. It is exported for tests.
func (m *Monitor) Tick(ctx context.Context) error {
	if !m.ticking.CompareAndSwap(false, true) {
		m.d.Log.Logf(logx.Sampling, "tick skipped: previous tick still running")
		return nil
	}
	defer m.ticking.Store(false)

	// Wall-clock time only: the monotonic clock stops during sleep, which
	// would hide sleep gaps from gap detection and recovery timers.
	now := m.d.Now().Round(0)
	tctx, cancel := context.WithTimeout(ctx, m.cfg.Interval())
	defer cancel()
	err := m.sample(tctx, now)
	m.status.LastTickAt = now
	m.status.LastTickMs = m.d.Now().Sub(now).Milliseconds()
	m.status.Ticks++
	if err != nil {
		m.onFailure(ctx, now, err)
	} else {
		m.onSuccess(ctx, now)
	}
	m.periodic(ctx, now)
	m.writeSnapshot(now)

	switch {
	case m.disabled:
		return ErrDisabled
	case m.herdrDown && now.Sub(m.herdrDownSince) >= herdrGoneAfter:
		return ErrHerdrGone
	}
	return nil
}

func (m *Monitor) onFailure(ctx context.Context, now time.Time, err error) {
	m.status.ConsecutiveFailures++
	m.status.LastError = err.Error()
	m.herdrDown = errors.Is(err, herdr.ErrUnavailable)
	switch {
	case !m.herdrDown:
		m.herdrDownSince = time.Time{}
	case m.herdrDownSince.IsZero():
		m.herdrDownSince = now
	}
	if m.firstFailureAt.IsZero() {
		m.firstFailureAt = now
	}
	m.status.Status = state.StatusDegraded
	if m.status.ConsecutiveFailures >= failingAfter {
		m.status.Status = state.StatusFailing
		m.alerts.Raise(ctx, now, "monitor:failed", "", alerting.KindFailed, alerting.Critical,
			"Claude session health: sampling is failing",
			fmt.Sprintf("%d consecutive sampling failures since %s: %s. Session data shown in the dashboard is stale.",
				m.status.ConsecutiveFailures, m.firstFailureAt.Format("15:04:05"), truncate(err.Error(), 160)))
	}
	if err.Error() != m.lastErrLogged || m.status.ConsecutiveFailures%60 == 0 {
		m.d.Log.Logf(logx.Sampling, "sample failed (%d consecutive): %v", m.status.ConsecutiveFailures, err)
		m.lastErrLogged = err.Error()
	}
	for _, s := range m.sessions {
		s.sampled = false
		s.note = "monitoring failed: " + truncate(err.Error(), 120)
		if s.missingSince.IsZero() {
			s.missingSince = now
		}
	}
}

func (m *Monitor) onSuccess(ctx context.Context, now time.Time) {
	if m.status.ConsecutiveFailures > 0 {
		m.d.Log.Logf(logx.Sampling, "sampling recovered after %d failures", m.status.ConsecutiveFailures)
	}
	m.status.ConsecutiveFailures, m.status.LastError, m.lastErrLogged = 0, "", ""
	m.firstFailureAt, m.herdrDownSince, m.herdrDown = time.Time{}, time.Time{}, false
	m.status.LastSuccessAt = now
	m.status.Status = state.StatusOK
	for _, s := range m.sessions {
		if !s.sampled {
			m.status.Status = state.StatusDegraded
		}
	}
	m.alerts.Resolve(ctx, now, "monitor:failed", "Sampling succeeded again.")
}

func (m *Monitor) sample(ctx context.Context, now time.Time) error {
	agents, err := m.d.Herdr.Agents(ctx)
	if err != nil {
		return fmt.Errorf("list herdr agents: %w", err)
	}
	table, err := m.d.Procs.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("capture process table: %w", err)
	}
	if table.Skipped > 0 {
		m.d.Log.Logf(logx.Sampling, "ps: skipped %d malformed lines", table.Skipped)
	}
	m.updateSystem(ctx, m.d.System.Sample(ctx))
	m.discover(now, agents)

	keys := m.sessionKeys()
	roots := map[string]int{}
	for _, key := range keys {
		s := m.sessions[key]
		if !s.unlistedAt.IsZero() {
			continue
		}
		root, err := m.resolveRoot(ctx, s.agent.PaneID, table, now)
		if err != nil {
			m.markMissing(s, now, err.Error())
			continue
		}
		s.root = root
		roots[key] = root.PID
	}

	usages := attribution.Measure(roots, table)
	for _, key := range keys {
		s := m.sessions[key]
		u, ok := usages[key]
		if !ok {
			continue
		}
		if u.Missing {
			m.markMissing(s, now, firstOr(u.Notes, "claude process exited"))
			continue
		}
		m.observe(ctx, s, u, table, now)
	}
	for _, key := range keys {
		m.staleAlerts(ctx, m.sessions[key], now)
	}
	m.reportTokens(ctx, now)
	return nil
}

func (m *Monitor) discover(now time.Time, agents []herdr.Agent) {
	live := map[string]herdr.Agent{}
	for _, a := range agents {
		if a.Agent == "claude" {
			live[sessionKey(a)] = a
		}
	}
	// A session missing from agent.list is kept (as not sampled) for a grace
	// period, so a brief listing glitch doesn't discard its hysteresis state
	// and alert cooldowns.
	for key, s := range m.sessions {
		if _, ok := live[key]; ok {
			continue
		}
		if s.unlistedAt.IsZero() {
			s.unlistedAt = now
			m.markMissing(s, now, "Herdr no longer lists this pane as a claude agent")
		}
		if now.Sub(s.unlistedAt) >= unlistedGrace {
			m.d.Log.Logf(logx.Discovery, "session ended key=%s pane=%s", key, s.agent.PaneID)
			m.alerts.Forget(key)
			delete(m.sessions, key)
		}
	}
	livePanes := map[string]bool{}
	for key, a := range live {
		livePanes[a.PaneID] = true
		s, ok := m.sessions[key]
		if !ok {
			s = &session{key: key, firstSeen: now, lastActive: now}
			m.sessions[key] = s
			m.d.Log.Logf(logx.Discovery, "session discovered key=%s pane=%s", key, a.PaneID)
		}
		s.agent, s.unlistedAt = a, time.Time{}
	}
	for pane := range m.roots {
		if !livePanes[pane] {
			delete(m.roots, pane)
		}
	}
}

// sessionKey is a stable identity for a session: the Herdr terminal, which
// survives pane moves. The Claude process run (PID + start time) is tracked
// separately, so restarts in the same terminal reset the baseline.
func sessionKey(a herdr.Agent) string {
	if a.TerminalID != "" {
		return a.TerminalID
	}
	return "pane:" + a.PaneID
}

func (m *Monitor) sessionKeys() []string {
	keys := make([]string, 0, len(m.sessions))
	for k := range m.sessions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveRoot returns the pane's verified Claude root, asking Herdr only
// when the cached root vanished, was replaced (PID reuse), or is due for
// re-verification.
func (m *Monitor) resolveRoot(ctx context.Context, paneID string, t *collect.ProcTable, now time.Time) (attribution.Root, error) {
	if c := m.roots[paneID]; c != nil {
		start, alive := t.StartTime(c.root.PID)
		if alive && collect.SameStart(start, c.root.Start) && now.Sub(c.verifiedAt) < rootRefresh {
			return c.root, nil
		}
	}
	info, err := m.d.Herdr.ProcessInfo(ctx, paneID)
	if err != nil {
		delete(m.roots, paneID)
		return attribution.Root{}, fmt.Errorf("pane process info: %w", err)
	}
	pp := attribution.PaneProcesses{ShellPID: info.ShellPID, ForegroundPGID: info.ForegroundPGID}
	for _, fp := range info.Procs {
		pp.GroupPIDs = append(pp.GroupPIDs, fp.PID)
		if fp.LooksLikeClaude {
			pp.ClaudePIDs = append(pp.ClaudePIDs, fp.PID)
		}
	}
	root, err := attribution.SelectRoot(pp, t)
	if err != nil {
		delete(m.roots, paneID)
		return attribution.Root{}, err
	}
	prev := m.roots[paneID]
	if prev == nil || prev.root.PID != root.PID || !collect.SameStart(prev.root.Start, root.Start) || prev.root.Confidence != root.Confidence {
		m.d.Log.Logf(logx.Attribution, "pane=%s root pid=%d name=%s confidence=%s notes=%q",
			paneID, root.PID, root.Name, root.Confidence, strings.Join(root.Notes, "; "))
	}
	m.roots[paneID] = &rootCache{root: root, group: pp.GroupPIDs, verifiedAt: now}
	return root, nil
}

func (m *Monitor) markMissing(s *session, now time.Time, note string) {
	if s.sampled || s.note != note {
		m.d.Log.Logf(logx.Attribution, "pane=%s not sampled: %s", s.agent.PaneID, note)
	}
	s.sampled, s.note = false, note
	if s.missingSince.IsZero() {
		s.missingSince = now
	}
	m.store.Touch(s.key, m.meta(s), now)
}

func (m *Monitor) meta(s *session) history.Meta {
	return history.Meta{PaneID: s.agent.PaneID, SessionID: s.agent.SessionID, Cwd: s.agent.Cwd}
}

func (m *Monitor) observe(ctx context.Context, s *session, u attribution.Usage, t *collect.ProcTable, now time.Time) {
	cpu, estimated := cpuPercent(s.cpuPrev, s.cpuPrevAt, u, now)
	s.cpuPrev, s.cpuPrevAt = u.CPUTimes, now
	s.cpuPercent, s.cpuEstimated = cpu, estimated

	switch s.agent.Status {
	case "working":
		s.lastActive = now
	case "idle", "done", "blocked":
	default:
		if cpu > m.cfg.IdleCPUPercent {
			s.lastActive = now
		}
	}
	idle := now.Sub(s.lastActive) >= config.Minutes(m.cfg.IdleAfterMinutes)

	s.orphanCount, s.orphanKB = 0, 0
	if c := m.roots[s.agent.PaneID]; c != nil {
		s.orphanCount, s.orphanKB = attribution.Orphans(c.group, u, t)
	}
	sample := history.Sample{T: now, RSSKB: u.TotalRSSKB(), ClaudeKB: u.ClaudeRSSKB, CPU: cpu, Idle: idle}
	runID := history.RunID{PID: s.root.PID, Start: s.root.Start}
	newRun, err := m.store.Observe(s.key, m.meta(s), runID, sample)
	if errors.Is(err, history.ErrOutOfOrder) {
		// The wall clock moved backwards (or persisted history came from a
		// skewed clock). Start a fresh run rather than rejecting samples
		// until the clock catches up.
		m.d.Log.Logf(logx.Lifecycle, "session %s: clock moved backwards; starting a fresh history run", s.key)
		m.store.ResetRun(s.key)
		s.tracker.Reset()
		newRun, err = m.store.Observe(s.key, m.meta(s), runID, sample)
	}
	if err != nil {
		m.markMissing(s, now, "history rejected sample: "+err.Error())
		return
	}
	sess, _ := m.store.Session(s.key)
	if newRun {
		if sess.Restarts > 0 && len(sess.PastRuns) > 0 {
			prev := sess.PastRuns[len(sess.PastRuns)-1]
			m.d.Log.Logf(logx.Lifecycle, "session %s pane=%s new claude run pid=%d (previous pid=%d); baseline and history reset",
				s.key, s.agent.PaneID, s.root.PID, prev.PID)
			body := fmt.Sprintf("Claude restarted in %s (PID %d → %d); history and baseline were reset.", s.agent.PaneID, prev.PID, s.root.PID)
			for _, k := range []string{":leak", ":growth", ":budget", ":elevated"} {
				m.alerts.Resolve(ctx, now, s.key+k, body)
			}
		}
		s.tracker.Reset()
	}
	if !s.missingSince.IsZero() {
		m.d.Log.Logf(logx.Sampling, "pane=%s sampling resumed", s.agent.PaneID)
	}
	s.sampled, s.note, s.missingSince, s.usage = true, "", time.Time{}, u

	run := sess.Run
	a := analysis.Evaluate(analysis.Input{Now: now, Samples: run.Samples(), Buckets: run.Buckets()}, m.p)
	s.assessment = a
	if tr, changed := s.tracker.Update(a, now, m.p.Recovery); changed {
		m.d.Log.Logf(logx.State, "session %s pane=%s %s -> %s: %s", s.key, s.agent.PaneID,
			orNone(string(tr.From)), tr.To, truncate(firstOr(tr.Reasons, ""), 200))
	}
	m.sessionAlerts(ctx, s, a, now)
	m.refreshResume(s, now)
}

// cpuPercent derives CPU usage from cumulative CPU-time deltas. Before a
// previous sample exists it falls back to ps's decaying average.
func cpuPercent(prev map[int]time.Duration, prevAt time.Time, u attribution.Usage, now time.Time) (float64, bool) {
	wall := now.Sub(prevAt).Seconds()
	if prev == nil || prevAt.IsZero() || wall <= 0 {
		return u.PSCPUPercent, true
	}
	var used time.Duration
	for pid, t := range u.CPUTimes {
		if old, ok := prev[pid]; ok {
			if t > old {
				used += t - old
			}
			continue
		}
		if st, ok := u.StartTimes[pid]; ok && !st.Before(prevAt.Add(-time.Second)) {
			used += t
		}
	}
	return used.Seconds() / wall * 100, false
}

func (m *Monitor) sessionAlerts(ctx context.Context, s *session, a analysis.Assessment, now time.Time) {
	label := fmt.Sprintf("%s (%s)", s.agent.PaneID, filepath.Base(s.agent.Cwd))
	stats := ""
	if sess, ok := m.store.Session(s.key); ok && sess.Run != nil {
		stats = fmt.Sprintf(" RSS %s, peak %s, baseline %s, PID %d.", analysis.MB(float64(a.CurrentKB)),
			analysis.MB(float64(sess.Run.PeakKB)), analysis.MB(float64(sess.Run.BaselineKB)), s.root.PID)
		// The tree includes children (MCP servers, dev servers, tests), so
		// name the split to show whether Claude itself or a child is growing.
		stats += fmt.Sprintf(" Claude %s, children %s", analysis.MB(float64(s.usage.ClaudeRSSKB)), analysis.MB(float64(s.usage.ChildrenRSSKB)))
		if len(s.usage.TopChildren) > 0 {
			c := s.usage.TopChildren[0]
			stats += fmt.Sprintf(" (largest: %s %s, PID %d)", c.Name, analysis.MB(float64(c.RSSKB)), c.PID)
		}
		stats += "."
	}
	body := func(reasons []string) string {
		return truncate(capitalize(firstOr(reasons, ""))+"."+stats, maxAlertBody)
	}

	switch s.tracker.State {
	case analysis.SuspectedLeak:
		m.alerts.Raise(ctx, now, s.key+":leak", s.key, alerting.KindLeak, alerting.Critical,
			"Possible memory leak: Claude "+label, body(s.tracker.Reasons))
	case analysis.SustainedGrowth:
		m.alerts.Raise(ctx, now, s.key+":growth", s.key, alerting.KindGrowth, alerting.Warning,
			"Sustained memory growth: Claude "+label, body(s.tracker.Reasons))
		m.alerts.Resolve(ctx, now, s.key+":leak", "Now classified as sustained growth, not a suspected leak."+stats)
	case analysis.InsufficientData:
	default:
		reason := truncate("Memory growth stopped: "+firstOr(s.tracker.Reasons, "")+"."+stats, maxAlertBody)
		m.alerts.Resolve(ctx, now, s.key+":leak", reason)
		m.alerts.Resolve(ctx, now, s.key+":growth", reason)
	}

	switch s.tracker.State {
	case analysis.Elevated:
		m.alerts.Raise(ctx, now, s.key+":elevated", s.key, alerting.KindElevated, alerting.Info,
			"Elevated memory: Claude "+label, body(s.tracker.Reasons))
	case analysis.InsufficientData:
	default:
		m.alerts.Resolve(ctx, now, s.key+":elevated", "Memory is back below the warning threshold."+stats)
	}

	switch {
	case a.BudgetExceeded:
		m.alerts.Raise(ctx, now, s.key+":budget", s.key, alerting.KindBudget, alerting.Critical,
			"Memory budget exceeded: Claude "+label,
			truncate(fmt.Sprintf("RSS %s has been above the %s critical budget for %s.%s",
				analysis.MB(float64(a.CurrentKB)), analysis.MB(float64(m.p.CriticalKB)), analysis.Dur(a.AboveCritFor), stats), maxAlertBody))
	case float64(a.CurrentKB) < 0.9*float64(m.p.CriticalKB):
		m.alerts.Resolve(ctx, now, s.key+":budget", "RSS is back below 90% of the critical budget."+stats)
	}
}

func (m *Monitor) staleAlerts(ctx context.Context, s *session, now time.Time) {
	key := s.key + ":stale"
	if !s.unlistedAt.IsZero() {
		return // a closed pane is not a monitoring failure
	}
	if !s.sampled && !s.missingSince.IsZero() && now.Sub(s.missingSince) >= 3*m.cfg.Interval() {
		m.alerts.Raise(ctx, now, key, s.key, alerting.KindStale, alerting.Warning,
			"Claude memory monitoring stale: "+s.agent.PaneID,
			truncate(fmt.Sprintf("No sample for %s since %s: %s.", s.agent.PaneID, s.missingSince.Format("15:04:05"), s.note), maxAlertBody))
	} else if s.sampled {
		m.alerts.Resolve(ctx, now, key, "Sampling resumed.")
	}
}

func (m *Monitor) updateSystem(ctx context.Context, sample collect.SystemSample) {
	a := m.sys.Update(sample, systemhealth.Params{
		Sustain:         config.Minutes(m.cfg.PressureSustainMinutes),
		SwapGrowthBytes: uint64(m.cfg.SwapGrowthMB) << 20,
		SwapWindow:      time.Duration(m.cfg.SwapGrowthWindowMinutes) * time.Minute,
	})
	if m.sysA == nil || m.sysA.Status != a.Status {
		m.d.Log.Logf(logx.State, "system memory status %s: %s", a.Status, strings.Join(a.Reasons, "; "))
	}
	m.sysA = &a
	if !m.cfg.SystemPressureWarnings {
		return
	}
	now := sample.TakenAt
	body := capitalize(strings.Join(a.Reasons, "; ")) + "."
	switch {
	case a.Persistent && sample.Pressure == collect.PressureCritical:
		m.alerts.Raise(ctx, now, "system:pressure:critical", "", alerting.KindSystemPressure, alerting.Critical,
			"System memory pressure critical", body)
	case a.Persistent:
		m.alerts.Raise(ctx, now, "system:pressure:warn", "", alerting.KindSystemPressure, alerting.Warning,
			"System memory pressure elevated", body)
		m.alerts.Resolve(ctx, now, "system:pressure:critical", "Pressure eased to warn.")
	case sample.Pressure == collect.PressureNormal:
		m.alerts.Resolve(ctx, now, "system:pressure:critical", "Kernel memory pressure is back to normal.")
		m.alerts.Resolve(ctx, now, "system:pressure:warn", "Kernel memory pressure is back to normal.")
	}
	switch {
	case a.RapidSwap:
		m.alerts.Raise(ctx, now, "system:swap", "", alerting.KindSwapGrowth, alerting.Warning, "Rapid swap growth", body)
	case a.SwapKnown && a.SwapGrowthBytes < int64(m.cfg.SwapGrowthMB)<<19:
		m.alerts.Resolve(ctx, now, "system:swap", "Swap growth has slowed.")
	}
}

func (m *Monitor) refreshResume(s *session, now time.Time) {
	if !s.resumeAt.IsZero() && s.resumeFor == s.agent.SessionID && now.Sub(s.resumeAt) < resumeEvery {
		return
	}
	s.resume = m.d.Resume(s.agent.SessionID, s.agent.SessionSource, s.agent.Cwd)
	s.resumeFor, s.resumeAt = s.agent.SessionID, now
}

// tokenText is the compact sidebar summary. It complements CPU/memory
// usage plugins by showing the health verdict rather than raw usage.
func (m *Monitor) tokenText(s *session) string {
	if !s.sampled {
		return "stale"
	}
	a := s.assessment
	switch s.tracker.State {
	case analysis.SuspectedLeak:
		w := a.Long
		if !w.Sustained {
			w = a.Short
		}
		return fmt.Sprintf("leak? +%s/%dm", short(w.RiseKB), w.Minutes)
	case analysis.SustainedGrowth:
		return fmt.Sprintf("↗ +%s/%dm", short(a.Short.RiseKB), a.Short.Minutes)
	case analysis.Elevated:
		if a.BudgetExceeded {
			return "over " + short(float64(a.CurrentKB))
		}
		return "high " + short(float64(a.CurrentKB))
	case analysis.InsufficientData:
		return "learning"
	default:
		if a.Trend == analysis.Rising {
			return "ok ↗"
		}
		return "ok"
	}
}

func (m *Monitor) reportTokens(ctx context.Context, now time.Time) {
	if !m.cfg.SidebarToken {
		return
	}
	ttl := 3*m.cfg.Interval() + 2*time.Second
	for _, key := range m.sessionKeys() {
		s := m.sessions[key]
		if !s.unlistedAt.IsZero() {
			continue // the pane may be gone; the token expires by TTL
		}
		text := m.tokenText(s)
		if text == s.tokenText && now.Sub(s.tokenSentAt) < 2*m.cfg.Interval() {
			continue
		}
		if err := m.d.Herdr.ReportTokens(ctx, s.agent.PaneID, map[string]*string{TokenName: &text}, ttl); err != nil {
			if err.Error() != s.tokenErr {
				m.d.Log.Logf(logx.Sampling, "pane=%s sidebar token report failed: %v", s.agent.PaneID, err)
				s.tokenErr = err.Error()
			}
			continue
		}
		s.tokenErr, s.tokenText, s.tokenSentAt = "", text, now
	}
}

func (m *Monitor) clearTokens(ctx context.Context) {
	if !m.cfg.SidebarToken {
		return
	}
	for _, key := range m.sessionKeys() {
		if s := m.sessions[key]; s.tokenText != "" {
			_ = m.d.Herdr.ReportTokens(ctx, s.agent.PaneID, map[string]*string{TokenName: nil}, time.Second)
		}
	}
}

func (m *Monitor) periodic(ctx context.Context, now time.Time) {
	if now.Sub(m.lastEnabledCheck) >= enabledEvery && m.d.PluginID != "" && !m.herdrDown {
		m.lastEnabledCheck = now
		enabled, err := m.d.Herdr.PluginEnabled(ctx, m.d.PluginID)
		switch {
		case err != nil:
			m.d.Log.Logf(logx.Lifecycle, "plugin enabled check failed: %v", err)
		case enabled:
			m.disabledChecks = 0
		default:
			// Herdr may briefly report an empty plugin list while restarting.
			m.disabledChecks++
			m.d.Log.Logf(logx.Lifecycle, "plugin %s reported disabled or uninstalled (%d/%d)", m.d.PluginID, m.disabledChecks, disabledAfter)
			if m.disabledChecks >= disabledAfter {
				m.d.Log.Logf(logx.Lifecycle, "plugin %s is disabled or uninstalled; stopping", m.d.PluginID)
				m.disabled = true
			}
		}
	}
	if now.Sub(m.lastPrune) >= pruneEvery {
		m.lastPrune = now
		if removed := m.store.Prune(now, time.Duration(m.cfg.RetentionHours*float64(time.Hour))); len(removed) > 0 {
			m.d.Log.Logf(logx.Persistence, "pruned %d sessions past retention", len(removed))
		}
	}
	if now.Sub(m.lastPersist) >= persistEvery {
		m.persist(now)
	}
}

func (m *Monitor) persist(now time.Time) {
	m.lastPersist = now
	if m.d.StateDir == "" {
		return
	}
	if err := m.store.Save(filepath.Join(m.d.StateDir, state.HistoryFile), now); err != nil {
		if m.status.PersistError != err.Error() {
			m.d.Log.Logf(logx.Persistence, "history save failed: %v", err)
		}
		m.status.PersistError = err.Error()
		return
	}
	m.status.PersistError = ""
	m.status.HistoryPersistedAt = now
}

func (m *Monitor) loadHistory() {
	if m.d.StateDir == "" {
		return
	}
	path := filepath.Join(m.d.StateDir, state.HistoryFile)
	n, err := m.store.Load(path, m.d.Now(), time.Duration(m.cfg.RetentionHours*float64(time.Hour)))
	if err != nil {
		m.d.Log.Logf(logx.Persistence, "history load failed, starting empty: %v", err)
		if renameErr := os.Rename(path, path+".corrupt"); renameErr == nil {
			m.d.Log.Logf(logx.Persistence, "moved unreadable history to %s.corrupt", path)
		}
		return
	}
	m.d.Log.Logf(logx.Persistence, "loaded history for %d sessions", n)
}

func (m *Monitor) shutdown(reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !m.herdrDown {
		m.clearTokens(ctx)
	}
	now := m.d.Now()
	m.persist(now)
	m.status.Status = state.StatusStopped
	m.status.LastError = reason
	for _, s := range m.sessions {
		s.sampled, s.note = false, "monitor stopped"
	}
	m.writeSnapshot(now)
	m.d.Log.Logf(logx.Lifecycle, "monitor stopped: %s", reason)
}

// Snapshot builds the published view.
func (m *Monitor) Snapshot(now time.Time) state.Snapshot {
	m.status.SessionsInHistory = len(m.store.Keys())
	snap := state.Snapshot{
		SchemaVersion: state.SchemaVersion, Version: m.d.Version, Platform: runtime.GOOS,
		GeneratedAt: now, Monitor: m.status, System: m.sysA, Alerts: m.alerts.Recent(),
		Config: m.cfg, ConfigWarnings: m.warnings,
	}
	for _, key := range m.sessionKeys() {
		snap.Sessions = append(snap.Sessions, m.view(m.sessions[key], now))
	}
	sort.SliceStable(snap.Sessions, func(i, j int) bool {
		a, b := snap.Sessions[i], snap.Sessions[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		return a.RSSKB > b.RSSKB
	})
	return snap
}

func (m *Monitor) view(s *session, now time.Time) state.Session {
	st := s.tracker.State
	if st == "" {
		st = analysis.InsufficientData
	}
	reasons := s.tracker.Reasons
	if len(reasons) == 0 {
		reasons = []string{firstOr(s.assessment.Reasons, "waiting for the first sample")}
	}
	v := state.Session{
		Key: s.key, PaneID: s.agent.PaneID, SessionID: s.agent.SessionID, Cwd: s.agent.Cwd,
		AgentStatus: s.agent.Status, LastActivityAt: s.lastActive, FirstSeen: s.firstSeen,
		PID: s.root.PID, ProcessComm: s.root.Name, Sampled: s.sampled, SampleNote: s.note,
		ClaudeKB: s.usage.ClaudeRSSKB, ChildrenKB: s.usage.ChildrenRSSKB, RSSKB: s.usage.TotalRSSKB(),
		CPUPercent: s.cpuPercent, CPUEstimated: s.cpuEstimated, ChildCount: s.usage.ChildCount,
		TopChildren: s.usage.TopChildren, OrphanCount: s.orphanCount, OrphanKB: s.orphanKB,
		Confidence:       string(s.root.Confidence),
		AttributionNotes: append(append([]string(nil), s.root.Notes...), s.usage.Notes...),
		State:            st, StateLabel: st.Label(), StateSince: s.tracker.Since, Reasons: reasons, HoldNote: s.tracker.Note,
		Trend: s.assessment.Trend, Short: s.assessment.Short, Long: s.assessment.Long,
		AboveWarnSec:   int64(s.assessment.AboveWarnFor / time.Second),
		AboveCritSec:   int64(s.assessment.AboveCritFor / time.Second),
		BudgetExceeded: s.assessment.BudgetExceeded, Resume: s.resume, Severity: st.Severity(),
	}
	v.Idle = now.Sub(s.lastActive) >= config.Minutes(m.cfg.IdleAfterMinutes)
	if s.root.PID > 0 {
		// Claude retitles itself (its kernel name is its version number), so
		// show the identity Herdr verified rather than the raw name.
		v.ProcessName = "claude"
	}
	if !s.sampled {
		v.Severity = max(v.Severity, 1)
	}
	if sess, ok := m.store.Session(s.key); ok && sess.Run != nil {
		r := sess.Run
		v.Restarts, v.RunStartedAt, v.LastSampleAt = sess.Restarts, r.StartedAt, r.LastSample
		v.PeakKB, v.PeakAt, v.BaselineKB, v.BaselineReady = r.PeakKB, r.PeakAt, r.BaselineKB, r.BaselineReady
		v.SparkMinutes = sparkFromBuckets(r.Buckets(), now)
		v.SparkRaw = sparkFromSamples(r.Samples())
	}
	return v
}

// sparkFromBuckets lays out the last hour minute by minute, marking minutes
// without data as -1 so the UI shows gaps instead of zeros.
func sparkFromBuckets(b []history.Bucket, now time.Time) []float64 {
	out := make([]float64, sparkMinutes)
	for i := range out {
		out[i] = -1
	}
	end := now.Truncate(time.Minute)
	for _, x := range b {
		idx := sparkMinutes - 1 - int(end.Sub(x.T)/time.Minute)
		if idx >= 0 && idx < sparkMinutes {
			out[idx] = float64(x.MedKB) / 1024
		}
	}
	return out
}

func sparkFromSamples(s []history.Sample) []float64 {
	if len(s) > sparkRawSamples {
		s = s[len(s)-sparkRawSamples:]
	}
	out := make([]float64, len(s))
	for i, x := range s {
		out[i] = float64(x.RSSKB) / 1024
	}
	return out
}

func (m *Monitor) writeSnapshot(now time.Time) {
	if m.d.StateDir == "" {
		return
	}
	data, err := json.Marshal(m.Snapshot(now))
	if err == nil {
		err = fsutil.WriteFileAtomic(filepath.Join(m.d.StateDir, state.SnapshotFile), data, 0o600)
	}
	if err != nil {
		m.d.Log.Logf(logx.Persistence, "snapshot write failed: %v", err)
	}
}

// Alerts exposes the alert engine (tests).
func (m *Monitor) Alerts() *alerting.Engine { return m.alerts }

// Store exposes the history store (tests).
func (m *Monitor) Store() *history.Store { return m.store }

func short(kb float64) string {
	if kb < 0 {
		kb = 0
	}
	if kb >= 1024*1024 {
		return fmt.Sprintf("%.1fG", kb/(1024*1024))
	}
	return fmt.Sprintf("%.0fM", kb/1024)
}

func firstOr(s []string, def string) string {
	if len(s) == 0 || s[0] == "" {
		return def
	}
	return s[0]
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
