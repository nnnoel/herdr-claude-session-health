package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-session-health/internal/analysis"
	"claude-session-health/internal/collect"
	"claude-session-health/internal/config"
	"claude-session-health/internal/herdr"
	"claude-session-health/internal/logx"
	"claude-session-health/internal/state"
)

var t0 = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)

// clock is a fake clock; real=true makes it follow the wall clock.
type clock struct {
	mu   sync.Mutex
	t    time.Time
	real bool
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.real {
		return time.Now()
	}
	return c.t
}

func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type proc struct {
	pid, ppid, pgid int
	rssMB           func(min float64) float64
	start           time.Time
	name            string
	cpuFrac         float64
}

// world simulates Herdr, the process table and system metrics.
type world struct {
	mu        sync.Mutex
	clock     *clock
	procs     map[int]*proc
	agents    []herdr.Agent
	info      map[string]herdr.ProcessInfo
	agentsErr error
	enabled   bool
	tokens    map[string][]*string
	notified  []string
	block     chan struct{}
	inFlight  atomic.Int32
	maxFlight atomic.Int32
}

func newWorld() *world {
	return &world{clock: &clock{t: t0}, procs: map[int]*proc{}, info: map[string]herdr.ProcessInfo{}, enabled: true, tokens: map[string][]*string{}}
}

func (w *world) Agents(context.Context) ([]herdr.Agent, error) {
	n := w.inFlight.Add(1)
	defer w.inFlight.Add(-1)
	for {
		m := w.maxFlight.Load()
		if n <= m || w.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	w.mu.Lock()
	block := w.block
	w.mu.Unlock()
	if block != nil {
		<-block
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]herdr.Agent(nil), w.agents...), w.agentsErr
}

func (w *world) ProcessInfo(_ context.Context, pane string) (herdr.ProcessInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, ok := w.info[pane]
	if !ok {
		return herdr.ProcessInfo{}, errors.New("no such pane")
	}
	return info, nil
}

func (w *world) ReportTokens(_ context.Context, pane string, tokens map[string]*string, _ time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tokens[pane] = append(w.tokens[pane], tokens[TokenName])
	return nil
}

func (w *world) PluginEnabled(context.Context, string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enabled, nil
}

func (w *world) Notify(_ context.Context, title, _ string) (bool, string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notified = append(w.notified, title)
	return true, "", nil
}

func (w *world) Snapshot(context.Context) (*collect.ProcTable, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.clock.Now()
	var b strings.Builder
	for _, p := range w.procs {
		el := now.Sub(p.start)
		fmt.Fprintf(&b, "%d %d %d %d 1.0 %s %s %s\n", p.pid, p.ppid, p.pgid, uint64(p.rssMB(now.Sub(t0).Minutes())*1024),
			psClock(time.Duration(float64(el)*p.cpuFrac)), psClock(el), p.name)
	}
	return collect.ParsePS([]byte(b.String()), now)
}

func (w *world) Sample(context.Context) collect.SystemSample {
	return collect.BuildSystemSample(w.clock.Now(),
		[]byte("hw.memsize: 34359738368\nvm.swapusage: total = 1024.00M  used = 2.00M  free = 1022.00M\nkern.memorystatus_vm_pressure_level: 1\nkern.memorystatus_level: 80\n"), nil)
}

func psClock(d time.Duration) string {
	total := d.Seconds()
	h := int(total) / 3600
	m := (int(total) % 3600) / 60
	return fmt.Sprintf("%02d:%02d:%05.2f", h, m, total-float64(h*3600+m*60))
}

func flat(mb float64) func(float64) float64 { return func(float64) float64 { return mb } }

// addSession creates a pane shell with a claude child (and optional extra processes).
func (w *world) addSession(pane, term string, shell, claude int, rss func(float64) float64, extra ...*proc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	start := w.clock.Now().Add(-time.Hour)
	w.procs[shell] = &proc{pid: shell, ppid: 1, pgid: shell, rssMB: flat(5), start: start, name: "zsh"}
	w.procs[claude] = &proc{pid: claude, ppid: shell, pgid: claude, rssMB: rss, start: start, name: "claude", cpuFrac: 0.1}
	group := []herdr.ForegroundProc{{PID: claude, Name: "claude", LooksLikeClaude: true}}
	for _, c := range extra {
		c.start = start
		w.procs[c.pid] = c
		group = append(group, herdr.ForegroundProc{PID: c.pid, Name: c.name})
	}
	w.info[pane] = herdr.ProcessInfo{PaneID: pane, ShellPID: shell, ForegroundPGID: claude, Procs: group}
	w.agents = append(w.agents, herdr.Agent{PaneID: pane, TerminalID: term, Agent: "claude", Status: "working", Cwd: "/tmp/" + term})
}

