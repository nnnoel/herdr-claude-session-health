// Package ui is the dashboard shown in a Herdr plugin pane. Rendering and
// key handling are pure functions of a Model so they can be tested; app.go
// owns the terminal and side effects.
package ui

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"claude-session-health/internal/alerting"
	"claude-session-health/internal/analysis"
	"claude-session-health/internal/state"
	"claude-session-health/internal/systemhealth"
)

type style string

const (
	plain   style = ""
	bold    style = "\x1b[1m"
	dim     style = "\x1b[2m"
	red     style = "\x1b[31m"
	green   style = "\x1b[32m"
	yellow  style = "\x1b[33m"
	magenta style = "\x1b[35m"
	cyan    style = "\x1b[36m"
	inverse style = "\x1b[7m"
	reset         = "\x1b[0m"
)

type seg struct {
	text string
	st   style
}

// Line is a sequence of styled segments.
type Line []seg

func ln(parts ...seg) Line { return Line(parts) }

func s(text string, st style) seg { return seg{text: sanitize(text), st: st} }

// Render writes the line clipped to width visible columns.
func (l Line) Render(width int) string {
	var b strings.Builder
	used := 0
	for _, sg := range l {
		if used >= width {
			break
		}
		r := []rune(sg.text)
		if used+len(r) > width {
			r = r[:width-used]
		}
		if sg.st != plain {
			b.WriteString(string(sg.st))
		}
		b.WriteString(string(r))
		if sg.st != plain {
			b.WriteString(reset)
		}
		used += len(r)
	}
	return b.String()
}

// Plain is the unstyled text (tests).
func (l Line) Plain() string {
	var b strings.Builder
	for _, sg := range l {
		b.WriteString(sg.text)
	}
	return b.String()
}

// sanitize removes control characters so data such as directory names can
// never inject terminal escape sequences.
func sanitize(t string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, t)
}

func stateStyle(st analysis.State) style {
	switch st {
	case analysis.SuspectedLeak:
		return red
	case analysis.SustainedGrowth:
		return yellow
	case analysis.Elevated:
		return magenta
	case analysis.Healthy:
		return green
	default:
		return dim
	}
}

func stateTag(st analysis.State) string {
	switch st {
	case analysis.SuspectedLeak:
		return "leak?"
	case analysis.SustainedGrowth:
		return "growth"
	case analysis.Elevated:
		return "high"
	case analysis.Healthy:
		return "ok"
	default:
		return "learning"
	}
}

// Render produces the screen for the model.
func Render(m Model) []Line {
	switch {
	case m.Snap == nil:
		return renderNoData(m)
	case m.View == ViewDetail:
		return renderDetail(m)
	case m.View == ViewConfirm:
		return renderConfirm(m)
	case m.View == ViewHelp:
		return renderHelp(m)
	default:
		return renderList(m)
	}
}

func renderNoData(m Model) []Line {
	lines := []Line{
		ln(s("Claude Session Health", bold)),
		ln(s("No monitor data: "+m.SnapErr, red)),
		ln(),
		ln(s("The background monitor may not be running. Invoke the plugin action", plain)),
		ln(s("\"Start Claude session health monitor\", or run: claude-session-health start", plain)),
	}
	return append(lines, flashLine(m), ln(s("[q] quit", dim)))
}

func header(m Model) []Line {
	snap := m.Snap
	var status seg
	switch {
	case snap.IsStale(m.Now):
		status = s(fmt.Sprintf("STALE: no update for %s — monitor not running?", analysis.Dur(snap.Age(m.Now))), red)
	case snap.Monitor.Status == state.StatusOK:
		status = s("● live", green)
	case snap.Monitor.Status == state.StatusStopped:
		status = s("monitor stopped", red)
	case snap.Monitor.Status == state.StatusFailing:
		status = s("● sampling failing: "+snap.Monitor.LastError, red)
	default:
		status = s("● "+snap.Monitor.Status, yellow)
	}
	attention := 0
	for _, x := range snap.Sessions {
		if x.Severity >= analysis.Elevated.Severity() {
			attention++
		}
	}
	lines := []Line{ln(
		s("Claude Session Health ", bold),
		s(fmt.Sprintf("· %d sessions · %d need attention · sampled %s ago · ", len(snap.Sessions), attention,
			analysis.Dur(m.Now.Sub(snap.Monitor.LastSuccessAt))), dim),
		status,
	)}
	lines = append(lines, systemLines(m)...)
	for _, w := range snap.ConfigWarnings {
		lines = append(lines, ln(s("config: "+w, yellow)))
	}
	return lines
}

