// Package attribution decides which processes belong to which Claude Code
// session. It verifies relationships in the process table before
// attributing memory, never counts a process twice, and labels anything it
// cannot verify as uncertain.
package attribution

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"claude-session-health/internal/collect"
)

// Confidence describes how well a root process was verified.
type Confidence string

// Confidence levels.
const (
	Verified  Confidence = "verified"  // claude is the pane shell's direct child in the foreground group
	Probable  Confidence = "probable"  // claude descends from the pane shell through a wrapper
	Uncertain Confidence = "uncertain" // relationship could not be verified
)

func (c Confidence) rank() int {
	switch c {
	case Verified:
		return 2
	case Probable:
		return 1
	default:
		return 0
	}
}

func weaker(a, b Confidence) Confidence {
	if a.rank() <= b.rank() {
		return a
	}
	return b
}

// ErrNoClaude means no Claude Code process could be found for the pane.
var ErrNoClaude = errors.New("no claude process found in pane")

// PaneProcesses is what Herdr reports about a pane, reduced to the facts
// attribution needs.
type PaneProcesses struct {
	ShellPID       int
	ForegroundPGID int
	ClaudePIDs     []int // foreground processes recognised as Claude Code
	GroupPIDs      []int // every foreground process Herdr listed
}

// Root is the verified top-level Claude Code process of a session.
type Root struct {
	PID        int
	Name       string
	Start      time.Time
	Confidence Confidence
	Notes      []string
}

// SelectRoot picks the outermost Claude Code process for a pane and grades
// how well its relationship to the pane is verified.
func SelectRoot(p PaneProcesses, t *collect.ProcTable) (Root, error) {
	var cands []int
	for _, pid := range p.ClaudePIDs {
		if _, ok := t.Procs[pid]; ok {
			cands = append(cands, pid)
		}
	}
	var notes []string
	conf := Verified
	if len(cands) == 0 && p.ShellPID > 0 {
		for pid, pr := range t.Procs {
			if pr.Name == "claude" && (pid == p.ShellPID || t.IsAncestor(p.ShellPID, pid)) {
				cands = append(cands, pid)
			}
		}
		if len(cands) > 0 {
			notes = append(notes, "found by executable name under the pane shell; Herdr listed no claude foreground process")
			conf = Probable
		}
	}
	if len(cands) == 0 {
		return Root{}, ErrNoClaude
	}
	sort.Ints(cands)

	var top []int
	for _, c := range cands {
		nested := false
		for _, o := range cands {
			if o != c && t.IsAncestor(o, c) {
				nested = true
				break
			}
		}
		if !nested {
			top = append(top, c)
		}
	}
	if n := len(cands) - len(top); n > 0 {
		notes = append(notes, fmt.Sprintf("%d nested claude process(es) counted as children of the session", n))
	}
	root := top[0]
	if len(top) > 1 {
		conf = Uncertain
		for _, c := range top {
			if t.Procs[c].PGID == p.ForegroundPGID {
				root = c
				break
			}
		}
		notes = append(notes, fmt.Sprintf("%d unrelated claude processes in the pane; chose PID %d", len(top), root))
	}

	pr := t.Procs[root]
	switch {
	case root == p.ShellPID:
		notes = append(notes, "claude is the pane's own process")
	case p.ShellPID > 0 && pr.PPID == p.ShellPID:
		notes = append(notes, "direct child of the pane shell")
	case p.ShellPID > 0 && t.IsAncestor(p.ShellPID, root):
		conf = weaker(conf, Probable)
		notes = append(notes, "descends from the pane shell through a wrapper process")
	default:
		conf = Uncertain
		notes = append(notes, "not a descendant of the pane shell")
	}
	if p.ForegroundPGID > 0 && pr.PGID != p.ForegroundPGID {
		conf = weaker(conf, Probable)
		notes = append(notes, "not in the pane's foreground process group")
	}
	start, _ := t.StartTime(root)
	return Root{PID: root, Name: pr.Name, Start: start, Confidence: conf, Notes: notes}, nil
}

// Child is a descendant process summary.
type Child struct {
	PID   int    `json:"pid"`
	Name  string `json:"name"`
	RSSKB uint64 `json:"rss_kb"`
}

