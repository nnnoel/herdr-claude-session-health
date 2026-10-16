package ui

import (
	"strconv"
	"time"

	"claude-session-health/internal/state"
)

// View is the active screen.
type View int

// Screens.
const (
	ViewList View = iota
	ViewDetail
	ViewConfirm
	ViewHelp
)

// Action is a side effect requested by a key press.
type Action int

// Actions.
const (
	ActNone Action = iota
	ActQuit
	ActFocus
	ActCopySession
	ActCopyAll
	ActCopyResume
	ActCopyID
	ActTerminate
)

// PendingTermination captures the session as displayed when the user
// pressed x, so confirmation always refers to what they saw.
type PendingTermination struct {
	Key    string
	Name   string // display name
	PaneID string
	Cwd    string
	RSSKB  uint64
	Resume string
	Target Target
}

// Model is the UI state.
type Model struct {
	Snap        *state.Snapshot
	SnapErr     string
	Now         time.Time
	Width       int
	Height      int
	Selected    int
	SelectedKey string
	View        View
	Scroll      int
	Confirm     string
	Pending     PendingTermination
	Flash       string
	FlashAt     time.Time
}

// SetSnapshot replaces the data, keeping the selection on the same session.
func (m *Model) SetSnapshot(snap *state.Snapshot, err error) {
	if err != nil {
		m.SnapErr = err.Error()
		if snap == nil && m.Snap != nil {
			// Keep showing the last snapshot; staleness marking makes clear it is old.
			return
		}
	} else {
		m.SnapErr = ""
	}
	m.Snap = snap
	if snap == nil {
		return
	}
	m.Selected = 0
	for i, x := range snap.Sessions {
		if x.Key == m.SelectedKey {
			m.Selected = i
		}
	}
	if m.Selected < len(snap.Sessions) && len(snap.Sessions) > 0 {
		m.SelectedKey = snap.Sessions[m.Selected].Key
	}
}

func (m *Model) selected() (state.Session, bool) {
	if m.Snap == nil || m.Selected < 0 || m.Selected >= len(m.Snap.Sessions) {
		return state.Session{}, false
	}
	return m.Snap.Sessions[m.Selected], true
}

// Selection returns the selected session, if any.
func (m *Model) Selection() (state.Session, bool) { return m.selected() }

// FlashMessage shows a transient status message.
func (m *Model) FlashMessage(msg string) { m.Flash, m.FlashAt = msg, m.Now }

func (m *Model) move(delta int) {
	if m.Snap == nil || len(m.Snap.Sessions) == 0 {
		return
	}
	m.Selected = min(max(0, m.Selected+delta), len(m.Snap.Sessions)-1)
	m.SelectedKey = m.Snap.Sessions[m.Selected].Key
}

// HandleKey updates the model for one key and returns any side effect.
func (m *Model) HandleKey(k string) Action {
	if k == "ctrl-c" {
		return ActQuit
	}
	switch m.View {
	case ViewConfirm:
		return m.confirmKey(k)
	case ViewHelp:
		if k == "esc" || k == "q" || k == "?" {
			m.View = ViewList
		}
		return ActNone
	case ViewDetail:
		switch k {
		case "esc", "h", "left", "q":
			m.View = ViewList
			return ActNone
		case "up", "k":
			m.Scroll = max(0, m.Scroll-1)
			return ActNone
		case "down", "j":
			m.Scroll++
			return ActNone
		}
	default:
		switch k {
		case "q", "esc":
			return ActQuit
		case "up", "k":
			m.move(-1)
			return ActNone
		case "down", "j":
			m.move(1)
			return ActNone
		case "enter", "l", "right":
			if _, ok := m.selected(); ok {
				m.View, m.Scroll = ViewDetail, 0
			}
			return ActNone
		case "?":
			m.View = ViewHelp
			return ActNone
		case "d":
			return ActCopyAll
		}
	}
	// Session actions, shared by the list and detail views.
	x, ok := m.selected()
	if !ok {
		return ActNone
	}
	switch k {
	case "f":
		return ActFocus
	case "c":
		return ActCopySession
	case "d":
		return ActCopyAll
	case "r":
		return ActCopyResume
	case "i":
		return ActCopyID
	case "x":
		if x.PID <= 1 || x.RunStartedAt.IsZero() {
			m.FlashMessage("No verified Claude process for this session; nothing to terminate.")
			return ActNone
		}
		m.Pending = PendingTermination{
			Key: x.Key, Name: x.ProcessName, PaneID: x.PaneID, Cwd: x.Cwd, RSSKB: x.RSSKB, Resume: x.Resume.Command,
			Target: Target{PID: x.PID, Start: x.RunStartedAt, Name: x.ProcessComm},
		}
		m.Confirm, m.View = "", ViewConfirm
	}
	return ActNone
}

func (m *Model) confirmKey(k string) Action {
	switch {
	case k == "esc":
		m.View, m.Confirm = ViewList, ""
		m.FlashMessage("Termination cancelled.")
	case k == "backspace":
		if n := len(m.Confirm); n > 0 {
			m.Confirm = m.Confirm[:n-1]
		}
	case k == "enter":
		want := strconv.Itoa(m.Pending.Target.PID)
		if m.Confirm != want {
			m.FlashMessage("Typed PID does not match " + want + "; nothing was terminated.")
			m.Confirm = ""
			return ActNone
		}
		m.View, m.Confirm = ViewList, ""
		return ActTerminate
	case len(k) == 1 && k[0] >= '0' && k[0] <= '9' && len(m.Confirm) < 10:
		m.Confirm += k
	}
	return ActNone
}

// ParseKeys splits raw terminal input into key names.
func ParseKeys(b []byte) []string {
	var keys []string
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			if i+2 < len(b) && (b[i+1] == '[' || b[i+1] == 'O') {
				switch b[i+2] {
				case 'A':
					keys = append(keys, "up")
				case 'B':
					keys = append(keys, "down")
				case 'C':
					keys = append(keys, "right")
				case 'D':
					keys = append(keys, "left")
				}
				j := i + 2
				for j < len(b) && !(b[j] >= 0x40 && b[j] <= 0x7e) {
					j++
				}
				i = j + 1
				continue
			}
			keys = append(keys, "esc")
		case c == '\r' || c == '\n':
			keys = append(keys, "enter")
		case c == 0x7f || c == 0x08:
			keys = append(keys, "backspace")
		case c == 0x03:
			keys = append(keys, "ctrl-c")
		case c >= 0x20 && c < 0x7f:
			keys = append(keys, string(rune(c)))
		}
		i++
	}
	return keys
}