func newMonitor(t *testing.T, w *world, cfg config.Config, stateDir string) (*Monitor, *logx.Logger) {
	t.Helper()
	logPath := ""
	if stateDir != "" {
		logPath = filepath.Join(stateDir, state.LogFile)
	}
	log, err := logx.New(logPath, 1<<20, 200)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	m := New(cfg, nil, Deps{
		Herdr: w, Procs: w, System: w, Notifier: w, Log: log, Now: w.clock.Now,
		StateDir: stateDir, PluginID: "claude-session-health", Version: "test",
	})
	return m, log
}

func tickFor(t *testing.T, m *Monitor, w *world, d time.Duration) {
	t.Helper()
	for el := time.Duration(0); el < d; el += m.cfg.Interval() {
		if err := m.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		w.clock.Advance(m.cfg.Interval())
	}
}

func sessionView(t *testing.T, m *Monitor, pane string) state.Session {
	t.Helper()
	for _, s := range m.Snapshot(m.d.Now()).Sessions {
		if s.PaneID == pane {
			return s
		}
	}
	t.Fatalf("no session for %s", pane)
	return state.Session{}
}

// 7. Sessions are attributed and judged independently.
func TestSessionsAttributedIndependently(t *testing.T) {
	w := newWorld()
	w.addSession("w1:p1", "term_a", 10, 11, func(m float64) float64 { return 800 + 15*m },
		&proc{pid: 12, ppid: 11, pgid: 11, rssMB: flat(100), name: "node"})
	w.addSession("w1:p2", "term_b", 20, 21, flat(800),
		&proc{pid: 22, ppid: 21, pgid: 21, rssMB: flat(50), name: "node"},
		&proc{pid: 23, ppid: 1, pgid: 21, rssMB: flat(300), name: "node"}) // orphan in B's group
	m, _ := newMonitor(t, w, config.Default(), "")
	tickFor(t, m, w, 30*time.Minute)

	a, b := sessionView(t, m, "w1:p1"), sessionView(t, m, "w1:p2")
	if a.State != analysis.SustainedGrowth && a.State != analysis.SuspectedLeak {
		t.Errorf("growing session A state = %s (%v)", a.State, a.Reasons)
	}
	if b.State != analysis.Healthy {
		t.Errorf("stable session B state = %s (%v)", b.State, b.Reasons)
	}
	if b.RSSKB != (800+50)*1024 || b.ChildCount != 1 || b.OrphanCount != 1 || b.OrphanKB != 300*1024 {
		t.Errorf("B memory: total %d children %d orphans %d/%d", b.RSSKB/1024, b.ChildCount, b.OrphanCount, b.OrphanKB/1024)
	}
	if a.Confidence != "verified" || a.PID != 11 || a.ChildCount != 1 {
		t.Errorf("A attribution = %s pid %d children %d", a.Confidence, a.PID, a.ChildCount)
	}
	if m.Snapshot(w.clock.Now()).Sessions[0].PaneID != "w1:p1" {
		t.Error("sessions must be sorted by severity")
	}
	for _, title := range w.notified {
		if strings.Contains(title, "w1:p2") {
			t.Errorf("stable session B was alerted: %q", title)
		}
	}
	if len(w.notified) == 0 || !strings.Contains(w.notified[0], "w1:p1") {
		t.Errorf("growing session A was not alerted: %v", w.notified)
	}
	tokA, tokB := w.tokens["w1:p1"], w.tokens["w1:p2"]
	if last := *tokA[len(tokA)-1]; !strings.HasPrefix(last, "↗") && !strings.HasPrefix(last, "leak?") {
		t.Errorf("A sidebar token = %q", last)
	}
	if last := *tokB[len(tokB)-1]; last != "ok" {
		t.Errorf("B sidebar token = %q", last)
	}
	if a.CPUEstimated || a.CPUPercent < 5 || a.CPUPercent > 15 {
		t.Errorf("A CPU %.1f%% (estimated %t); want ~10%% from CPU-time deltas", a.CPUPercent, a.CPUEstimated)
	}
}

