package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 10. Logs stay bounded on disk and in memory.
func TestLogRotationIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.log")
	l, err := New(path, 8192, 20)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		l.Logf(Sampling, "line %d %s", i, strings.Repeat("x", 80))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + ".1"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Size() > 8192 {
			t.Errorf("%s is %d bytes, over the 8192 limit", p, info.Size())
		}
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Error("only one backup file may exist")
	}
	if got := len(l.Recent()); got != 20 {
		t.Errorf("recent = %d, want 20", got)
	}
	if last := l.Recent()[19]; !strings.Contains(last, "line 4999") {
		t.Errorf("newest line = %q", last)
	}
}

func TestLogLinesAreSingleLineAndTruncated(t *testing.T) {
	l, _ := New("", 4096, 5)
	l.Logf(State, "a\nb\rc %s", strings.Repeat("y", 2000))
	got := l.Recent()[0]
	if strings.ContainsAny(got, "\n\r") || len(got) > maxLine+100 || !strings.Contains(got, "[state] a b c") {
		t.Errorf("line = %q (len %d)", got[:min(80, len(got))], len(got))
	}
	var nilLogger *Logger
	nilLogger.Logf(State, "ignored")
	if nilLogger.Recent() != nil || nilLogger.Close() != nil {
		t.Error("nil logger must be a no-op")
	}
}
