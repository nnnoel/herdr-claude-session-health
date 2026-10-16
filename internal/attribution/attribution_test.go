package attribution

import (
	"errors"
	"strings"
	"testing"
	"time"

	"claude-session-health/internal/collect"
)

// table builds a process table from "pid ppid pgid rssKB name" rows.
func table(t *testing.T, rows ...string) *collect.ProcTable {
	t.Helper()
	var b strings.Builder
	for _, r := range rows {
		f := strings.Fields(r)
		b.WriteString(strings.Join([]string{f[0], f[1], f[2], f[3], "1.0", "0:01.00", "10:00", f[4]}, " "))
		b.WriteString("\n")
	}
	tab, err := collect.ParsePS([]byte(b.String()), time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

func TestSelectRootConfidence(t *testing.T) {
	tab := table(t,
		"1 0 1 100 launchd",
		"10 1 10 100 zsh", // pane A shell
		"11 10 11 4000 claude",
		"20 1 20 100 zsh", // pane B shell
		"21 20 21 100 caffeinate",
		"22 21 21 4000 claude", // behind a wrapper
		"30 1 30 100 zsh",      // pane C shell
		"40 1 40 4000 claude",  // not under pane C's shell
	)
	tests := []struct {
		name string
		pp   PaneProcesses
		pid  int
		conf Confidence
	}{
		{"direct child", PaneProcesses{ShellPID: 10, ForegroundPGID: 11, ClaudePIDs: []int{11}}, 11, Verified},
		{"wrapper", PaneProcesses{ShellPID: 20, ForegroundPGID: 21, ClaudePIDs: []int{22}}, 22, Probable},
		{"not under shell", PaneProcesses{ShellPID: 30, ForegroundPGID: 40, ClaudePIDs: []int{40}}, 40, Uncertain},
		{"name fallback", PaneProcesses{ShellPID: 10, ForegroundPGID: 11}, 11, Probable},
		{"other group", PaneProcesses{ShellPID: 10, ForegroundPGID: 99, ClaudePIDs: []int{11}}, 11, Probable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := SelectRoot(tt.pp, tab)
			if err != nil {
				t.Fatalf("SelectRoot: %v", err)
			}
			if r.PID != tt.pid || r.Confidence != tt.conf {
				t.Errorf("root = %d %s (%v), want %d %s", r.PID, r.Confidence, r.Notes, tt.pid, tt.conf)
			}
			if len(r.Notes) == 0 {
				t.Error("every attribution should explain itself")
			}
		})
	}
	if _, err := SelectRoot(PaneProcesses{ShellPID: 30, ClaudePIDs: []int{999}}, tab); !errors.Is(err, ErrNoClaude) {
		t.Errorf("vanished pid: err = %v", err)
	}
}

func TestSelectRootPrefersOutermostClaude(t *testing.T) {
	tab := table(t,
		"10 1 10 100 zsh",
		"11 10 11 4000 claude",
		"12 11 11 100 bash",
		"13 12 11 3000 claude", // claude started by claude's own tool
	)
	r, err := SelectRoot(PaneProcesses{ShellPID: 10, ForegroundPGID: 11, ClaudePIDs: []int{11, 13}}, tab)
	if err != nil || r.PID != 11 || r.Confidence != Verified {
		t.Fatalf("root = %+v, %v", r, err)
	}
	if !strings.Contains(strings.Join(r.Notes, ";"), "nested") {
		t.Errorf("notes should mention the nested claude: %v", r.Notes)
	}
	u := Measure(map[string]int{"a": 11}, tab)["a"]
	if u.TotalRSSKB() != 4000+100+3000 || u.ChildCount != 2 {
		t.Errorf("nested claude must be counted once as a child: %+v", u)
	}
}

func TestSelectRootAmbiguous(t *testing.T) {
	tab := table(t,
		"10 1 10 100 zsh",
		"11 10 11 4000 claude",
		"12 10 12 4000 claude",
	)
	r, err := SelectRoot(PaneProcesses{ShellPID: 10, ForegroundPGID: 12, ClaudePIDs: []int{11, 12}}, tab)
	if err != nil || r.PID != 12 || r.Confidence != Uncertain {
		t.Errorf("ambiguous pane must be uncertain and prefer the foreground group: %+v, %v", r, err)
	}
}

// Sessions are attributed independently and no PID is ever counted twice,
// even with nested sessions, cycles, duplicated roots or shared children.
func TestMeasureIndependentAndNoDoubleCounting(t *testing.T) {
	tab := table(t,
		"10 1 10 100 zsh",
		"11 10 11 1000 claude", // session A
		"12 11 11 200 node",
		"13 12 11 50 sh",
		"20 1 20 100 zsh",
		"21 20 21 2000 claude", // session B
		"22 21 21 300 node",
		"30 11 30 5000 claude", // session C's root lives inside A's tree
		"31 30 30 400 node",
		"50 51 50 10 loop-a", // ppid cycle, unrelated
		"51 50 50 10 loop-b",
	)
	got := Measure(map[string]int{"A": 11, "B": 21, "C": 30, "D": 11}, tab)

	if a := got["A"]; a.TotalRSSKB() != 1000+200+50 || a.ChildCount != 2 || a.PIDs[30] || a.PIDs[31] {
		t.Errorf("A = total %d children %d pids %v", a.TotalRSSKB(), a.ChildCount, a.PIDs)
	}
	if b := got["B"]; b.TotalRSSKB() != 2000+300 || b.ChildCount != 1 {
		t.Errorf("B = %+v", b)
	}
	if c := got["C"]; c.TotalRSSKB() != 5000+400 {
		t.Errorf("C = %+v", c)
	}
	if d := got["D"]; !d.Missing || d.TotalRSSKB() != 0 {
		t.Errorf("duplicate root D must not be counted again: %+v", d)
	}
	seen := map[int]string{}
	for key, u := range got {
		for pid := range u.PIDs {
			if other, dup := seen[pid]; dup {
				t.Errorf("pid %d counted by %s and %s", pid, key, other)
			}
			seen[pid] = key
		}
	}
	if !strings.Contains(strings.Join(got["A"].Notes, ";"), "root of session C") {
		t.Errorf("A should note that C's root was excluded: %v", got["A"].Notes)
	}
	if u := Measure(map[string]int{"Z": 50}, tab)["Z"]; u.TotalRSSKB() != 20 {
		t.Errorf("cycle walk = %d, want 20", u.TotalRSSKB())
	}
}

func TestMeasureVanishedRoot(t *testing.T) {
	tab := table(t, "10 1 10 100 zsh")
	if u := Measure(map[string]int{"x": 999}, tab)["x"]; !u.Missing {
		t.Errorf("vanished root should be Missing: %+v", u)
	}
}

func TestOrphansReportedNotCounted(t *testing.T) {
	tab := table(t,
		"10 1 10 100 zsh",
		"11 10 11 1000 claude",
		"12 11 11 200 node",
		"13 1 11 700 node", // same process group, re-parented to launchd
	)
	u := Measure(map[string]int{"a": 11}, tab)["a"]
	n, kb := Orphans([]int{11, 12, 13, 13, 999}, u, tab)
	if n != 1 || kb != 700 {
		t.Errorf("orphans = %d, %d KB; want 1, 700", n, kb)
	}
	if u.TotalRSSKB() != 1200 {
		t.Errorf("orphans must not be added to the session: %d", u.TotalRSSKB())
	}
}
