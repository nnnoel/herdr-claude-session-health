package history

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)

func sample(sec int, mb uint64) Sample {
	return Sample{T: t0.Add(time.Duration(sec) * time.Second), RSSKB: mb * 1024}
}

func TestBaselineAndPeak(t *testing.T) {
	s := NewStore(100, 100, 3)
	meta := Meta{PaneID: "w1:p1"}
	id := RunID{PID: 10, Start: t0}
	for i, mb := range []uint64{500, 700, 600, 900, 650} {
		if _, err := s.Observe("k", meta, id, sample(i*5, mb)); err != nil {
			t.Fatal(err)
		}
	}
	r := mustRun(t, s, "k")
	if !r.BaselineReady || r.BaselineKB != 600*1024 {
		t.Errorf("baseline = %d ready=%t, want median of first 3 (600 MB)", r.BaselineKB/1024, r.BaselineReady)
	}
	if r.PeakKB != 900*1024 {
		t.Errorf("peak = %d", r.PeakKB/1024)
	}
}

// 6. A restarted process, or a reused PID with a new start time, never
// inherits the old baseline, peak or history.
func TestRestartAndPIDReuseResetBaseline(t *testing.T) {
	tests := []struct {
		name string
		next RunID
	}{
		{"restart with new pid", RunID{PID: 11, Start: t0.Add(10 * time.Minute)}},
		{"reused pid, new start time", RunID{PID: 10, Start: t0.Add(10 * time.Minute)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStore(100, 100, 2)
			meta := Meta{PaneID: "w1:p1"}
			for i := 0; i < 5; i++ {
				if _, err := s.Observe("k", meta, RunID{PID: 10, Start: t0}, sample(i*5, 3000)); err != nil {
					t.Fatal(err)
				}
			}
			started, err := s.Observe("k", meta, tt.next, sample(700, 400))
			if err != nil || !started {
				t.Fatalf("new run not started: %v %v", started, err)
			}
			r := mustRun(t, s, "k")
			if r.PeakKB != 400*1024 || r.BaselineReady || r.Raw.Len() != 1 || len(r.Buckets()) != 0 {
				t.Errorf("new run inherited state: peak %d baseline-ready %t raw %d", r.PeakKB/1024, r.BaselineReady, r.Raw.Len())
			}
			sess, _ := s.Session("k")
			if sess.Restarts != 1 || len(sess.PastRuns) != 1 || sess.PastRuns[0].PeakKB != 3000*1024 {
				t.Errorf("past runs = %+v restarts %d", sess.PastRuns, sess.Restarts)
			}
		})
	}
}

func TestSameRunToleratesStartJitter(t *testing.T) {
	s := NewStore(10, 10, 1)
	_, _ = s.Observe("k", Meta{}, RunID{PID: 10, Start: t0}, sample(0, 1))
	started, _ := s.Observe("k", Meta{}, RunID{PID: 10, Start: t0.Add(time.Second)}, sample(5, 1))
	if started {
		t.Error("one second of etime jitter must not start a new run")
	}
}

func TestOutOfOrderSampleRejected(t *testing.T) {
	s := NewStore(10, 10, 1)
	id := RunID{PID: 1, Start: t0}
	_, _ = s.Observe("k", Meta{}, id, sample(10, 1))
	if _, err := s.Observe("k", Meta{}, id, sample(5, 1)); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("err = %v, want ErrOutOfOrder", err)
	}
}

// 10. History stays bounded regardless of how long a session runs.
func TestHistoryIsBounded(t *testing.T) {
	s := NewStore(50, 30, 1)
	id := RunID{PID: 1, Start: t0}
	for i := 0; i < 24*3600/5; i++ { // a full day at 5 s
		if _, err := s.Observe("k", Meta{}, id, sample(i*5, uint64(500+i%7))); err != nil {
			t.Fatal(err)
		}
	}
	r := mustRun(t, s, "k")
	if r.Raw.Len() != 50 || r.Minutes.Len() != 30 || len(r.pending) > 12 {
		t.Errorf("raw %d minutes %d pending %d", r.Raw.Len(), r.Minutes.Len(), len(r.pending))
	}
	for i := 0; i < 20; i++ {
		_, _ = s.Observe("k", Meta{}, RunID{PID: 100 + i, Start: t0.Add(time.Duration(i+1) * 48 * time.Hour)}, sample(200000+i*5, 1))
	}
	if sess, _ := s.Session("k"); len(sess.PastRuns) != maxPastRuns {
		t.Errorf("past runs = %d, want %d", len(sess.PastRuns), maxPastRuns)
	}
}

