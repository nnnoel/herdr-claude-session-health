// Package diag renders copyable diagnostic reports. Reports contain only
// what the snapshot and log contain: no prompts, conversation content,
// terminal titles or command lines.
package diag

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"claude-session-health/internal/analysis"
	"claude-session-health/internal/state"
)

// Report is the full diagnostic report.
func Report(snap *state.Snapshot, snapErr error, logTail []string, env map[string]string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Claude Code Session Health — diagnostic report\n")
	fmt.Fprintf(&b, "generated: %s\n", now.Format(time.RFC3339))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, env[k])
	}
	if snap == nil {
		fmt.Fprintf(&b, "\nsnapshot: unavailable (%v)\n", snapErr)
	} else {
		fmt.Fprintf(&b, "\nsnapshot: version %s, platform %s, generated %s (%s ago)%s\n",
			snap.Version, snap.Platform, snap.GeneratedAt.Format(time.RFC3339), snap.Age(now).Round(time.Second),
			staleTag(snap.IsStale(now)))
		mon := snap.Monitor
		fmt.Fprintf(&b, "monitor: status=%s pid=%d started=%s ticks=%d last_tick_ms=%d failures=%d herdr=%s\n",
			mon.Status, mon.DaemonPID, fmtTime(mon.StartedAt), mon.Ticks, mon.LastTickMs,
			mon.ConsecutiveFailures, orDash(mon.HerdrVersion))
		if mon.LastError != "" {
			fmt.Fprintf(&b, "monitor last error: %s\n", mon.LastError)
		}
		fmt.Fprintf(&b, "history: %d sessions retained, persisted %s%s\n", mon.SessionsInHistory,
			fmtTime(mon.HistoryPersistedAt), errTag(mon.PersistError))
		if len(snap.ConfigWarnings) > 0 {
			fmt.Fprintf(&b, "config warnings:\n  - %s\n", strings.Join(snap.ConfigWarnings, "\n  - "))
		}
		if cfg, err := json.Marshal(snap.Config); err == nil {
			fmt.Fprintf(&b, "config: %s\n", cfg)
		}
		if sys := snap.System; sys != nil {
			fmt.Fprintf(&b, "\nsystem: status=%s pressure=%s reasons=%q\n", sys.Status, sys.Sample.Pressure, sys.Reasons)
			fmt.Fprintf(&b, "  total=%s available≈%s compressed=%s swap_used=%s swap_total=%s free_pct=%s\n",
				bytesP(sys.Sample.TotalBytes), bytesP(sys.Sample.AvailableBytes), bytesP(sys.Sample.CompressedBytes),
				bytesP(sys.Sample.SwapUsedBytes), bytesP(sys.Sample.SwapTotalBytes), intP(sys.Sample.FreePercent))
			if len(sys.Sample.Unavailable) > 0 {
				fmt.Fprintf(&b, "  unavailable: %s\n", strings.Join(sys.Sample.Unavailable, ", "))
			}
			if len(sys.Sample.Errors) > 0 {
				fmt.Fprintf(&b, "  errors: %s\n", strings.Join(sys.Sample.Errors, "; "))
			}
		}
		fmt.Fprintf(&b, "\nsessions (%d):\n", len(snap.Sessions))
		for _, s := range snap.Sessions {
			b.WriteString(indent(SessionReport(snap, s, now), "  "))
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "recent alerts (%d):\n", len(snap.Alerts))
		for _, a := range snap.Alerts {
			status := "notified"
			if !a.Notified {
				status = "not notified: " + a.Suppressed
			}
			fmt.Fprintf(&b, "  %s [%s/%s] %s — %s (%s)\n", a.At.Format(time.RFC3339), a.Kind, a.Level, a.Title, a.Body, status)
		}
	}
	fmt.Fprintf(&b, "\nlog tail (%d lines):\n", len(logTail))
	for _, l := range logTail {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	return b.String()
}

// SessionReport describes one session.
func SessionReport(snap *state.Snapshot, s state.Session, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session %s pane=%s key=%s\n", orDash(s.SessionID), s.PaneID, s.Key)
	fmt.Fprintf(&b, "  cwd=%s pid=%d process=%s run_started=%s restarts=%d\n", s.Cwd, s.PID, orDash(s.ProcessName),
		fmtTime(s.RunStartedAt), s.Restarts)
	fmt.Fprintf(&b, "  state=%s since=%s reasons=%q", s.State, fmtTime(s.StateSince), s.Reasons)
	if s.HoldNote != "" {
		fmt.Fprintf(&b, " hold=%q", s.HoldNote)
	}
	b.WriteString("\n")
	fresh := "current"
	if snap.SessionStale(s, now) {
		fresh = "STALE — not current"
	}
	fmt.Fprintf(&b, "  last_sample=%s (%s)%s\n", fmtTime(s.LastSampleAt), fresh, noteTag(s.SampleNote))
	fmt.Fprintf(&b, "  rss=%s claude=%s children=%s (%d procs) peak=%s baseline=%s cpu=%.1f%%%s\n",
		analysis.MB(float64(s.RSSKB)), analysis.MB(float64(s.ClaudeKB)), analysis.MB(float64(s.ChildrenKB)), s.ChildCount,
		analysis.MB(float64(s.PeakKB)), baseline(s), s.CPUPercent, estTag(s.CPUEstimated))
	fmt.Fprintf(&b, "  attribution=%s notes=%q orphans=%d (%s, not counted)\n", orDash(s.Confidence), s.AttributionNotes,
		s.OrphanCount, analysis.MB(float64(s.OrphanKB)))
	for _, w := range []analysis.Window{s.Short, s.Long} {
		fmt.Fprintf(&b, "  window %dm: points=%d/%d rise=%s (%+.0f%%) slope=%s/min up=%.0f%% sustained=%t — %s\n",
			w.Minutes, w.Points, w.Expected, analysis.MB(w.RiseKB), w.RisePct, analysis.MB(w.SlopeKBPerMin),
			w.UpFraction*100, w.Sustained, w.Why)
	}
	fmt.Fprintf(&b, "  agent=%s idle=%t last_activity=%s above_warn=%ds above_crit=%ds\n", orDash(s.AgentStatus), s.Idle,
		fmtTime(s.LastActivityAt), s.AboveWarnSec, s.AboveCritSec)
	if s.Resume.Command != "" {
		fmt.Fprintf(&b, "  resume: %s (verified=%t; %s)\n", s.Resume.Command, s.Resume.Verified, s.Resume.Note)
	} else {
		fmt.Fprintf(&b, "  resume: unavailable (%s)\n", s.Resume.Note)
	}
	return b.String()
}

func baseline(s state.Session) string {
	if !s.BaselineReady {
		return "pending"
	}
	return analysis.MB(float64(s.BaselineKB))
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return prefix + strings.Join(lines, "\n"+prefix)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

func bytesP(v *uint64) string {
	if v == nil {
		return "unavailable"
	}
	return analysis.MB(float64(*v) / 1024)
}

func intP(v *int) string {
	if v == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%d%%", *v)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func staleTag(stale bool) string {
	if stale {
		return " — STALE: the monitor is not updating"
	}
	return ""
}

func errTag(e string) string {
	if e == "" {
		return ""
	}
	return " (last save error: " + e + ")"
}

func noteTag(n string) string {
	if n == "" {
		return ""
	}
	return fmt.Sprintf(" note=%q", n)
}

func estTag(est bool) string {
	if est {
		return " (ps estimate)"
	}
	return ""
}
