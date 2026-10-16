package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"claude-session-health/internal/fsutil"
	"claude-session-health/internal/ring"
)

const persistVersion = 1

type persistedRun struct {
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	FirstSeen     time.Time `json:"first_seen"`
	LastSample    time.Time `json:"last_sample"`
	PeakKB        uint64    `json:"peak_kb"`
	PeakAt        time.Time `json:"peak_at"`
	BaselineKB    uint64    `json:"baseline_kb"`
	BaselineReady bool      `json:"baseline_ready"`
	BaselineBuf   []uint64  `json:"baseline_buf,omitempty"`
	Raw           []Sample  `json:"raw"`
	Minutes       []Bucket  `json:"minutes"`
	Pending       []Sample  `json:"pending,omitempty"`
}

type persistedSession struct {
	Key       string        `json:"key"`
	PaneID    string        `json:"pane_id"`
	SessionID string        `json:"session_id,omitempty"`
	Cwd       string        `json:"cwd,omitempty"`
	FirstSeen time.Time     `json:"first_seen"`
	LastSeen  time.Time     `json:"last_seen"`
	Restarts  int           `json:"restarts"`
	PastRuns  []RunSummary  `json:"past_runs,omitempty"`
	Run       *persistedRun `json:"run,omitempty"`
}

type persistedFile struct {
	Version  int                `json:"version"`
	SavedAt  time.Time          `json:"saved_at"`
	Sessions []persistedSession `json:"sessions"`
}

// Save writes the store atomically to path.
func (s *Store) Save(path string, now time.Time) error {
	f := persistedFile{Version: persistVersion, SavedAt: now.UTC()}
	for _, k := range s.Keys() {
		sess := s.sessions[k]
		ps := persistedSession{
			Key: sess.Key, PaneID: sess.Meta.PaneID, SessionID: sess.Meta.SessionID, Cwd: sess.Meta.Cwd,
			FirstSeen: sess.FirstSeen, LastSeen: sess.LastSeen, Restarts: sess.Restarts, PastRuns: sess.PastRuns,
		}
		if r := sess.Run; r != nil {
			ps.Run = &persistedRun{
				PID: r.PID, StartedAt: r.StartedAt, FirstSeen: r.FirstSeen, LastSample: r.LastSample,
				PeakKB: r.PeakKB, PeakAt: r.PeakAt, BaselineKB: r.BaselineKB, BaselineReady: r.BaselineReady,
				BaselineBuf: r.baselineBuf, Raw: r.Raw.Slice(), Minutes: r.Minutes.Slice(), Pending: r.pending,
			}
		}
		f.Sessions = append(f.Sessions, ps)
	}
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode history: %w", err)
	}
	if err := fsutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("save history: %w", err)
	}
	return nil
}

// Load replaces the store's contents from path. A missing file is not an
// error. Entries are re-bounded to the current capacities, out-of-order
// samples are dropped, and sessions older than retention are discarded.
func (s *Store) Load(path string, now time.Time, retention time.Duration) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read history: %w", err)
	}
	var f persistedFile
	if err := json.Unmarshal(data, &f); err != nil {
		return 0, fmt.Errorf("decode history: %w", err)
	}
	if f.Version != persistVersion {
		return 0, fmt.Errorf("history version %d is not supported (want %d)", f.Version, persistVersion)
	}
	loaded := 0
	for _, ps := range f.Sessions {
		if ps.Key == "" || now.Sub(ps.LastSeen) > retention {
			continue
		}
		sess := &Session{
			Key: ps.Key, Meta: Meta{PaneID: ps.PaneID, SessionID: ps.SessionID, Cwd: ps.Cwd},
			FirstSeen: ps.FirstSeen, LastSeen: ps.LastSeen, Restarts: ps.Restarts, PastRuns: ps.PastRuns,
		}
		if len(sess.PastRuns) > maxPastRuns {
			sess.PastRuns = sess.PastRuns[len(sess.PastRuns)-maxPastRuns:]
		}
		if pr := ps.Run; pr != nil && pr.PID > 0 {
			r := &Run{
				PID: pr.PID, StartedAt: pr.StartedAt, FirstSeen: pr.FirstSeen, LastSample: pr.LastSample,
				PeakKB: pr.PeakKB, PeakAt: pr.PeakAt, BaselineKB: pr.BaselineKB, BaselineReady: pr.BaselineReady,
				Raw:     ring.FromSlice(s.rawCap, ordered(pr.Raw, func(x Sample) time.Time { return x.T })),
				Minutes: ring.FromSlice(s.minuteCap, ordered(pr.Minutes, func(x Bucket) time.Time { return x.T })),
				pending: ordered(pr.Pending, func(x Sample) time.Time { return x.T }), baselineN: s.baselineN,
			}
			if !r.BaselineReady {
				r.baselineBuf = pr.BaselineBuf
			}
			if last, ok := r.Raw.Last(); ok && last.T.After(r.LastSample) {
				r.LastSample = last.T
			}
			sess.Run = r
		}
		s.sessions[sess.Key] = sess
		loaded++
	}
	return loaded, nil
}

// ordered keeps only strictly increasing timestamps so corrupted files
// cannot break the time-ordering invariant.
func ordered[T any](items []T, at func(T) time.Time) []T {
	sorted := sort.SliceIsSorted(items, func(i, j int) bool { return at(items[i]).Before(at(items[j])) })
	if !sorted {
		items = append([]T(nil), items...)
		sort.SliceStable(items, func(i, j int) bool { return at(items[i]).Before(at(items[j])) })
	}
	out := make([]T, 0, len(items))
	var last time.Time
	for _, it := range items {
		if t := at(it); !t.IsZero() && t.After(last) {
			out = append(out, it)
			last = t
		}
	}
	return out
}