func systemLines(m Model) []Line {
	sys := m.Snap.System
	if sys == nil {
		return []Line{ln(s("System: waiting for the first sample", dim))}
	}
	st := map[systemhealth.Status]style{systemhealth.OK: green, systemhealth.Warn: yellow, systemhealth.Critical: red}[sys.Status]
	if st == plain {
		st = dim
	}
	sm := sys.Sample
	parts := []string{"pressure " + string(sm.Pressure)}
	if sm.AvailableBytes != nil && sm.TotalBytes != nil {
		parts = append(parts, fmt.Sprintf("avail≈%s/%s", gb(*sm.AvailableBytes), gb(*sm.TotalBytes)))
	} else if sm.TotalBytes != nil {
		parts = append(parts, "RAM "+gb(*sm.TotalBytes)+" (available: unavailable)")
	}
	if sm.CompressedBytes != nil {
		parts = append(parts, "compressed "+gb(*sm.CompressedBytes))
	}
	if sm.SwapUsedBytes != nil {
		swap := "swap " + analysis.MB(float64(*sm.SwapUsedBytes)/1024)
		if sys.SwapKnown {
			swap += fmt.Sprintf(" (%+.0f MB/%s)", float64(sys.SwapGrowthBytes)/(1<<20), analysis.Dur(sys.SwapCovered))
		}
		parts = append(parts, swap)
	} else {
		parts = append(parts, "swap unavailable")
	}
	tag := s("System ", bold)
	if m.Snap.IsStale(m.Now) {
		st, tag = dim, s("System (stale) ", dim)
	}
	lines := []Line{ln(tag, s("● "+string(sys.Status)+" ", st), s(strings.Join(parts, " · "), plain))}
	if sys.Status == systemhealth.Warn || sys.Status == systemhealth.Critical {
		lines = append(lines, ln(s("  "+strings.Join(sys.Reasons, "; "), st)))
	}
	return lines
}

func gb(b uint64) string { return fmt.Sprintf("%.1f GB", float64(b)/(1<<30)) }

func short(kb uint64) string {
	if kb >= 1024*1024 {
		return fmt.Sprintf("%.1fG", float64(kb)/(1024*1024))
	}
	return fmt.Sprintf("%.0fM", float64(kb)/1024)
}

func renderList(m Model) []Line {
	lines := header(m)
	w := m.Width
	sparkW := w - 67
	lines = append(lines, ln(s(strings.Repeat("─", max(0, w)), dim)))
	hdr := fmt.Sprintf("  %-10s %-7s %-12s %6s %6s %5s ", "STATE", "PANE", "DIR", "RSS", "PEAK", "CPU")
	if sparkW >= 8 {
		hdr += fmt.Sprintf("%-*s ", sparkW, "LAST 60 MIN")
	}
	lines = append(lines, ln(s(hdr+"SAMPLE", dim)))
	if len(m.Snap.Sessions) == 0 {
		lines = append(lines, ln(s("  No Claude Code sessions detected in this Herdr session.", dim)))
	}

	var rows []Line
	selRow := 0
	for i, x := range m.Snap.Sessions {
		if i == m.Selected {
			selRow = len(rows)
		}
		rows = append(rows, sessionRow(m, x, i == m.Selected, sparkW))
		if i == m.Selected || x.Severity >= analysis.Elevated.Severity() || !x.Sampled {
			rows = append(rows, ln(s("      "+firstReason(x), dim)))
		}
	}
	footer := listFooter(m)
	avail := m.Height - len(lines) - len(footer)
	if avail > 0 && len(rows) > avail {
		start := min(max(0, selRow-avail/2), len(rows)-avail)
		rows = rows[start : start+avail]
	}
	lines = append(lines, rows...)
	return append(lines, footer...)
}

func firstReason(x state.Session) string {
	r := ""
	if len(x.Reasons) > 0 {
		r = x.Reasons[0]
	}
	if x.HoldNote != "" {
		r += " (" + x.HoldNote + ")"
	}
	if !x.Sampled && x.SampleNote != "" {
		r = "not sampled: " + x.SampleNote
	}
	return r
}