// sidebar_show_rss appends the current session size to the verdict.
func TestSidebarShowRSS(t *testing.T) {
	w := newWorld()
	w.addSession("w1:p1", "term_a", 10, 11, func(m float64) float64 { return 800 + 15*m })
	w.addSession("w1:p2", "term_b", 20, 21, flat(800),
		&proc{pid: 22, ppid: 21, pgid: 21, rssMB: flat(50), name: "node"})
	cfg := config.Default()
	cfg.SidebarShowRSS = true
	m, _ := newMonitor(t, w, cfg, "")

	tickFor(t, m, w, time.Minute)
	if tok := w.tokens["w1:p2"]; *tok[len(tok)-1] != "learning 850M" {
		t.Errorf("new session token = %q, want %q", *tok[len(tok)-1], "learning 850M")
	}

	tickFor(t, m, w, 29*time.Minute)
	if tok := w.tokens["w1:p2"]; *tok[len(tok)-1] != "ok 850M" {
		t.Errorf("stable session token = %q, want %q", *tok[len(tok)-1], "ok 850M")
	}
	a := sessionView(t, m, "w1:p1")
	want := " · " + short(float64(a.RSSKB))
	if tok := w.tokens["w1:p1"]; !strings.HasSuffix(*tok[len(tok)-1], want) ||
		(!strings.HasPrefix(*tok[len(tok)-1], "↗") && !strings.HasPrefix(*tok[len(tok)-1], "leak?")) {
		t.Errorf("growing session token = %q, want a growth verdict ending in %q", *tok[len(tok)-1], want)
	}
}

// 6 (end to end). A restart in the same pane resets the baseline and
// resolves alerts about the old process.
func TestRestartInSamePaneResetsBaseline(t *testing.T) {
	w := newWorld()
	w.addSession("w1:p1", "term_a", 10, 11, func(m float64) float64 { return 800 + 15*m })
	m, _ := newMonitor(t, w, config.Default(), "")
	tickFor(t, m, w, 30*time.Minute)
	if s := sessionView(t, m, "w1:p1"); s.State != analysis.SustainedGrowth {
		t.Fatalf("precondition: growth, got %s", s.State)
	}

	w.mu.Lock()
	delete(w.procs, 11)
	w.procs[31] = &proc{pid: 31, ppid: 10, pgid: 31, rssMB: flat(600), start: w.clock.Now(), name: "claude"}
	w.info["w1:p1"] = herdr.ProcessInfo{PaneID: "w1:p1", ShellPID: 10, ForegroundPGID: 31,
		Procs: []herdr.ForegroundProc{{PID: 31, Name: "claude", LooksLikeClaude: true}}}
	w.mu.Unlock()
	tickFor(t, m, w, time.Minute)

	s := sessionView(t, m, "w1:p1")
	if s.PID != 31 || s.Restarts != 1 || s.PeakKB != 600*1024 || s.State != analysis.InsufficientData {
		t.Errorf("after restart: pid %d restarts %d peak %d state %s", s.PID, s.Restarts, s.PeakKB/1024, s.State)
	}
	recovered := false
	for _, a := range m.Alerts().Recent() {
		if a.Kind == "recovery" && strings.Contains(a.Body, "restarted") {
			recovered = true
		}
	}
	if !recovered {
		t.Error("growth alert for the old process was not resolved")
	}
}

// Sampling failures are reported, never shown as current, and recover.
func TestSamplingFailureMarksStaleAndRecovers(t *testing.T) {
	w := newWorld()
	w.addSession("w1:p1", "term_a", 10, 11, flat(800))
	m, _ := newMonitor(t, w, config.Default(), "")
	tickFor(t, m, w, time.Minute)

	w.mu.Lock()
	w.agentsErr = errors.New("boom")
	w.mu.Unlock()
	tickFor(t, m, w, 20*time.Second)
	snap := m.Snapshot(w.clock.Now())
	if snap.Monitor.Status != state.StatusFailing || snap.Monitor.ConsecutiveFailures != 4 {
		t.Errorf("monitor status %s failures %d", snap.Monitor.Status, snap.Monitor.ConsecutiveFailures)
	}
	if s := snap.Sessions[0]; s.Sampled || !snap.SessionStale(s, w.clock.Now()) || !strings.Contains(s.SampleNote, "boom") {
		t.Errorf("session must be marked stale: sampled=%t note=%q", s.Sampled, s.SampleNote)
	}
	if !m.Alerts().Active("monitor:failed") {
		t.Error("monitor_failed alert not raised")
	}

	w.mu.Lock()
	w.agentsErr = nil
	w.mu.Unlock()
	tickFor(t, m, w, 10*time.Second)
	snap = m.Snapshot(w.clock.Now())
	if snap.Monitor.Status != state.StatusOK || !snap.Sessions[0].Sampled || m.Alerts().Active("monitor:failed") {
		t.Errorf("did not recover: %s sampled=%t", snap.Monitor.Status, snap.Sessions[0].Sampled)
	}
}

