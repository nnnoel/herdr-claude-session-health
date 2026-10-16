//go:build darwin || linux

package ui

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"claude-session-health/internal/collect"
)

// Target is the process the user asked to terminate, as shown on screen.
type Target struct {
	PID   int
	Start time.Time
	Name  string
}

// Terminate re-verifies the target against the live process table and only
// then sends SIGTERM. It refuses if the PID now belongs to another process
// (different start time or executable), so a reused PID is never signalled.
func Terminate(ctx context.Context, runner collect.Runner, t Target, now time.Time) error {
	if t.PID <= 1 || t.Start.IsZero() {
		return fmt.Errorf("no verified process to terminate")
	}
	out, err := runner.Run(ctx, "ps", "-o", "pid=,etime=,ucomm=", "-p", strconv.Itoa(t.PID))
	if err != nil {
		return fmt.Errorf("PID %d is no longer running", t.PID)
	}
	f := strings.Fields(string(out))
	if len(f) < 3 || f[0] != strconv.Itoa(t.PID) {
		return fmt.Errorf("could not verify PID %d (unexpected ps output)", t.PID)
	}
	elapsed, err := collect.ParseClock(f[1])
	if err != nil {
		return fmt.Errorf("could not verify PID %d: %w", t.PID, err)
	}
	if start := now.Add(-elapsed).Truncate(time.Second); !collect.SameStart(start, t.Start) {
		return fmt.Errorf("PID %d now belongs to a different process (start time changed); not terminating", t.PID)
	}
	if name := collect.ProcName(f[2]); t.Name != "" && name != t.Name {
		return fmt.Errorf("PID %d is now %q, not %q; not terminating", t.PID, name, t.Name)
	}
	if err := syscall.Kill(t.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("send SIGTERM to PID %d: %w", t.PID, err)
	}
	return nil
}

// CopyToClipboard copies text with the platform clipboard tool.
func CopyToClipboard(ctx context.Context, text string) error {
	for _, tool := range clipboardTools {
		path, err := exec.LookPath(tool[0])
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		cmd := exec.CommandContext(ctx, path, tool[1:]...)
		cmd.Stdin = strings.NewReader(text)
		err = cmd.Run()
		cancel()
		if err != nil {
			return fmt.Errorf("%s failed: %w", tool[0], err)
		}
		return nil
	}
	return fmt.Errorf("no clipboard tool found (tried %d)", len(clipboardTools))
}
