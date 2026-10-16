//go:build darwin || linux

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-session-health/internal/state"
)

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	l1, ok, err := TryLock(dir)
	if err != nil || !ok {
		t.Fatalf("first lock: %v %v", ok, err)
	}
	if _, ok, _ := TryLock(dir); ok {
		t.Fatal("second lock acquired while the first is held: duplicate loops possible")
	}
	if !Running(dir) {
		t.Error("Running should report the held lock")
	}
	l1.Release()
	if Running(dir) {
		t.Error("lock still held after release")
	}
	l2, ok, _ := TryLock(dir)
	if !ok {
		t.Fatal("lock not re-acquirable after release")
	}
	l2.Release()
}

func TestStopRefusesForeignProcess(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := TryLock(dir)
	defer l.Release()
	// The PID file names this test process but a different executable;
	// Stop must refuse instead of signalling it.
	data, _ := json.Marshal(PIDInfo{PID: os.Getpid(), Exe: "/nonexistent/claude-session-health"})
	if err := os.WriteFile(filepath.Join(dir, state.PIDFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Stop(dir, 0); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("Stop = %v, want refusal", err)
	}
}

func TestStopWhenNotRunning(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, state.PIDFile), []byte(`{"pid":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, err := Stop(dir, 0)
	if err != nil || !strings.Contains(msg, "not running") {
		t.Errorf("Stop = %q, %v", msg, err)
	}
	if _, err := os.Stat(filepath.Join(dir, state.PIDFile)); !os.IsNotExist(err) {
		t.Error("stale pid file should be removed")
	}
}

func TestSameExecutable(t *testing.T) {
	for _, tc := range []struct {
		ps, exe string
		want    bool
	}{
		{"claude-session-health", "claude-session-health", true},
		{"claude-session-", "claude-session-health", true}, // Linux 15-char comm
		{"claude-session", "claude-session-health", false},
		{"python3", "claude-session-health", false},
	} {
		if got := sameExecutable(tc.ps, tc.exe); got != tc.want {
			t.Errorf("sameExecutable(%q, %q) = %t", tc.ps, tc.exe, got)
		}
	}
}

func TestTryLockWaitOutlastsBriefProbe(t *testing.T) {
	dir := t.TempDir()
	probe, _, _ := TryLock(dir)
	go func() { time.Sleep(150 * time.Millisecond); probe.Release() }()
	l, ok, err := TryLockWait(dir, 10, 50*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("lock not acquired after the probe released it: %v %v", ok, err)
	}
	l.Release()
}

func TestPIDFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WritePID(dir, "1.2.3", "/x/bin"); err != nil {
		t.Fatal(err)
	}
	info, err := ReadPID(dir)
	if err != nil || info.PID != os.Getpid() || info.Version != "1.2.3" {
		t.Errorf("ReadPID = %+v, %v", info, err)
	}
	RemovePID(dir)
	if _, err := ReadPID(dir); err == nil {
		t.Error("pid file not removed")
	}
	if err := os.WriteFile(filepath.Join(dir, state.PIDFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPID(dir); err == nil {
		t.Error("garbage pid file accepted")
	}
}