func TestVanishedClaudeIsStaleNotZero(t *testing.T) {
	w := newWorld()
	w.addSession("w1:p1", "term_a", 10, 11, flat(800))
	m, _ := newMonitor(t, w, config.Default(), "")
	tickFor(t, m, w, time.Minute)
	w.mu.Lock()
	delete(w.procs, 11)
	w.info["w1:p1"] = herdr.ProcessInfo{PaneID: "w1:p1", ShellPID: 10}
	w.mu.Unlock()
	tickFor(t, m, w, 30*time.Second)
	s := sessionView(t, m, "w1:p1")
	if s.Sampled || s.LastSampleAt.After(t0.Add(time.Minute)) {
		t.Errorf("vanished process reported as sampled: %+v", s)
	}
	sess, _ := m.Store().Session(s.Key)
	for _, x := range sess.Run.Samples() {
		if x.RSSKB == 0 {
			t.Fatal("a missing process produced a zero sample")
		}
	}
	if !m.Alerts().Active(s.Key + ":stale") {
		t.Error("stale alert not raised")
	}
}

func TestStopsWhenHerdrGoneOrPluginDisabled(t *testing.T) {
	w := newWorld()
	w.agentsErr = fmt.Errorf("%w: connection refused", herdr.ErrUnavailable)
	m, _ := newMonitor(t, w, config.Default(), "")
	var err error
	for i := 0; i < 100 && err == nil; i++ {
		err = m.Tick(context.Background())
		w.clock.Advance(5 * time.Second)
	}
	if !errors.Is(err, ErrHerdrGone) {
		t.Errorf("err = %v, want ErrHerdrGone", err)
	}

	// A disabled plugin stops the monitor, but only after consecutive checks,
	// so a transiently empty plugin list during a Herdr restart is tolerated.
	w2 := newWorld()
	w2.enabled = false
	m2, _ := newMonitor(t, w2, config.Default(), "")
	var stopAt time.Duration
	for el := time.Duration(0); el <= 2*time.Minute; el += 5 * time.Second {
		if err := m2.Tick(context.Background()); errors.Is(err, ErrDisabled) {
			stopAt = el
			break
		}
		w2.clock.Advance(5 * time.Second)
	}
	if stopAt < 60*time.Second || stopAt > 70*time.Second {
		t.Errorf("stopped at %s, want after 3 checks 30 s apart", stopAt)
	}
}

func TestBrieflyUnlistedSessionKeepsState(t *testing.T) {
	w := newWorld()
	w.addSession("w1:p1", "term_a", 10, 11, func(m float64) float64 { return 800 + 15*m })
	m, _ := newMonitor(t, w, config.Default(), "")
	tickFor(t, m, w, 30*time.Minute)
	if s := sessionView(t, m, "w1:p1"); s.State != analysis.SustainedGrowth {
		t.Fatalf("precondition: growth, got %s", s.State)
	}
	notified := len(w.notified)

	w.mu.Lock()
	agents := w.agents
	w.agents = nil
	w.mu.Unlock()
	tickFor(t, m, w, 30*time.Second)
	if s := sessionView(t, m, "w1:p1"); s.Sampled || s.State != analysis.SustainedGrowth {
		t.Errorf("during the glitch: sampled %t state %s", s.Sampled, s.State)
	}
	if m.Alerts().Active("term_a:stale") {
		t.Error("an unlisted pane must not raise a stale alert")
	}
	w.mu.Lock()
	w.agents = agents
	w.mu.Unlock()
	tickFor(t, m, w, time.Minute)
	if s := sessionView(t, m, "w1:p1"); !s.Sampled || s.State != analysis.SustainedGrowth {
		t.Errorf("after the glitch: sampled %t state %s", s.Sampled, s.State)
	}
	if len(w.notified) != notified {
		t.Errorf("listing glitch caused new notifications: %v", w.notified[notified:])
	}

	w.mu.Lock()
	w.agents = nil
	w.mu.Unlock()
	tickFor(t, m, w, 2*time.Minute)
	if len(m.Snapshot(w.clock.Now()).Sessions) != 0 {
		t.Error("a pane gone for longer than the grace period must be dropped")
	}
}

