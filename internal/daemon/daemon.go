//go:build darwin || linux

// Package daemon manages the background monitor process: a per-session
// exclusive lock (so reloads and repeated startup hooks never create a second
// loop), a PID file, detached spawning, and verified stopping.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"claude-session-health/internal/fsutil"
	"claude-session-health/internal/state"
)

// PIDInfo is written by the running daemon.
type PIDInfo struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	Exe       string    `json:"exe"`
	StartedAt time.Time `json:"started_at"`
}

// Lock is a held exclusive lock.
type Lock struct{ f *os.File }

// TryLock takes the daemon lock without blocking. ok is false when another
// process holds it.
func TryLock(dir string) (*Lock, bool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, fmt.Errorf("create state dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, state.LockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock: %w", err)
	}
	return &Lock{f: f}, true, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		_ = l.f.Close()
		l.f = nil
	}
}

// Running reports whether a daemon currently holds the lock.
func Running(dir string) bool {
	l, ok, err := TryLock(dir)
	if err != nil {
		return false
	}
	if ok {
		l.Release()
		return false
	}
	return true
}

// WritePID records the current process.
func WritePID(dir, version, exe string) error {
	data, err := json.Marshal(PIDInfo{PID: os.Getpid(), Version: version, Exe: exe, StartedAt: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("encode pid file: %w", err)
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, state.PIDFile), data, 0o600)
}

// ReadPID reads the PID file.
func ReadPID(dir string) (PIDInfo, error) {
	data, err := os.ReadFile(filepath.Join(dir, state.PIDFile))
	if err != nil {
		return PIDInfo{}, fmt.Errorf("read pid file: %w", err)
	}
	var info PIDInfo
	if err := json.Unmarshal(data, &info); err != nil || info.PID <= 0 {
		return PIDInfo{}, errors.New("invalid pid file")
	}
	return info, nil
}

// RemovePID deletes the PID file if it still names this process.
func RemovePID(dir string) {
	if info, err := ReadPID(dir); err == nil && info.PID == os.Getpid() {
		_ = os.Remove(filepath.Join(dir, state.PIDFile))
	}
}

// Start launches a detached daemon (`exe run`) unless one with the same
// version and executable is already running. A daemon from another version
// or path is stopped first, so plugin updates replace the old loop.
func Start(dir, exe, version string) (string, error) {
	if Running(dir) {
		info, err := ReadPID(dir)
		if err == nil && info.Version == version && info.Exe == exe {
			return fmt.Sprintf("monitor already running (pid %d)", info.PID), nil
		}
		if _, err := Stop(dir, 5*time.Second); err != nil {
			return "", fmt.Errorf("replace running monitor: %w", err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	stderr, err := os.OpenFile(filepath.Join(dir, state.StderrFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("open daemon stderr: %w", err)
	}
	defer stderr.Close()
	cmd := exec.Command(exe, "run")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, stderr, stderr
	// Startup hooks are one-shot commands; the monitor must outlive them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("spawn monitor: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return fmt.Sprintf("monitor started (pid %d)", pid), nil
}

// sameExecutable compares a ps name with the expected executable name.
// Linux truncates comm to 15 characters, so a 15-character name matches
// as a prefix.
func sameExecutable(psName, exe string) bool {
	if psName == exe {
		return true
	}
	return len(psName) == 15 && strings.HasPrefix(exe, psName)
}

// TryLockWait retries TryLock briefly, so a momentary probe by Running in
// another process cannot make a freshly started daemon give up.
func TryLockWait(dir string, attempts int, delay time.Duration) (*Lock, bool, error) {
	for i := 0; ; i++ {
		l, ok, err := TryLock(dir)
		if err != nil || ok || i >= attempts-1 {
			return l, ok, err
		}
		time.Sleep(delay)
	}
}

// Stop terminates the running daemon after verifying that the PID still
// belongs to this plugin's executable. It waits up to wait for the lock to
// be released.
func Stop(dir string, wait time.Duration) (string, error) {
	if !Running(dir) {
		_ = os.Remove(filepath.Join(dir, state.PIDFile))
		return "monitor is not running", nil
	}
	info, err := ReadPID(dir)
	if err != nil {
		return "", fmt.Errorf("monitor holds the lock but its pid file is unreadable: %w", err)
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(info.PID)).Output()
	if err != nil {
		return "", fmt.Errorf("pid %d from the pid file is not running: %w", info.PID, err)
	}
	if name := filepath.Base(strings.TrimSpace(string(out))); !sameExecutable(name, filepath.Base(info.Exe)) {
		return "", fmt.Errorf("pid %d is %q, not this plugin's monitor; refusing to signal it", info.PID, name)
	}
	if err := syscall.Kill(info.PID, syscall.SIGTERM); err != nil {
		return "", fmt.Errorf("signal monitor pid %d: %w", info.PID, err)
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !Running(dir) {
			return fmt.Sprintf("monitor stopped (pid %d)", info.PID), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("monitor pid %d did not exit within %s", info.PID, wait)
}