// Usage is the measured process tree of one session.
type Usage struct {
	RootPID       int
	ClaudeRSSKB   uint64
	ChildrenRSSKB uint64
	ChildCount    int
	TopChildren   []Child
	CPUTimes      map[int]time.Duration
	StartTimes    map[int]time.Time
	PIDs          map[int]bool
	PSCPUPercent  float64 // sum of ps %cpu, used only for the first sample
	Notes         []string
	Missing       bool // root vanished from the table or was already attributed
}

// TotalRSSKB is Claude plus its descendants. Shared pages are counted once
// per process, so this over-estimates physical memory.
func (u Usage) TotalRSSKB() uint64 { return u.ClaudeRSSKB + u.ChildrenRSSKB }

// Measure walks each session's tree. A walk never descends into another
// session's root, and each PID is attributed to at most one session, so
// descendants are never double counted.
func Measure(roots map[string]int, t *collect.ProcTable) map[string]Usage {
	keys := make([]string, 0, len(roots))
	for k := range roots {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rootOwner := map[int]string{}
	for _, k := range keys {
		if _, taken := rootOwner[roots[k]]; !taken {
			rootOwner[roots[k]] = k
		}
	}

	claimed := map[int]string{}
	out := make(map[string]Usage, len(keys))
	for _, k := range keys {
		root := roots[k]
		u := Usage{RootPID: root, CPUTimes: map[int]time.Duration{}, StartTimes: map[int]time.Time{}, PIDs: map[int]bool{}}
		if owner := rootOwner[root]; owner != k {
			u.Notes = append(u.Notes, fmt.Sprintf("root PID %d is already attributed to session %s; not counted again", root, owner))
			u.Missing = true
			out[k] = u
			continue
		}
		rp, ok := t.Procs[root]
		if !ok {
			u.Missing = true
			out[k] = u
			continue
		}
		u.ClaudeRSSKB = rp.RSSKB
		u.PSCPUPercent = rp.CPUPercent
		u.CPUTimes[root] = rp.CPUTime
		u.StartTimes[root], _ = t.StartTime(root)
		u.PIDs[root] = true
		claimed[root] = k

		var children []Child
		queue := append([]int(nil), t.Children(root)...)
		for len(queue) > 0 {
			pid := queue[0]
			queue = queue[1:]
			if u.PIDs[pid] {
				continue
			}
			if owner, isRoot := rootOwner[pid]; isRoot && owner != k {
				u.Notes = append(u.Notes, fmt.Sprintf("PID %d is the root of session %s; excluded from this tree", pid, owner))
				continue
			}
			if owner, taken := claimed[pid]; taken && owner != k {
				continue
			}
			p, ok := t.Procs[pid]
			if !ok {
				continue
			}
			claimed[pid] = k
			u.PIDs[pid] = true
			u.ChildrenRSSKB += p.RSSKB
			u.PSCPUPercent += p.CPUPercent
			u.CPUTimes[pid] = p.CPUTime
			u.StartTimes[pid], _ = t.StartTime(pid)
			children = append(children, Child{PID: pid, Name: p.Name, RSSKB: p.RSSKB})
			queue = append(queue, t.Children(pid)...)
		}
		u.ChildCount = len(children)
		sort.Slice(children, func(i, j int) bool {
			if children[i].RSSKB != children[j].RSSKB {
				return children[i].RSSKB > children[j].RSSKB
			}
			return children[i].PID < children[j].PID
		})
		if len(children) > 5 {
			children = children[:5]
		}
		u.TopChildren = children
		out[k] = u
	}
	return out
}

// Orphans counts processes Herdr listed in the pane's foreground group that
// are not in the measured tree (typically helpers re-parented to launchd
// after their parent exited). They are reported separately and never added
// to the session's RSS, because ownership cannot be verified.
func Orphans(groupPIDs []int, u Usage, t *collect.ProcTable) (count int, rssKB uint64) {
	seen := map[int]bool{}
	for _, pid := range groupPIDs {
		if seen[pid] || u.PIDs[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := t.Procs[pid]; ok {
			count++
			rssKB += p.RSSKB
		}
	}
	return count, rssKB
}
