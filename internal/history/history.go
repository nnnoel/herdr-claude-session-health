// Package history keeps bounded per-session memory history in two tiers:
// raw samples at the sampling interval (short horizon) and one-minute
// buckets (hours). Each Claude process run has its own history, so a
// restarted or reused PID never inherits an old baseline.
package history

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"claude-session-health/internal/collect"
	"claude-session-health/internal/ring"
)

// Sample is one successful measurement. Missing samples are simply absent:
// a gap is never recorded as zero.
type Sample struct {
	T        time.Time `json:"t"`
	RSSKB    uint64    `json:"r"`
	ClaudeKB uint64    `json:"c"`
	CPU      float64   `json:"u"`
	Idle     bool      `json:"i,omitempty"`
}

// Bucket summarises the samples within one wall-clock minute.
type Bucket struct {
	T     time.Time `json:"t"` // start of the minute
	MedKB uint64    `json:"m"` // median RSS, robust to single spikes
	MinKB uint64    `json:"lo"`
	MaxKB uint64    `json:"hi"`
	N     int       `json:"n"`
	Idle  int       `json:"i"`
	CPU   float64   `json:"u"`
}

// RunID identifies one Claude process lifetime.
type RunID struct {
	PID   int
	Start time.Time
}

// Run is the history of one process lifetime.
type Run struct {
	PID           int
	StartedAt     time.Time
	FirstSeen     time.Time
	LastSample    time.Time
	PeakKB        uint64
	PeakAt        time.Time
	BaselineKB    uint64
	BaselineReady bool
	Raw           *ring.Ring[Sample]
	Minutes       *ring.Ring[Bucket]
	pending       []Sample
	baselineBuf   []uint64
	baselineN     int
}

// ErrOutOfOrder rejects samples that do not advance time.
var ErrOutOfOrder = errors.New("sample is not newer than the previous sample")

func newRun(id RunID, firstSeen time.Time, rawCap, minuteCap, baselineN int) *Run {
	return &Run{
		PID: id.PID, StartedAt: id.Start, FirstSeen: firstSeen,
		Raw: ring.New[Sample](rawCap), Minutes: ring.New[Bucket](minuteCap),
		baselineN: baselineN,
	}
}

// Matches reports whether id is this run (same PID and start time).
func (r *Run) Matches(id RunID) bool {
	return r.PID == id.PID && collect.SameStart(r.StartedAt, id.Start)
}

// Add appends a sample, rolling completed minutes into buckets.
func (r *Run) Add(s Sample) error {
	if !r.LastSample.IsZero() && !s.T.After(r.LastSample) {
		return ErrOutOfOrder
	}
	if len(r.pending) > 0 && !s.T.Truncate(time.Minute).Equal(r.pending[0].T.Truncate(time.Minute)) {
		r.closeBucket()
	}
	r.pending = append(r.pending, s)
	r.Raw.Push(s)
	r.LastSample = s.T
	if s.RSSKB > r.PeakKB {
		r.PeakKB, r.PeakAt = s.RSSKB, s.T
	}
	if !r.BaselineReady {
		r.baselineBuf = append(r.baselineBuf, s.RSSKB)
		if len(r.baselineBuf) >= r.baselineN {
			r.BaselineKB = median(r.baselineBuf)
			r.BaselineReady = true
			r.baselineBuf = nil
		}
	}
	return nil
}

func (r *Run) closeBucket() {
	if len(r.pending) == 0 {
		return
	}
	vals := make([]uint64, len(r.pending))
	b := Bucket{T: r.pending[0].T.Truncate(time.Minute), N: len(r.pending), MinKB: r.pending[0].RSSKB}
	var cpu float64
	for i, s := range r.pending {
		vals[i] = s.RSSKB
		if s.RSSKB < b.MinKB {
			b.MinKB = s.RSSKB
		}
		if s.RSSKB > b.MaxKB {
			b.MaxKB = s.RSSKB
		}
		if s.Idle {
			b.Idle++
		}
		cpu += s.CPU
	}
	b.MedKB = median(vals)
	b.CPU = cpu / float64(len(r.pending))
	r.Minutes.Push(b)
	r.pending = r.pending[:0]
}

// Buckets returns completed minute buckets, oldest first.
func (r *Run) Buckets() []Bucket { return r.Minutes.Slice() }

// Samples returns raw samples, oldest first.
func (r *Run) Samples() []Sample { return r.Raw.Slice() }