func TestBackwardClockStepStartsFreshRun(t *testing.T) {
	w := newWorld()
	w.addSession("w1:p1", "term_a", 10, 11, flat(800))
	m, _ := newMonitor(t, w, config.Default(), "")
	tickFor(t, m, w, 5*time.Minute)
	w.clock.Advance(-10 * time.Minute)
	tickFor(t, m, w, 30*time.Second)
	if s := sessionView(t, m, "w1:p1"); !s.Sampled {
		t.Errorf("samples rejected after a backward clock step: %q", s.SampleNote)
	}
}

// Samples can never overlap: a re-entrant Tick returns immediately.
func TestTicksNeverOverlap(t *testing.T) {
	w := newWorld()
	w.addSession("w1:p1", "term_a", 10, 11, flat(800))
	w.block = make(chan struct{})
	m, _ := newMonitor(t, w, config.Default(), "")
	done := make(chan struct{})
	go func() { _ = m.Tick(context.Background()); close(done) }()
	for w.inFlight.Load() == 0 {
		runtime.Gosched()
	}
	start := time.Now()
	if err := m.Tick(context.Background()); err != nil || time.Since(start) > time.Second {
		t.Errorf("overlapping tick: %v after %s", err, time.Since(start))
	}
	close(w.block)
	<-done
	if got := w.maxFlight.Load(); got != 1 {
		t.Errorf("max concurrent samples = %d, want 1", got)
	}
}

// 10. Memory stays bounded over a long run.
func TestLongRunStaysBounded(t *testing.T) {
	cfg := config.Default()
	cfg.RawRetentionMinutes = 5
	cfg.RetentionHours = 2
	w := newWorld()
	w.addSession("w1:p1", "term_a", 10, 11, func(m float64) float64 { return 800 + float64(int(m)%30)*10 })
	m, log := newMonitor(t, w, cfg, "")
	tickFor(t, m, w, 3*time.Hour)
	sess, _ := m.Store().Session(sessionView(t, m, "w1:p1").Key)
	if sess.Run.Raw.Len() > cfg.RawCapacity() || sess.Run.Minutes.Len() > cfg.MinuteCapacity() {
		t.Errorf("raw %d/%d minutes %d/%d", sess.Run.Raw.Len(), cfg.RawCapacity(), sess.Run.Minutes.Len(), cfg.MinuteCapacity())
	}
	if len(m.Alerts().Recent()) > recentAlertCap || len(log.Recent()) > 200 {
		t.Errorf("alerts %d log %d", len(m.Alerts().Recent()), len(log.Recent()))
	}
}

// 12. Run shuts down cleanly: returns promptly, persists history, publishes
// a stopped snapshot, clears sidebar tokens and leaks no goroutines.
func TestRunShutsDownCleanly(t *testing.T) {
	before := runtime.NumGoroutine()
	dir := t.TempDir()
	w := newWorld()
	w.clock = &clock{real: true}
	w.addSession("w1:p1", "term_a", 10, 11, flat(800))
	cfg := config.Default()
	cfg.SampleIntervalSec = 2
	m, log := newMonitor(t, w, cfg, dir)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, state.SnapshotFile)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no snapshot written")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop within 2s of cancellation")
	}
	snap, err := state.Read(filepath.Join(dir, state.SnapshotFile))
	if err != nil {
		t.Fatalf("final snapshot: %v", err)
	}
	if snap.Monitor.Status != state.StatusStopped || snap.Sessions[0].Sampled {
		t.Errorf("final snapshot status %s sampled %t", snap.Monitor.Status, snap.Sessions[0].Sampled)
	}
	if _, err := os.Stat(filepath.Join(dir, state.HistoryFile)); err != nil {
		t.Errorf("history not persisted: %v", err)
	}
	w.mu.Lock()
	toks := w.tokens["w1:p1"]
	w.mu.Unlock()
	if len(toks) == 0 || toks[len(toks)-1] != nil {
		t.Error("sidebar token not cleared on shutdown")
	}
	_ = log.Close()
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines: %d before, %d after", before, after)
	}
	data, _ := os.ReadFile(filepath.Join(dir, state.LogFile))
	if !strings.Contains(string(data), "monitor stopped") {
		t.Error("lifecycle not logged")
	}
}

func TestSnapshotHasNoCommandLines(t *testing.T) {
	w := newWorld()
	w.addSession("w1:p1", "term_a", 10, 11, flat(800))
	m, _ := newMonitor(t, w, config.Default(), "")
	tickFor(t, m, w, 10*time.Second)
	data, err := json.Marshal(m.Snapshot(w.clock.Now()))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"--dangerously", "Bearer", "terminal_title"} {
		if strings.Contains(string(data), bad) {
			t.Errorf("snapshot contains %q", bad)
		}
	}
}