func sessionRow(m Model, x state.Session, selected bool, sparkW int) Line {
	marker := "  "
	if selected {
		marker = "> "
	}
	stale := m.Snap.IsStale(m.Now) || m.Snap.SessionStale(x, m.Now)
	tag, st := stateTag(x.State), stateStyle(x.State)
	val := plain
	if stale {
		tag, st, val = "stale", red, dim
	}
	dir := filepath.Base(x.Cwd)
	cpu := fmt.Sprintf("%.0f%%", x.CPUPercent)
	row := ln(
		s(marker, bold),
		s(fmt.Sprintf("● %-8s ", tag), st),
		s(fmt.Sprintf("%-7s %-12s ", clip(x.PaneID, 7), clip(dir, 12)), plain),
		s(fmt.Sprintf("%6s %6s %5s ", short(x.RSSKB), short(x.PeakKB), cpu), val),
	)
	if sparkW >= 8 {
		row = append(row, s(fmt.Sprintf("%-*s ", sparkW, Sparkline(x.SparkMinutes, sparkW)), cyan))
	}
	age := "never"
	if !x.LastSampleAt.IsZero() {
		age = analysis.Dur(m.Now.Sub(x.LastSampleAt))
	}
	if stale {
		row = append(row, s(age+" ago", red))
	} else {
		row = append(row, s(age, dim))
	}
	if selected {
		row[0].st = inverse
	}
	return row
}

func listFooter(m Model) []Line {
	var lines []Line
	alerts := m.Snap.Alerts
	shown := 0
	for i := len(alerts) - 1; i >= 0 && shown < 3; i-- {
		lines = append(lines, alertLine(alerts[i]))
		shown++
	}
	lines = append(lines, flashLine(m))
	lines = append(lines, ln(s("[↑↓] select [enter] details [f] focus [c] copy session diag [d] full diag [r] copy resume [x] terminate [?] help [q] quit", dim)))
	return lines
}

func alertLine(a alerting.Alert) Line {
	st := map[alerting.Level]style{alerting.Critical: red, alerting.Warning: yellow, alerting.Info: green}[a.Level]
	note := ""
	if !a.Notified {
		st, note = dim, " (not notified: "+a.Suppressed+")"
	}
	return ln(s(a.At.Local().Format("15:04")+" ", dim), s(a.Title, st), s(note, dim))
}

func flashLine(m Model) Line {
	if m.Flash == "" || m.Now.Sub(m.FlashAt) > 6*time.Second {
		return ln()
	}
	return ln(s(m.Flash, bold))
}

