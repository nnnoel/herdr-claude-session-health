package diag

import (
	"errors"
	"strings"
	"testing"
	"time"

	"claude-session-health/internal/analysis"
	"claude-session-health/internal/config"
	"claude-session-health/internal/state"
)

var now = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)

func TestReportContents(t *testing.T) {
	snap := &state.Snapshot{
		SchemaVersion: state.SchemaVersion, Version: "0.1.0", GeneratedAt: now.Add(-10 * time.Minute), Config: config.Default(),
		Monitor:  state.MonitorStatus{Status: state.StatusOK},
		Sessions: []state.Session{{Key: "k", PaneID: "w1:p1", PID: 5, State: analysis.Healthy, Sampled: true, LastSampleAt: now.Add(-10 * time.Minute)}},
	}
	out := Report(snap, nil, []string{"log line"}, map[string]string{"b": "2", "a": "1"}, now)
	for _, want := range []string{"STALE", "STALE — not current", "pane=w1:p1", "log line", "config:", "a: 1\nb: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	none := Report(nil, errors.New("no snapshot"), nil, nil, now)
	if !strings.Contains(none, "unavailable (no snapshot)") {
		t.Errorf("missing snapshot report:\n%s", none)
	}
}
