//go:build darwin || linux

package ui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"claude-session-health/internal/collect"
	"claude-session-health/internal/diag"
	"claude-session-health/internal/fsutil"
	"claude-session-health/internal/state"
)

// App runs the dashboard in the current terminal. It only reads the
// daemon's snapshot, so a slow or stopped daemon never blocks the UI.
type App struct {
	SnapshotPath string
	LogPath      string
	DiagEnv      map[string]string
	Focus        func(ctx context.Context, paneID string) error
	Runner       collect.Runner
}

// Run blocks until the user quits or the pane is closed.
func (a *App) Run(ctx context.Context) error {
	fd := int(os.Stdin.Fd())
	restore, err := makeRaw(fd)
	if err != nil {
		return fmt.Errorf("the dashboard needs a terminal: %w", err)
	}
	defer restore()
	out := bufio.NewWriterSize(os.Stdout, 64*1024)
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l")
	defer func() {
		fmt.Fprint(out, "\x1b[?25h\x1b[?1049l")
		_ = out.Flush()
	}()

	keys := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			select {
			case keys <- append([]byte(nil), buf[:n]...):
			case <-ctx.Done():
				return
			}
		}
	}()
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	m := &Model{Now: time.Now()}
	reload := func() { snap, err := state.Read(a.SnapshotPath); m.SetSnapshot(snap, err) }
	reload()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		m.Now = time.Now()
		m.Width, m.Height = 100, 30
		if w, h, err := termSize(fd); err == nil && w > 0 && h > 0 {
			m.Width, m.Height = w, h
		}
		draw(out, m)
		select {
		case <-ctx.Done():
			return nil
		case sig := <-sigs:
			if sig != syscall.SIGWINCH {
				return nil
			}
		case <-ticker.C:
			reload()
		case b, ok := <-keys:
			if !ok {
				return nil
			}
			m.Now = time.Now()
			for _, k := range ParseKeys(b) {
				act := m.HandleKey(k)
				if act == ActQuit {
					return nil
				}
				a.perform(ctx, m, act)
			}
		}
	}
}

func draw(out *bufio.Writer, m *Model) {
	lines := Render(*m)
	if len(lines) > m.Height {
		lines = lines[:m.Height]
	}
	fmt.Fprint(out, "\x1b[H")
	for i, l := range lines {
		fmt.Fprint(out, l.Render(m.Width), "\x1b[K")
		if i < len(lines)-1 {
			fmt.Fprint(out, "\r\n")
		}
	}
	fmt.Fprint(out, "\x1b[J")
	_ = out.Flush()
}

func (a *App) perform(ctx context.Context, m *Model, act Action) {
	if act == ActNone {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if act == ActCopyAll {
		a.copy(ctx, m, diag.Report(m.Snap, nil, a.logTail(), a.DiagEnv, time.Now()), "Copied full diagnostic report.")
		return
	}
	if act == ActTerminate {
		t := m.Pending
		if err := Terminate(ctx, a.Runner, t.Target, time.Now()); err != nil {
			m.FlashMessage("Not terminated: " + err.Error())
			return
		}
		msg := fmt.Sprintf("Sent SIGTERM to PID %d.", t.Target.PID)
		if t.Resume != "" {
			msg += " Resume with: " + t.Resume
		}
		m.FlashMessage(msg)
		return
	}
	x, ok := m.Selection()
	if !ok || m.Snap == nil {
		return
	}
	switch act {
	case ActFocus:
		if err := a.Focus(ctx, x.PaneID); err != nil {
			m.FlashMessage("Focus failed: " + err.Error())
		} else {
			m.FlashMessage("Focused " + x.PaneID + ".")
		}
	case ActCopySession:
		a.copy(ctx, m, diag.SessionReport(m.Snap, x, time.Now()), "Copied session diagnostics.")
	case ActCopyID:
		if x.SessionID == "" {
			m.FlashMessage("Herdr has not reported a session id for this pane.")
			return
		}
		a.copy(ctx, m, x.SessionID, "Copied session id "+x.SessionID+".")
	case ActCopyResume:
		if x.Resume.Command == "" {
			m.FlashMessage("No resume command: " + x.Resume.Note)
			return
		}
		msg := "Copied verified resume command."
		if !x.Resume.Verified {
			msg = "Copied resume command (unverified: " + x.Resume.Note + ")."
		}
		a.copy(ctx, m, x.Resume.Command, msg)
	}
}

func (a *App) copy(ctx context.Context, m *Model, text, ok string) {
	if err := CopyToClipboard(ctx, text); err != nil {
		m.FlashMessage("Copy failed: " + err.Error())
		return
	}
	m.FlashMessage(ok)
}

func (a *App) logTail() []string {
	data, err := fsutil.ReadTail(a.LogPath, 64*1024)
	if err != nil {
		return []string{"log unavailable: " + err.Error()}
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 1 {
		lines = lines[1:] // the first line may be cut mid-way
	}
	if len(lines) > 100 {
		lines = lines[len(lines)-100:]
	}
	return lines
}
