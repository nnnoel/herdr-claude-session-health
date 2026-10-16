package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAgents(t *testing.T) {
	raw := json.RawMessage(`{"agents":[
	  {"agent":"claude","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"2aaebfe0-71a7-4dd1-a5f7-727c1573ec6a"},
	   "agent_status":"idle","cwd":"/tmp/a","foreground_cwd":"/tmp/a/sub","focused":false,"pane_id":"w1:p6","tab_id":"w1:t1",
	   "terminal_id":"term_1","terminal_title":"secret conversation title","workspace_id":"w1"},
	  {"agent":null,"agent_status":"unknown","cwd":"/tmp/b","pane_id":"w1:p7","terminal_id":"term_2"},
	  {"pane_id":""}
	],"type":"agent_list"}`)
	agents, err := ParseAgents(raw)
	if err != nil {
		t.Fatalf("ParseAgents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}
	a := agents[0]
	if a.Agent != "claude" || a.SessionID != "2aaebfe0-71a7-4dd1-a5f7-727c1573ec6a" || a.Cwd != "/tmp/a/sub" || a.TerminalID != "term_1" {
		t.Errorf("agent 0 = %+v", a)
	}
	if agents[1].Agent != "" || agents[1].Cwd != "/tmp/b" {
		t.Errorf("agent 1 = %+v", agents[1])
	}
	if data, _ := json.Marshal(agents); strings.Contains(string(data), "secret conversation title") {
		t.Error("terminal titles must not be retained")
	}
	for _, bad := range []string{`{}`, `{"agents":"x"}`, `not json`} {
		if _, err := ParseAgents(json.RawMessage(bad)); err == nil {
			t.Errorf("ParseAgents(%s) succeeded", bad)
		}
	}
}

func TestParseProcessInfoNeverRetainsCommandLines(t *testing.T) {
	raw := json.RawMessage(`{"process_info":{"foreground_process_group_id":70548,"pane_id":"w2:p1","shell_pid":70376,
	  "foreground_processes":[
	    {"argv":["node","/x/mcp-remote","--header","Authorization:Bearer phx_SECRET"],"argv0":"node","cmdline":"node /x/mcp-remote --header Authorization:Bearer phx_SECRET","name":"node","pid":70627},
	    {"argv0":"mcp --header Authorization:Bearer phx_SECRET","name":"node","pid":70591},
	    {"argv":["claude","--dangerously-skip-permissions"],"argv0":"claude","cmdline":"claude --dangerously-skip-permissions","name":"2.1.301","pid":70548},
	    {"pid":0}
	  ]}}`)
	info, err := ParseProcessInfo(raw)
	if err != nil {
		t.Fatalf("ParseProcessInfo: %v", err)
	}
	if info.ShellPID != 70376 || info.ForegroundPGID != 70548 || len(info.Procs) != 3 {
		t.Fatalf("info = %+v", info)
	}
	if !info.Procs[2].LooksLikeClaude || info.Procs[0].LooksLikeClaude || info.Procs[1].LooksLikeClaude {
		t.Errorf("claude classification wrong: %+v", info.Procs)
	}
	data, _ := json.Marshal(info)
	for _, secret := range []string{"SECRET", "Bearer", "--header", "dangerously"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("parsed process info retains %q: %s", secret, data)
		}
	}
	if _, err := ParseProcessInfo(json.RawMessage(`{"process_info":{}}`)); err == nil {
		t.Error("missing foreground_processes should fail")
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		argv0  string
		argv   []string
		name   string
		want   string
		claude bool
	}{
		{argv0: "claude", argv: []string{"claude"}, name: "2.1.301", want: "claude", claude: true},
		{argv0: "/Users/x/.local/bin/claude --resume abc", name: "2.1.301", want: "claude", claude: true},
		{argv0: "node", argv: []string{"node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, want: "node", claude: true},
		{argv0: "node", argv: []string{"node", "/x/playwright-mcp"}, want: "node"},
		{argv0: "mcp --header Authorization:Bearer xyz", name: "node", want: "mcp"},
		{argv0: "", argv: nil, name: "python3.10", want: "python3.10"},
		{argv0: "claude-wrapper", want: "claude-wrapper"},
	}
	for _, tt := range tests {
		got, claude := Classify(tt.argv0, tt.argv, tt.name)
		if got != tt.want || claude != tt.claude {
			t.Errorf("Classify(%q, %v, %q) = %q, %t; want %q, %t", tt.argv0, tt.argv, tt.name, got, claude, tt.want, tt.claude)
		}
	}
}

func TestDecodeResponse(t *testing.T) {
	if _, err := DecodeResponse([]byte(`{"id":"a","result":{"type":"pong"}}`), "a"); err != nil {
		t.Errorf("valid response: %v", err)
	}
	for _, bad := range []string{
		`{"id":"a","error":{"code":"not_found","message":"no pane"}}`,
		`{"id":"b","result":{}}`,
		`{"id":"a"}`,
		`{"id":"a","result":null}`,
		`not json`,
		``,
	} {
		if _, err := DecodeResponse([]byte(bad), "a"); err == nil {
			t.Errorf("DecodeResponse(%q) succeeded", bad)
		}
	}
}

func TestParsePluginEnabled(t *testing.T) {
	raw := json.RawMessage(`{"plugins":[{"plugin_id":"claude-session-health","enabled":true},{"plugin_id":"other","enabled":false}]}`)
	if ok, err := ParsePluginEnabled(raw, "claude-session-health"); !ok || err != nil {
		t.Errorf("enabled = %t %v", ok, err)
	}
	if ok, _ := ParsePluginEnabled(raw, "other"); ok {
		t.Error("disabled plugin reported enabled")
	}
	if ok, _ := ParsePluginEnabled(json.RawMessage(`{"plugins":[]}`), "x"); ok {
		t.Error("missing plugin reported enabled")
	}
	if _, err := ParsePluginEnabled(json.RawMessage(`{}`), "x"); err == nil {
		t.Error("missing plugins field should fail")
	}
}

// fakeServer answers one request per connection, like Herdr.
func fakeServer(t *testing.T, handle func(req map[string]any) string) string {
	t.Helper()
	// Unix socket paths are length-limited, so avoid the long default temp dir.
	dir, err := os.MkdirTemp("/tmp", "csh-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req map[string]any
				_ = json.Unmarshal(line, &req)
				if resp := handle(req); resp != "" {
					_, _ = c.Write([]byte(resp + "\n"))
				} else {
					time.Sleep(2 * time.Second)
				}
			}(conn)
		}
	}()
	return path
}

func TestClientCallOverSocket(t *testing.T) {
	path := fakeServer(t, func(req map[string]any) string {
		switch req["method"] {
		case "ping":
			return `{"id":"` + req["id"].(string) + `","result":{"type":"pong","version":"0.9.5"}}`
		case "hang":
			return ""
		default:
			return `{"id":"` + req["id"].(string) + `","error":{"code":"unknown_method","message":"nope"}}`
		}
	})
	c := &Client{SocketPath: path, Timeout: 300 * time.Millisecond}
	v, err := c.Ping(context.Background())
	if err != nil || v != "0.9.5" {
		t.Fatalf("Ping = %q, %v", v, err)
	}
	if _, err := c.Call(context.Background(), "bogus", nil); err == nil || !strings.Contains(err.Error(), "unknown_method") {
		t.Errorf("error response = %v", err)
	}
	start := time.Now()
	if _, err := c.Call(context.Background(), "hang", nil); err == nil {
		t.Error("hung server should time out")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("timeout not honoured: %s", time.Since(start))
	}
	missing := &Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock"), Timeout: time.Second}
	if _, err := missing.Ping(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("missing socket error = %v, want ErrUnavailable", err)
	}
}