// median returns the lower median, which is robust to a single spike.
func median(v []uint64) uint64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]uint64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)-1)/2]
}

// RunSummary records a finished run.
type RunSummary struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	PeakKB    uint64    `json:"peak_kb"`
	LastKB    uint64    `json:"last_kb"`
}

// Meta is descriptive session data refreshed every tick.
type Meta struct {
	PaneID    string
	SessionID string
	Cwd       string
}

// Session is the history for one stable session identity (a Herdr terminal).
type Session struct {
	Key       string
	Meta      Meta
	FirstSeen time.Time
	LastSeen  time.Time
	Restarts  int
	PastRuns  []RunSummary
	Run       *Run
}

const maxPastRuns = 5

// Store holds all sessions with bounded memory.
type Store struct {
	rawCap, minuteCap, baselineN int
	sessions                     map[string]*Session
}

// NewStore sizes each run's raw and minute rings. baselineN samples form the baseline.
func NewStore(rawCap, minuteCap, baselineN int) *Store {
	if baselineN < 1 {
		baselineN = 1
	}
	return &Store{rawCap: rawCap, minuteCap: minuteCap, baselineN: baselineN, sessions: map[string]*Session{}}
}

// Session returns the session for key, if any.
func (s *Store) Session(key string) (*Session, bool) {
	sess, ok := s.sessions[key]
	return sess, ok
}

// Keys returns every stored session key, sorted.
func (s *Store) Keys() []string {
	keys := make([]string, 0, len(s.sessions))
	for k := range s.sessions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Touch records that a session exists without adding a sample.
func (s *Store) Touch(key string, meta Meta, now time.Time) *Session {
	sess, ok := s.sessions[key]
	if !ok {
		sess = &Session{Key: key, FirstSeen: now}
		s.sessions[key] = sess
	}
	sess.Meta = meta
	sess.LastSeen = now
	return sess
}

// Observe adds a sample for run id, starting a fresh run (and baseline)
// when the PID or its start time changed. It reports whether a new run began.
func (s *Store) Observe(key string, meta Meta, id RunID, sample Sample) (bool, error) {
	sess := s.Touch(key, meta, sample.T)
	started := false
	if sess.Run == nil || !sess.Run.Matches(id) {
		if sess.Run != nil {
			sess.archive()
			sess.Restarts++
		}
		sess.Run = newRun(id, sample.T, s.rawCap, s.minuteCap, s.baselineN)
		started = true
	}
	if err := sess.Run.Add(sample); err != nil {
		return started, fmt.Errorf("session %s: %w", key, err)
	}
	return started, nil
}

func (sess *Session) archive() {
	old := sess.Run
	last, _ := old.Raw.Last()
	sess.PastRuns = append(sess.PastRuns, RunSummary{
		PID: old.PID, StartedAt: old.StartedAt, EndedAt: old.LastSample, PeakKB: old.PeakKB, LastKB: last.RSSKB,
	})
	if len(sess.PastRuns) > maxPastRuns {
		sess.PastRuns = sess.PastRuns[len(sess.PastRuns)-maxPastRuns:]
	}
	sess.Run = nil
}

// ResetRun archives a session's current run so its next sample starts a
// fresh history and baseline. It is used after a backward clock step, which
// would otherwise make every new sample look out of order.
func (s *Store) ResetRun(key string) {
	if sess, ok := s.sessions[key]; ok && sess.Run != nil {
		sess.archive()
	}
}

// MaxSessions bounds how many sessions (live or ended) the store keeps.
const MaxSessions = 50

// Prune deletes sessions not seen within retention, then the least recently
// seen sessions beyond MaxSessions, and returns their keys.
func (s *Store) Prune(now time.Time, retention time.Duration) []string {
	var removed []string
	for k, sess := range s.sessions {
		if now.Sub(sess.LastSeen) > retention {
			delete(s.sessions, k)
			removed = append(removed, k)
		}
	}
	if extra := len(s.sessions) - MaxSessions; extra > 0 {
		keys := s.Keys()
		sort.SliceStable(keys, func(i, j int) bool { return s.sessions[keys[i]].LastSeen.Before(s.sessions[keys[j]].LastSeen) })
		for _, k := range keys[:extra] {
			delete(s.sessions, k)
			removed = append(removed, k)
		}
	}
	sort.Strings(removed)
	return removed
}
