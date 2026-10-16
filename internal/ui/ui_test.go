package ui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"claude-session-health/internal/analysis"
	"claude-session-health/internal/collect"
	"claude-session-health/internal/config"
	"claude-session-health/internal/resume"
	"claude-session-health/internal/state"
)

var now = time.Date(2026, 10, 16, 10, 30, 0, 0, time.UTC)

func snapshot(age time.Duration) *state.Snapshot {
	return &state.Snapshot{
		SchemaVersion: state.SchemaVersion, GeneratedAt: now.Add(-age), Config: config.Default(),
		Monitor: state.MonitorStatus{Status: state.StatusOK, LastSuccessAt: now.Add(-age)},
		Sessions: []state.Session{
			{Key: "a", PaneID: "w1:p1", Cwd: "/tmp/leaky", PID: 4242, ProcessName: "claude", RunStartedAt: now.Add(-time.Hour),
				State: analysis.SuspectedLeak, StateLabel: "Suspected leak", Severity: 4, Sampled: true, LastSampleAt: now.Add(-age),
				RSSKB: 3 << 20, PeakKB: 3 << 20, Reasons: []string{"possible leak: RSS rose 1.2 GB steadily over 60 min"},
				SparkMinutes: []float64{-1, 100, 200, 300}, Resume: resume.Info{Command: "claude --resume x", Verified: true}},
			{Key: "b", PaneID: "w1:p2", Cwd: "/tmp/\x1b[31mevil", PID: 77, ProcessName: "claude", RunStartedAt: now.Add(-time.Hour),
				State: analysis.Healthy, StateLabel: "Healthy", Sampled: true, LastSampleAt: now.Add(-age), Reasons: []string{"flat"}},
		},
	}
}

func render(m Model) string {
	var b strings.Builder
	for _, l := range Render(m) {
		b.WriteString(l.Plain())
		b.WriteString("\n")
	}
	return b.String()
}

func TestListShowsSessionsBySeverityWithReasons(t *testing.T) {
	m := Model{Now: now, Width: 140, Height: 40}
	m.SetSnapshot(snapshot(2*time.Second), nil)
	out := render(m)
	for _, want := range []string{"● live", "leak?", "w1:p1", "possible leak: RSS rose", "ok", "need attention"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "w1:p1") > strings.Index(out, "w1:p2") {
		t.Error("the leaking session should be listed first")
	}
}

// Old data is never presented as current.
func TestStaleSnapshotIsLabelled(t *testing.T) {
	m := Model{Now: now, Width: 140, Height: 40}
	m.SetSnapshot(snapshot(5*time.Minute), nil)
	out := render(m)
	if !strings.Contains(out, "STALE: no update for 5m") || !strings.Contains(out, "stale") || strings.Contains(out, "● live") {
		t.Errorf("stale snapshot not labelled:\n%s", out)
	}
	m.View = ViewDetail
	if !strings.Contains(render(m), "Values below are not current") {
		t.Error("detail view must flag stale values")
	}
	fresh := Model{Now: now, Width: 140, Height: 40}
	snap := snapshot(time.Second)
	snap.Sessions[1].Sampled = false
	snap.Sessions[1].SampleNote = "claude process exited"
	fresh.SetSnapshot(snap, nil)
	if !strings.Contains(render(fresh), "not sampled: claude process exited") {
		t.Error("unsampled session must show why")
	}
}

func TestNoDataAndReadErrors(t *testing.T) {
	m := Model{Now: now, Width: 100, Height: 20}
	m.SetSnapshot(nil, errors.New("read snapshot: no such file"))
	if out := render(m); !strings.Contains(out, "No monitor data") || !strings.Contains(out, "start") {
		t.Errorf("no-data screen:\n%s", out)
	}
	m.SetSnapshot(snapshot(time.Second), nil)
	m.SetSnapshot(nil, errors.New("decode snapshot: truncated"))
	if m.Snap == nil || m.SnapErr == "" {
		t.Error("a transient read error must keep the last snapshot (shown as stale by age)")
	}
}

func TestRenderSanitizesTerminalEscapes(t *testing.T) {
	m := Model{Now: now, Width: 140, Height: 40}
	m.SetSnapshot(snapshot(time.Second), nil)
	for _, view := range []View{ViewList, ViewDetail} {
		m.View = view
		for _, l := range Render(m) {
			for _, sg := range l {
				if strings.ContainsRune(sg.text, 0x1b) {
					t.Fatalf("raw escape in rendered text: %q", sg.text)
				}
			}
		}
	}
}

