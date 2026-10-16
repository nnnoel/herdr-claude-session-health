// Package state defines the snapshot the daemon publishes for the UI and
// diagnostics, plus the file layout of the plugin's state directory.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"claude-session-health/internal/alerting"
	"claude-session-health/internal/analysis"
	"claude-session-health/internal/attribution"
	"claude-session-health/internal/config"
	"claude-session-health/internal/resume"
	"claude-session-health/internal/systemhealth"
)

// SchemaVersion is bumped on incompatible snapshot changes.
const SchemaVersion = 1

// File names inside the per-socket state directory.
const (
	SnapshotFile = "snapshot.json"
	HistoryFile  = "history.json"
	LogFile      = "monitor.log"
	StderrFile   = "daemon.stderr"
	PIDFile      = "daemon.pid"
	LockFile     = "daemon.lock"
)

// Monitor status values.
const (
	StatusStarting = "starting"
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StatusFailing  = "failing"
	StatusStopped  = "stopped"
)

// Snapshot is everything the UI renders.
type Snapshot struct {
	SchemaVersion  int                      `json:"schema_version"`
	Version        string                   `json:"version"`
	Platform       string                   `json:"platform"`
	GeneratedAt    time.Time                `json:"generated_at"`
	Monitor        MonitorStatus            `json:"monitor"`
	System         *systemhealth.Assessment `json:"system"`
	Sessions       []Session                `json:"sessions"`
	Alerts         []alerting.Alert         `json:"alerts"`
	Config         config.Config            `json:"config"`
	ConfigWarnings []string                 `json:"config_warnings,omitempty"`
}

// MonitorStatus describes the sampling loop itself.
type MonitorStatus struct {
	Status              string    `json:"status"`
	DaemonPID           int       `json:"daemon_pid"`
	StartedAt           time.Time `json:"started_at"`
	LastTickAt          time.Time `json:"last_tick_at"`
	LastSuccessAt       time.Time `json:"last_success_at"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastError           string    `json:"last_error,omitempty"`
	LastTickMs          int64     `json:"last_tick_ms"`
	Ticks               uint64    `json:"ticks"`
	HerdrVersion        string    `json:"herdr_version,omitempty"`
	HistoryPersistedAt  time.Time `json:"history_persisted_at"`
	PersistError        string    `json:"persist_error,omitempty"`
	SessionsInHistory   int       `json:"sessions_in_history"`
}

// Session is one live Claude Code session.
type Session struct {
	Key              string              `json:"key"`
	PaneID           string              `json:"pane_id"`
	SessionID        string              `json:"session_id,omitempty"`
	Cwd              string              `json:"cwd"`
	AgentStatus      string              `json:"agent_status"`
	Idle             bool                `json:"idle"`
	LastActivityAt   time.Time           `json:"last_activity_at"`
	PID              int                 `json:"pid"`
	ProcessName      string              `json:"process_name"` // display name
	ProcessComm      string              `json:"process_comm"` // ps ucomm token, re-checked before terminating
	RunStartedAt     time.Time           `json:"run_started_at"`
	FirstSeen        time.Time           `json:"first_seen"`
	Restarts         int                 `json:"restarts"`
	Sampled          bool                `json:"sampled"`
	LastSampleAt     time.Time           `json:"last_sample_at"`
	SampleNote       string              `json:"sample_note,omitempty"`
	RSSKB            uint64              `json:"rss_kb"`
	ClaudeKB         uint64              `json:"claude_kb"`
	ChildrenKB       uint64              `json:"children_kb"`
	PeakKB           uint64              `json:"peak_kb"`
	PeakAt           time.Time           `json:"peak_at"`
	BaselineKB       uint64              `json:"baseline_kb"`
	BaselineReady    bool                `json:"baseline_ready"`
	CPUPercent       float64             `json:"cpu_percent"`
	CPUEstimated     bool                `json:"cpu_estimated"`
	ChildCount       int                 `json:"child_count"`
	TopChildren      []attribution.Child `json:"top_children,omitempty"`
	OrphanCount      int                 `json:"orphan_count"`
	OrphanKB         uint64              `json:"orphan_kb"`
	Confidence       string              `json:"confidence"`
	AttributionNotes []string            `json:"attribution_notes,omitempty"`
	State            analysis.State      `json:"state"`
	StateLabel       string              `json:"state_label"`
	StateSince       time.Time           `json:"state_since"`
	Reasons          []string            `json:"reasons"`
	HoldNote         string              `json:"hold_note,omitempty"`
	Trend            analysis.Trend      `json:"trend"`
	Short            analysis.Window     `json:"short_window"`
	Long             analysis.Window     `json:"long_window"`
	AboveWarnSec     int64               `json:"above_warn_sec"`
	AboveCritSec     int64               `json:"above_crit_sec"`
	BudgetExceeded   bool                `json:"budget_exceeded"`
	SparkMinutes     []float64           `json:"spark_minutes"` // MB per minute, oldest first; -1 = no data
	SparkRaw         []float64           `json:"spark_raw"`     // MB per raw sample, oldest first
	Resume           resume.Info         `json:"resume"`
	Severity         int                 `json:"severity"`
}

// Dir returns the per-Herdr-socket state directory under root, so separate
// Herdr sessions never share a daemon, lock or history.
func Dir(root, socketPath string) string {
	return filepath.Join(root, fmt.Sprintf("%016x", fnv1a([]byte(socketPath))))
}

// fnv1a keeps directory names stable across Go versions.
func fnv1a(b []byte) uint64 {
	h := uint64(0xcbf29ce484222325)
	for _, c := range b {
		h ^= uint64(c)
		h *= 0x100000001b3
	}
	return h
}

// Read loads a snapshot file.
func Read(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if s.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("snapshot schema %d is not supported (want %d); restart the monitor", s.SchemaVersion, SchemaVersion)
	}
	return &s, nil
}

// Age is how old the snapshot is relative to now.
func (s *Snapshot) Age(now time.Time) time.Duration { return now.Sub(s.GeneratedAt) }

// StaleAfter is how old data may be before it must not be shown as current.
func (s *Snapshot) StaleAfter() time.Duration {
	interval := time.Duration(s.Config.SampleIntervalSec) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return 3*interval + 2*time.Second
}

// IsStale reports whether the snapshot itself is too old to be current.
func (s *Snapshot) IsStale(now time.Time) bool { return s.Age(now) > s.StaleAfter() }

// SessionStale reports whether a session's last sample is too old to be current.
func (s *Snapshot) SessionStale(sess Session, now time.Time) bool {
	return !sess.Sampled || sess.LastSampleAt.IsZero() || now.Sub(sess.LastSampleAt) > s.StaleAfter()
}
