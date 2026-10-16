// Package logx is a small size-bounded logger. The log file rotates to a
// single ".1" backup, so disk use never exceeds roughly twice maxBytes, and a
// fixed number of recent lines stays in memory for diagnostics.
//
// Callers must never log prompts, conversation content, terminal titles,
// process command lines or secrets. Process names, PIDs, pane IDs and
// numeric measurements are fine.
package logx

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"claude-session-health/internal/ring"
)

const maxLine = 512

// Categories used across the plugin so log lines are easy to filter.
const (
	Lifecycle   = "lifecycle"
	Discovery   = "discovery"
	Attribution = "attribution"
	Sampling    = "sampling"
	Persistence = "persistence"
	State       = "state"
	Alert       = "alert"
	Config      = "config"
)

// Logger writes timestamped, single-line entries. A nil *Logger discards everything.
type Logger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
	recent   *ring.Ring[string]
	now      func() time.Time
}

// New opens (or creates) the log at path. An empty path keeps lines in memory only.
func New(path string, maxBytes int64, recentLines int) (*Logger, error) {
	if maxBytes < 4096 {
		maxBytes = 4096
	}
	l := &Logger{path: path, maxBytes: maxBytes, recent: ring.New[string](recentLines), now: time.Now}
	if path == "" {
		return l, nil
	}
	if err := l.open(); err != nil {
		return nil, fmt.Errorf("open log %s: %w", path, err)
	}
	return l, nil
}

func (l *Logger) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.file, l.size = f, info.Size()
	return nil
}

// Logf records one line under category.
func (l *Logger) Logf(category, format string, args ...any) {
	if l == nil {
		return
	}
	msg := strings.NewReplacer("\n", " ", "\r", " ").Replace(fmt.Sprintf(format, args...))
	if len(msg) > maxLine {
		msg = msg[:maxLine] + "…"
	}
	line := fmt.Sprintf("%s [%s] %s", l.now().UTC().Format(time.RFC3339), category, msg)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.recent.Push(line)
	if l.file == nil {
		return
	}
	if l.size+int64(len(line))+1 > l.maxBytes {
		l.rotate()
		if l.file == nil {
			return
		}
	}
	n, _ := l.file.WriteString(line + "\n")
	l.size += int64(n)
}

func (l *Logger) rotate() {
	_ = l.file.Close()
	l.file = nil
	_ = os.Rename(l.path, l.path+".1")
	if err := l.open(); err != nil {
		l.file = nil
	}
}

// Recent returns the newest in-memory lines, oldest first.
func (l *Logger) Recent() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recent.Slice()
}

// Close closes the log file.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