func TestRenderClipsToWidth(t *testing.T) {
	l := Line{s("abcdef", bold), s("ghij", plain)}
	if got := l.Render(8); got != string(bold)+"abcdef"+reset+"gh" {
		t.Errorf("Render(8) = %q", got)
	}
	if got := l.Render(0); got != "" {
		t.Errorf("Render(0) = %q", got)
	}
}

func TestSparkline(t *testing.T) {
	if got := Sparkline([]float64{-1, 0, 50, 100}, 4); got != " ▁▅█" {
		t.Errorf("sparkline = %q", got)
	}
	if got := Sparkline([]float64{800, 801, 800}, 3); got != "▄▄▄" {
		t.Errorf("near-flat series must render flat: %q", got)
	}
	if got := []rune(Sparkline(make([]float64, 60), 20)); len(got) != 20 {
		t.Errorf("resampled width = %d", len(got))
	}
}

func TestParseKeys(t *testing.T) {
	got := ParseKeys([]byte("j\x1b[A\x1b[B\r\x1b\x7fq\x03x"))
	want := []string{"j", "up", "down", "enter", "esc", "backspace", "q", "ctrl-c", "x"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ParseKeys = %v, want %v", got, want)
	}
}

// Terminating requires typing the exact PID; nothing happens otherwise.
func TestTerminateRequiresTypedPID(t *testing.T) {
	m := &Model{Now: now, Width: 120, Height: 40}
	m.SetSnapshot(snapshot(time.Second), nil)
	if act := m.HandleKey("x"); act != ActNone || m.View != ViewConfirm || m.Pending.Target.PID != 4242 {
		t.Fatalf("x should open the confirmation: view %d pending %+v", m.View, m.Pending)
	}
	out := render(*m)
	for _, want := range []string{"PID 4242", "SIGTERM", "Type the PID", "claude --resume x"} {
		if !strings.Contains(out, want) {
			t.Errorf("confirmation lacks %q", want)
		}
	}
	for _, k := range []string{"4", "2", "4", "enter"} {
		if act := m.HandleKey(k); act == ActTerminate {
			t.Fatal("wrong PID confirmed termination")
		}
	}
	if m.View != ViewConfirm || !strings.Contains(m.Flash, "does not match") {
		t.Errorf("mismatch must stay on the confirmation: view %d flash %q", m.View, m.Flash)
	}
	var act Action
	for _, k := range []string{"4", "2", "4", "2", "enter"} {
		act = m.HandleKey(k)
	}
	if act != ActTerminate || m.View != ViewList {
		t.Errorf("exact PID: act %d view %d", act, m.View)
	}
	m.HandleKey("x")
	m.HandleKey("esc")
	if m.View != ViewList || !strings.Contains(m.Flash, "cancelled") {
		t.Error("esc must cancel")
	}
}

type fakeRunner struct{ out string }

func (f fakeRunner) Run(context.Context, string, ...string) ([]byte, error) {
	if f.out == "" {
		return nil, errors.New("exit status 1")
	}
	return []byte(f.out), nil
}

// Termination is refused whenever the PID cannot be re-verified, so a
// reused PID is never signalled. These cases never send a signal.
func TestTerminateRefusesUnverifiedTargets(t *testing.T) {
	target := Target{PID: 999999, Start: now.Add(-time.Hour), Name: "claude"}
	cases := map[string]fakeRunner{
		"gone":           {out: ""},
		"reused pid":     {out: "999999 05:00 claude\n"},
		"other program":  {out: "999999 01:00:00 python3\n"},
		"garbage output": {out: "nonsense\n"},
	}
	for name, r := range cases {
		if err := Terminate(context.Background(), r, target, now); err == nil {
			t.Errorf("%s: terminate succeeded", name)
		}
	}
	if err := Terminate(context.Background(), fakeRunner{}, Target{PID: 1, Start: now}, now); err == nil {
		t.Error("pid 1 must never be signalled")
	}
}

// The success path is exercised only against a synthetic child process
// that this test starts itself.
func TestTerminateSignalsVerifiedSyntheticChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start synthetic child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	runner := collect.ExecRunner{Timeout: 3 * time.Second}
	started := time.Now()
	time.Sleep(1100 * time.Millisecond) // etime has one-second resolution
	err := Terminate(context.Background(), runner, Target{PID: cmd.Process.Pid, Start: started.Truncate(time.Second), Name: "sleep"}, time.Now())
	if err != nil {
		t.Fatalf("terminate verified child: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit after SIGTERM")
	}
}
