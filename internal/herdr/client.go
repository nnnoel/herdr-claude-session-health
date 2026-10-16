// Package herdr is a minimal client for the documented Herdr socket API
// (newline-delimited JSON, one request per connection).
//
// Privacy: responses are decoded into narrow structs. Process command lines
// and terminal titles are never decoded, stored or logged; argv is inspected
// transiently to recognise Claude Code and then discarded.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Source identifies this plugin's metadata reports.
const Source = "plugin:claude-session-health"

const maxResponse = 16 << 20

// ErrUnavailable wraps failures to reach the Herdr socket at all.
var ErrUnavailable = errors.New("herdr socket unavailable")

// Client talks to one Herdr server socket.
type Client struct {
	SocketPath string
	Timeout    time.Duration
	seq        atomic.Uint64
}

// NewClient returns a client with a default per-request timeout.
func NewClient(socketPath string) *Client {
	return &Client{SocketPath: socketPath, Timeout: 3 * time.Second}
}

type rpcError struct {
	Code    any    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// Call sends one request and returns the raw result.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.SocketPath == "" {
		return nil, fmt.Errorf("%w: HERDR_SOCKET_PATH is not set", ErrUnavailable)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if params == nil {
		params = struct{}{}
	}
	id := fmt.Sprintf("csh:%d", c.seq.Add(1))
	req, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("write %s request: %w", method, err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxResponse)).ReadBytes('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return nil, fmt.Errorf("read %s response: %w", method, err)
	}
	return DecodeResponse(line, id)
}

// DecodeResponse validates one response line.
func DecodeResponse(line []byte, wantID string) (json.RawMessage, error) {
	var r response
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, fmt.Errorf("invalid herdr response: %w", err)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("herdr error %v: %s", r.Error.Code, r.Error.Message)
	}
	if wantID != "" && r.ID != wantID {
		return nil, fmt.Errorf("herdr response id %q does not match request %q", r.ID, wantID)
	}
	if len(r.Result) == 0 || string(r.Result) == "null" {
		return nil, errors.New("herdr response has no result")
	}
	return r.Result, nil
}

// Agent is one pane Herdr recognises as hosting an agent.
type Agent struct {
	PaneID        string
	TerminalID    string
	WorkspaceID   string
	TabID         string
	Agent         string // e.g. "claude"; empty when the pane has no detected agent
	Status        string // working, blocked, idle, done, unknown
	Cwd           string
	SessionID     string // Claude session id reported by Herdr's Claude integration
	SessionSource string
	Focused       bool
}

// Agents calls agent.list.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	raw, err := c.Call(ctx, "agent.list", nil)
	if err != nil {
		return nil, err
	}
	return ParseAgents(raw)
}