// Sparkline draws values (MB; negative = no data) in width cells, scaled
// between the min and max. Near-flat series render flat so noise is not
// exaggerated.
func Sparkline(vals []float64, width int) string {
	if width <= 0 {
		return ""
	}
	if len(vals) > width {
		vals = resample(vals, width)
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range vals {
		if v >= 0 {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	ticks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range vals {
		switch {
		case v < 0:
			b.WriteRune(' ')
		case hi-lo < math.Max(1, 0.02*hi):
			b.WriteRune(ticks[3])
		default:
			b.WriteRune(ticks[int((v-lo)/(hi-lo)*float64(len(ticks)-1)+0.5)])
		}
	}
	return b.String()
}

// resample keeps the newest values, averaging buckets of valid points.
func resample(vals []float64, width int) []float64 {
	out := make([]float64, width)
	per := float64(len(vals)) / float64(width)
	for i := range out {
		from, to := int(float64(i)*per), int(float64(i+1)*per)
		sum, n := 0.0, 0
		for _, v := range vals[from:min(to, len(vals))] {
			if v >= 0 {
				sum += v
				n++
			}
		}
		out[i] = -1
		if n > 0 {
			out[i] = sum / float64(n)
		}
	}
	return out
}

func clip(t string, n int) string {
	r := []rune(t)
	if len(r) <= n {
		return t
	}
	return string(r[:n-1]) + "…"
}

func renderDetail(m Model) []Line {
	x, ok := m.selected()
	if !ok {
		return []Line{ln(s("Session no longer exists. [esc] back", dim))}
	}
	snap := m.Snap
	stale := snap.IsStale(m.Now) || snap.SessionStale(x, m.Now)
	var body []Line
	add := func(parts ...seg) { body = append(body, ln(parts...)) }
	section := func(title string) {
		add(s("─ "+title+" ", bold), s(strings.Repeat("─", max(0, m.Width-len(title)-3)), dim))
	}

	add(s(fmt.Sprintf("Session %s · %s", x.PaneID, filepath.Base(x.Cwd)), bold), s("   ", plain),
		s(x.StateLabel, stateStyle(x.State)), s(" since "+fmtClock(x.StateSince), dim))
	if stale {
		add(s(fmt.Sprintf("STALE: last sample %s (%s ago). Values below are not current.", fmtClock(x.LastSampleAt),
			analysis.Dur(m.Now.Sub(x.LastSampleAt))), red))
		if x.SampleNote != "" {
			add(s("  "+x.SampleNote, red))
		}
	}
	for i, r := range x.Reasons {
		label := "Reason: "
		if i > 0 {
			label = "        "
		}
		add(s(label, dim), s(r, plain))
	}
	if x.HoldNote != "" {
		add(s("Hold:   ", dim), s(x.HoldNote, yellow))
	}
	add(s("A suspected leak is a heuristic flag, never a certainty.", dim))

	section("Process")
	add(s(fmt.Sprintf("PID %d (%s) · started %s (%s ago) · restarts in this pane %d", x.PID, orDash(x.ProcessName),
		fmtClock(x.RunStartedAt), analysis.Dur(m.Now.Sub(x.RunStartedAt)), x.Restarts), plain))
	add(s(fmt.Sprintf("Session id %s · agent %s · idle %t · last activity %s", orDash(x.SessionID), orDash(x.AgentStatus),
		x.Idle, fmtClock(x.LastActivityAt)), plain))
	add(s("Cwd "+x.Cwd, plain))
	conf := stateStyle(analysis.Healthy)
	if x.Confidence != "verified" {
		conf = yellow
	}
	add(s("Attribution ", dim), s(orDash(x.Confidence), conf), s(" — "+strings.Join(x.AttributionNotes, "; "), plain))

	section("Memory (summed RSS; shared pages are counted once per process)")
	add(s(fmt.Sprintf("Total %s = claude %s + %d children %s · peak %s at %s · baseline %s",
		analysis.MB(float64(x.RSSKB)), analysis.MB(float64(x.ClaudeKB)), x.ChildCount, analysis.MB(float64(x.ChildrenKB)),
		analysis.MB(float64(x.PeakKB)), fmtClock(x.PeakAt), baseline(x)), plain))
	cpuNote := "from CPU-time delta"
	if x.CPUEstimated {
		cpuNote = "ps estimate"
	}
	add(s(fmt.Sprintf("CPU %.1f%% (%s, summed over the tree; can exceed 100%%)", x.CPUPercent, cpuNote), plain))
	if len(x.TopChildren) > 0 {
		var kids []string
		for _, c := range x.TopChildren {
			kids = append(kids, fmt.Sprintf("%s %s (pid %d)", c.Name, analysis.MB(float64(c.RSSKB)), c.PID))
		}
		add(s("Top children: "+strings.Join(kids, ", "), plain))
	}
	if x.OrphanCount > 0 {
		add(s(fmt.Sprintf("%d other process(es) in the pane's process group, %s, are not under claude and are not counted",
			x.OrphanCount, analysis.MB(float64(x.OrphanKB))), yellow))
	}

	section("History")
	sparkW := max(10, min(60, m.Width-24))
	add(s("Last 60 min  ", dim), s(Sparkline(x.SparkMinutes, sparkW), cyan), s(" "+rangeOf(x.SparkMinutes), dim))
	add(s(fmt.Sprintf("Last %-3d smp ", len(x.SparkRaw)), dim), s(Sparkline(x.SparkRaw, sparkW), cyan), s(" "+rangeOf(x.SparkRaw), dim))

	section("Growth")
	for _, w := range []analysis.Window{x.Short, x.Long} {
		verdict, st := "not sustained", plain
		if w.Sustained {
			verdict, st = "SUSTAINED", yellow
		}
		add(s(fmt.Sprintf("%3d min: %s (%+.0f%%) · slope %s/h · %.0f%% minutes up · coverage %.0f%% · ", w.Minutes,
			analysis.MB(w.RiseKB), w.RisePct, analysis.MB(w.SlopeKBPerMin*60), w.UpFraction*100, w.Coverage*100), plain),
			s(verdict, st))
		add(s("         "+w.Why, dim))
	}
	add(s(fmt.Sprintf("Time above warning: %s · above critical budget: %s · trend %s",
		analysis.Dur(time.Duration(x.AboveWarnSec)*time.Second), analysis.Dur(time.Duration(x.AboveCritSec)*time.Second), x.Trend), plain))

	section("Thresholds")
	c := snap.Config
	add(s(fmt.Sprintf("warn %d MB · critical %d MB (after %.0f min) · growth ≥ %d MB and ≥ %.0f%% · %.0f%% minutes up",
		c.WarnRSSMB, c.CriticalRSSMB, c.ElevatedMinMinutes, c.MinGrowthMB, c.MinGrowthPercent, c.MinUpFraction*100), plain))
	add(s(fmt.Sprintf("windows %d/%d min · recovery %.0f min · idle after %.0f min · cooldown %.0f min · verbosity %s",
		c.GrowthWindowMinutes, c.LeakWindowMinutes, c.RecoveryMinutes, c.IdleAfterMinutes, c.AlertCooldownMinutes, c.Verbosity), plain))

	section("Recent alerts")
	n := 0
	for i := len(snap.Alerts) - 1; i >= 0 && n < 5; i-- {
		if snap.Alerts[i].SessionKey == x.Key {
			body = append(body, alertLine(snap.Alerts[i]))
			n++
		}
	}
	if n == 0 {
		add(s("none", dim))
	}

	section("Resume")
	if x.Resume.Command != "" {
		st := green
		if !x.Resume.Verified {
			st = yellow
		}
		add(s(x.Resume.Command, st))
	}
	add(s(x.Resume.Note, dim))

	footer := []Line{flashLine(m), ln(s("[esc] back [↑↓] scroll [f] focus pane [c] copy diag [i] copy session id [r] copy resume [x] terminate", dim))}
	avail := max(1, m.Height-len(footer))
	start := min(max(0, m.Scroll), max(0, len(body)-avail))
	if len(body) > avail {
		body = body[start : start+avail]
	}
	return append(body, footer...)
}

func rangeOf(vals []float64) string {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range vals {
		if v >= 0 {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	if math.IsInf(lo, 1) {
		return "no data"
	}
	return fmt.Sprintf("%s – %s", analysis.MB(lo*1024), analysis.MB(hi*1024))
}

func baseline(x state.Session) string {
	if !x.BaselineReady {
		return "pending"
	}
	return analysis.MB(float64(x.BaselineKB))
}

func fmtClock(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("15:04:05")
}

func orDash(t string) string {
	if t == "" {
		return "—"
	}
	return t
}

func renderConfirm(m Model) []Line {
	t := m.Pending
	lines := []Line{
		ln(s("Terminate Claude Code session?", bold+red)),
		ln(),
		ln(s(fmt.Sprintf("Pane %s · PID %d (%s) · started %s · RSS %s", t.PaneID, t.Target.PID, orDash(t.Name),
			fmtClock(t.Target.Start), analysis.MB(float64(t.RSSKB))), plain)),
		ln(s("Cwd "+t.Cwd, plain)),
		ln(),
		ln(s("This sends SIGTERM to that one process. The Claude Code session in the pane stops and", plain)),
		ln(s("any in-progress response or tool run is lost. Its child processes (MCP servers, tools)", plain)),
		ln(s("normally exit with it. The PID and start time are re-checked first, so a reused PID is", plain)),
		ln(s("never signalled. Nothing is restarted automatically.", plain)),
		ln(),
	}
	if t.Resume != "" {
		lines = append(lines, ln(s("Afterwards the conversation can usually be resumed with:", plain)), ln(s("  "+t.Resume, green)), ln())
	}
	lines = append(lines,
		ln(s(fmt.Sprintf("Type the PID (%d) and press Enter to confirm, or Esc to cancel: ", t.Target.PID), bold), s(m.Confirm+"_", yellow)),
		flashLine(m))
	return lines
}

func renderHelp(m Model) []Line {
	lines := []Line{
		ln(s("Claude Session Health — help", bold)),
		ln(),
		ln(s("States", bold)),
		ln(s("  ok        Healthy: no sustained growth, below the warning threshold", green)),
		ln(s("  learning  Insufficient data: not enough history for the growth window yet", dim)),
		ln(s("  high      Elevated memory: above the warning threshold for the minimum duration", magenta)),
		ln(s("  growth    Sustained growth: steady, still-rising RSS over the growth window", yellow)),
		ln(s("  leak?     Suspected leak: sustained over the leak window, or steady growth while idle", red)),
		ln(s("  stale     No current sample: values shown are old and not current", red)),
		ln(),
		ln(s("Keys", bold)),
		ln(s("  ↑/↓ or j/k select · enter details · esc back · f focus the session's pane", plain)),
		ln(s("  c copy session diagnostics · d copy full diagnostic report · i copy session id", plain)),
		ln(s("  r copy resume command · x terminate (asks for the PID) · q quit", plain)),
		ln(),
		ln(s("RSS is summed per process, so shared pages are counted more than once.", dim)),
		ln(s("Warnings are heuristics; a suspected leak is never certain.", dim)),
		ln(),
		ln(s("[esc] back", dim)),
	}
	return lines
}
