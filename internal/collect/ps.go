package collect

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PSArgs selects every process with the fields the monitor needs.
//
// Privacy: ucomm (the kernel's accounting name, at most 16 characters) is
// used instead of comm, because on macOS comm reflects a process's
// self-assigned title, and tools that retitle themselves can expose their
// arguments, including credentials. Only the first token of ucomm is kept
// (see ProcName). Full command lines are never requested.
var PSArgs = []string{"-axo", "pid=,ppid=,pgid=,rss=,%cpu=,time=,etime=,ucomm="}

// ProcName reduces a ps name field to a safe display name: its first token,
// base name only, without control characters, at most 32 runes.
func ProcName(field string) string {
	f := strings.Fields(field)
	if len(f) == 0 {
		return ""
	}
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, filepath.Base(f[0]))
	if r := []rune(name); len(r) > 32 {
		name = string(r[:32])
	}
	return name
}

// Proc is one row of the process table.
type Proc struct {
	PID        int
	PPID       int
	PGID       int
	RSSKB      uint64
	CPUPercent float64       // ps's decaying average, used only before a CPU-time delta exists
	CPUTime    time.Duration // cumulative user+system CPU time
	Elapsed    time.Duration // wall time since the process started
	Name       string        // first token of ucomm (see ProcName)
}

// ProcTable is one consistent snapshot of the process table.
type ProcTable struct {
	TakenAt  time.Time
	Procs    map[int]Proc
	Skipped  int // malformed or duplicate lines ignored
	children map[int][]int
}

// PS captures process tables by running ps.
type PS struct {
	Runner Runner
	Now    func() time.Time
}

// Snapshot runs ps once and parses the result.
func (p PS) Snapshot(ctx context.Context) (*ProcTable, error) {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	// etime is computed while ps runs, so stamp the table at the midpoint;
	// otherwise ps latency would shift every start-time estimate.
	before := now()
	out, err := p.Runner.Run(ctx, "ps", PSArgs...)
	if err != nil {
		return nil, fmt.Errorf("run ps: %w", err)
	}
	after := now()
	return ParsePS(out, before.Add(after.Sub(before)/2))
}

// ParsePS parses ps output in PSArgs format. Malformed lines are counted and
// skipped; output without a single valid row is an error.
func ParsePS(out []byte, takenAt time.Time) (*ProcTable, error) {
	t := &ProcTable{TakenAt: takenAt, Procs: map[int]Proc{}, children: map[int][]int{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		p, ok := parsePSLine(line)
		if !ok {
			t.Skipped++
			continue
		}
		if _, dup := t.Procs[p.PID]; dup {
			t.Skipped++
			continue
		}
		t.Procs[p.PID] = p
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan ps output: %w", err)
	}
	if len(t.Procs) == 0 {
		return nil, fmt.Errorf("ps output had no parseable processes (%d malformed lines)", t.Skipped)
	}
	for pid, p := range t.Procs {
		if p.PPID != pid {
			t.children[p.PPID] = append(t.children[p.PPID], pid)
		}
	}
	for k := range t.children {
		sort.Ints(t.children[k])
	}
	return t, nil
}

func parsePSLine(line string) (Proc, bool) {
	f := strings.Fields(line)
	if len(f) < 8 {
		return Proc{}, false
	}
	pid, err1 := strconv.Atoi(f[0])
	ppid, err2 := strconv.Atoi(f[1])
	pgid, err3 := strconv.Atoi(f[2])
	rss, err4 := strconv.ParseUint(f[3], 10, 64)
	cpu, err5 := strconv.ParseFloat(f[4], 64)
	cpuTime, err6 := ParseClock(f[5])
	elapsed, err7 := ParseClock(f[6])
	for _, err := range []error{err1, err2, err3, err4, err5, err6, err7} {
		if err != nil {
			return Proc{}, false
		}
	}
	if pid <= 0 || ppid < 0 || pgid < 0 || cpu < 0 || math.IsNaN(cpu) || math.IsInf(cpu, 0) {
		return Proc{}, false
	}
	return Proc{
		PID: pid, PPID: ppid, PGID: pgid, RSSKB: rss, CPUPercent: cpu,
		CPUTime: cpuTime, Elapsed: elapsed, Name: ProcName(f[7]),
	}, true
}

// ParseClock parses ps time formats: [DD-][HH:]MM:SS[.ff] and macOS's
// unbounded-minutes MMM:SS.ff.
func ParseClock(s string) (time.Duration, error) {
	var days int64
	rest := s
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		d, err := strconv.ParseInt(rest[:i], 10, 64)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("bad day field in %q", s)
		}
		days, rest = d, rest[i+1:]
	}
	parts := strings.Split(rest, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("bad clock %q", s)
	}
	secs, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil || secs < 0 || secs >= 60 || math.IsNaN(secs) {
		return 0, fmt.Errorf("bad seconds in %q", s)
	}
	total := secs
	mult := 60.0
	for i := len(parts) - 2; i >= 0; i-- {
		v, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("bad field in %q", s)
		}
		total += float64(v) * mult
		mult *= 60
	}
	total += float64(days) * 86400
	if total > float64(math.MaxInt64/int64(time.Second)) {
		return 0, fmt.Errorf("clock %q overflows", s)
	}
	return time.Duration(total * float64(time.Second)), nil
}

// Children returns the direct children of pid, sorted.
func (t *ProcTable) Children(pid int) []int { return t.children[pid] }

// StartTime estimates when pid started (one-second precision from etime).
func (t *ProcTable) StartTime(pid int) (time.Time, bool) {
	p, ok := t.Procs[pid]
	if !ok {
		return time.Time{}, false
	}
	return t.TakenAt.Add(-p.Elapsed).Truncate(time.Second), true
}

// IsAncestor reports whether anc is a strict ancestor of pid. It tolerates
// cycles and missing parents in malformed tables.
func (t *ProcTable) IsAncestor(anc, pid int) bool {
	seen := map[int]bool{}
	for cur := pid; ; {
		p, ok := t.Procs[cur]
		if !ok || seen[cur] || p.PPID == cur {
			return false
		}
		seen[cur] = true
		if p.PPID == anc {
			return true
		}
		cur = p.PPID
	}
}

// SameStart reports whether two start-time estimates describe the same
// process. etime has one-second precision and ps latency adds jitter; a
// reused PID within a few seconds of the original's start is not a
// realistic case, so a few seconds of tolerance is safe.
func SameStart(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= 5*time.Second
}