// ParseAgents decodes an agent.list result.
func ParseAgents(raw json.RawMessage) ([]Agent, error) {
	var payload struct {
		Agents *[]struct {
			PaneID        string  `json:"pane_id"`
			TerminalID    string  `json:"terminal_id"`
			WorkspaceID   string  `json:"workspace_id"`
			TabID         string  `json:"tab_id"`
			Agent         *string `json:"agent"`
			AgentStatus   string  `json:"agent_status"`
			Cwd           string  `json:"cwd"`
			ForegroundCwd string  `json:"foreground_cwd"`
			Focused       bool    `json:"focused"`
			AgentSession  *struct {
				Kind   string `json:"kind"`
				Source string `json:"source"`
				Value  string `json:"value"`
			} `json:"agent_session"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode agent list: %w", err)
	}
	if payload.Agents == nil {
		return nil, errors.New("agent list has no agents field")
	}
	agents := make([]Agent, 0, len(*payload.Agents))
	for _, a := range *payload.Agents {
		if a.PaneID == "" {
			continue
		}
		out := Agent{
			PaneID: a.PaneID, TerminalID: a.TerminalID, WorkspaceID: a.WorkspaceID, TabID: a.TabID,
			Status: a.AgentStatus, Cwd: a.ForegroundCwd, Focused: a.Focused,
		}
		if out.Cwd == "" {
			out.Cwd = a.Cwd
		}
		if a.Agent != nil {
			out.Agent = *a.Agent
		}
		if s := a.AgentSession; s != nil && s.Kind == "id" {
			out.SessionID, out.SessionSource = s.Value, s.Source
		}
		agents = append(agents, out)
	}
	return agents, nil
}

// ForegroundProc is a process in a pane's foreground process group.
type ForegroundProc struct {
	PID             int
	Name            string // executable base name only
	LooksLikeClaude bool
}

// ProcessInfo is the pane's shell and foreground processes.
type ProcessInfo struct {
	PaneID         string
	ShellPID       int
	ForegroundPGID int
	Procs          []ForegroundProc
}

// ProcessInfo calls pane.process_info.
func (c *Client) ProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error) {
	raw, err := c.Call(ctx, "pane.process_info", map[string]any{"pane_id": paneID})
	if err != nil {
		return ProcessInfo{}, err
	}
	return ParseProcessInfo(raw)
}

// ParseProcessInfo decodes a pane.process_info result.
func ParseProcessInfo(raw json.RawMessage) (ProcessInfo, error) {
	var payload struct {
		ProcessInfo *struct {
			PaneID         string `json:"pane_id"`
			ShellPID       int    `json:"shell_pid"`
			ForegroundPGID int    `json:"foreground_process_group_id"`
			Procs          *[]struct {
				PID   int      `json:"pid"`
				Name  string   `json:"name"`
				Argv0 string   `json:"argv0"`
				Argv  []string `json:"argv"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ProcessInfo{}, fmt.Errorf("decode process info: %w", err)
	}
	pi := payload.ProcessInfo
	if pi == nil || pi.Procs == nil {
		return ProcessInfo{}, errors.New("process info has no foreground_processes")
	}
	info := ProcessInfo{PaneID: pi.PaneID, ShellPID: pi.ShellPID, ForegroundPGID: pi.ForegroundPGID}
	for _, p := range *pi.Procs {
		if p.PID <= 0 {
			continue
		}
		name, claude := Classify(p.Argv0, p.Argv, p.Name)
		info.Procs = append(info.Procs, ForegroundProc{PID: p.PID, Name: name, LooksLikeClaude: claude})
	}
	return info, nil
}

// Classify derives a display name and whether the process is Claude Code.
// Claude retitles itself to its version number, so argv is the reliable
// signal. Only the first token's base name is ever returned: argv0 can be a
// retitled string containing arguments, including secrets.
func Classify(argv0 string, argv []string, name string) (string, bool) {
	first := firstToken(argv0)
	if first == "" && len(argv) > 0 {
		first = argv[0]
	}
	base := filepath.Base(first)
	claude := base == "claude"
	if !claude && len(argv) > 0 && filepath.Base(argv[0]) == "claude" {
		claude = true
	}
	if !claude && len(argv) > 1 && (base == "node" || base == "bun") &&
		strings.Contains(argv[1], "@anthropic-ai/claude-code") {
		claude = true
	}
	display := base
	if display == "" || display == "." || display == "/" {
		display = filepath.Base(firstToken(name))
	}
	if r := []rune(display); len(r) > 32 {
		display = string(r[:32])
	}
	return display, claude
}

func firstToken(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// ReportTokens sets (or, with nil values, clears) display tokens on a pane.
func (c *Client) ReportTokens(ctx context.Context, paneID string, tokens map[string]*string, ttl time.Duration) error {
	params := map[string]any{"pane_id": paneID, "source": Source, "tokens": tokens}
	if ttl > 0 {
		params["ttl_ms"] = ttl.Milliseconds()
	}
	_, err := c.Call(ctx, "pane.report_metadata", params)
	return err
}

// Notify shows a Herdr notification. It reports whether Herdr displayed it
// and, if not, Herdr's reason (e.g. disabled, rate_limited).
func (c *Client) Notify(ctx context.Context, title, body string) (bool, string, error) {
	raw, err := c.Call(ctx, "notification.show", map[string]any{"title": title, "body": body, "sound": "none"})
	if err != nil {
		return false, "", err
	}
	var r struct {
		Shown  *bool  `json:"shown"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return false, "", fmt.Errorf("decode notification result: %w", err)
	}
	if r.Shown == nil {
		return true, "", nil
	}
	return *r.Shown, r.Reason, nil
}

// FocusAgent focuses the agent pane identified by target (a pane id).
func (c *Client) FocusAgent(ctx context.Context, target string) error {
	_, err := c.Call(ctx, "agent.focus", map[string]any{"target": target})
	return err
}

// OpenPluginPane opens one of this plugin's pane entrypoints.
func (c *Client) OpenPluginPane(ctx context.Context, pluginID, entrypoint string) error {
	_, err := c.Call(ctx, "plugin.pane.open", map[string]any{
		"plugin_id": pluginID, "entrypoint": entrypoint, "focus": true,
	})
	return err
}

// Ping returns the server version.
func (c *Client) Ping(ctx context.Context) (string, error) {
	raw, err := c.Call(ctx, "ping", nil)
	if err != nil {
		return "", err
	}
	var r struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("decode ping: %w", err)
	}
	return r.Version, nil
}

// PluginEnabled reports whether pluginID is installed and enabled.
func (c *Client) PluginEnabled(ctx context.Context, pluginID string) (bool, error) {
	raw, err := c.Call(ctx, "plugin.list", map[string]any{"plugin_id": pluginID})
	if err != nil {
		return false, err
	}
	return ParsePluginEnabled(raw, pluginID)
}

// ParsePluginEnabled decodes a plugin.list result.
func ParsePluginEnabled(raw json.RawMessage, pluginID string) (bool, error) {
	var r struct {
		Plugins *[]struct {
			PluginID string `json:"plugin_id"`
			Enabled  bool   `json:"enabled"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return false, fmt.Errorf("decode plugin list: %w", err)
	}
	if r.Plugins == nil {
		return false, errors.New("plugin list has no plugins field")
	}
	for _, p := range *r.Plugins {
		if p.PluginID == pluginID && p.Enabled {
			return true, nil
		}
	}
	return false, nil
}