func TestBucketsUseMedian(t *testing.T) {
	s := NewStore(100, 100, 1)
	id := RunID{PID: 1, Start: t0}
	for i, mb := range []uint64{500, 500, 5000, 500, 500, 500} { // one spike sample
		_, _ = s.Observe("k", Meta{}, id, sample(i*5, mb))
	}
	_, _ = s.Observe("k", Meta{}, id, sample(61, 500)) // closes the first minute
	b := mustRun(t, s, "k").Buckets()
	if len(b) != 1 || b[0].MedKB != 500*1024 || b[0].MaxKB != 5000*1024 || b[0].N != 6 {
		t.Errorf("bucket = %+v", b)
	}
}

func TestPrune(t *testing.T) {
	s := NewStore(10, 10, 1)
	s.Touch("old", Meta{}, t0)
	s.Touch("new", Meta{}, t0.Add(23*time.Hour))
	removed := s.Prune(t0.Add(25*time.Hour), 24*time.Hour)
	if len(removed) != 1 || removed[0] != "old" || len(s.Keys()) != 1 {
		t.Errorf("removed %v keys %v", removed, s.Keys())
	}
}

func TestPruneCapsSessionCount(t *testing.T) {
	s := NewStore(10, 10, 1)
	for i := 0; i < MaxSessions+20; i++ {
		s.Touch(fmt.Sprintf("k%03d", i), Meta{}, t0.Add(time.Duration(i)*time.Minute))
	}
	removed := s.Prune(t0.Add(3*time.Hour), 24*time.Hour)
	if len(s.Keys()) != MaxSessions || len(removed) != 20 || removed[0] != "k000" {
		t.Errorf("kept %d, removed %v", len(s.Keys()), removed)
	}
}

func TestResetRunStartsFresh(t *testing.T) {
	s := NewStore(10, 10, 1)
	id := RunID{PID: 1, Start: t0}
	_, _ = s.Observe("k", Meta{}, id, sample(600, 1))
	s.ResetRun("k")
	if started, err := s.Observe("k", Meta{}, id, sample(10, 2)); err != nil || !started {
		t.Errorf("after reset an earlier timestamp must start a new run: %v %v", started, err)
	}
}

func TestPersistRoundTripAndRebound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s := NewStore(100, 100, 3)
	id := RunID{PID: 42, Start: t0}
	for i := 0; i < 30; i++ {
		_, _ = s.Observe("k", Meta{PaneID: "w1:p1", SessionID: "sid", Cwd: "/tmp"}, id, sample(i*5, uint64(600+i)))
	}
	if err := s.Save(path, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	smaller := NewStore(10, 1, 3)
	n, err := smaller.Load(path, t0.Add(time.Hour), 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("load: %d %v", n, err)
	}
	r := mustRun(t, smaller, "k")
	if r.Raw.Len() != 10 || r.Minutes.Len() != 1 || r.PeakKB != 629*1024 || !r.Matches(id) {
		t.Errorf("reloaded run: raw %d minutes %d peak %d", r.Raw.Len(), r.Minutes.Len(), r.PeakKB/1024)
	}
	if _, err := smaller.Observe("k", Meta{}, id, sample(30*5, 700)); err != nil {
		t.Errorf("continuing a reloaded run: %v", err)
	}
	expired := NewStore(10, 10, 3)
	if n, _ := expired.Load(path, t0.Add(48*time.Hour), 24*time.Hour); n != 0 {
		t.Errorf("sessions past retention loaded: %d", n)
	}
}

func TestLoadRejectsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"garbage.json": "{not json",
		"version.json": `{"version":99,"sessions":[]}`,
	}
	for name, body := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewStore(10, 10, 1).Load(path, t0, time.Hour); err == nil {
			t.Errorf("%s loaded without error", name)
		}
	}
	if n, err := NewStore(10, 10, 1).Load(filepath.Join(dir, "missing.json"), t0, time.Hour); n != 0 || err != nil {
		t.Errorf("missing file: %d %v", n, err)
	}
	unordered := `{"version":1,"sessions":[{"key":"k","last_seen":"2026-10-16T10:00:00Z","run":{"pid":5,"started_at":"2026-10-16T09:00:00Z",
	  "raw":[{"t":"2026-10-16T09:59:00Z","r":3},{"t":"2026-10-16T09:58:00Z","r":2},{"t":"2026-10-16T09:58:00Z","r":9}],"minutes":[]}}]}`
	path := filepath.Join(dir, "unordered.json")
	if err := os.WriteFile(path, []byte(unordered), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(10, 10, 1)
	if _, err := s.Load(path, t0, time.Hour); err != nil {
		t.Fatal(err)
	}
	raw := mustRun(t, s, "k").Samples()
	if len(raw) != 2 || !raw[0].T.Before(raw[1].T) {
		t.Errorf("samples not re-ordered/deduplicated: %+v", raw)
	}
}

func mustRun(t *testing.T, s *Store, key string) *Run {
	t.Helper()
	sess, ok := s.Session(key)
	if !ok || sess.Run == nil {
		t.Fatalf("no run for %s", key)
	}
	return sess.Run
}
