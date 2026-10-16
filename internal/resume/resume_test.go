package resume

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const id = "2aaebfe0-71a7-4dd1-a5f7-727c1573ec6a"

func writeTranscript(t *testing.T, configDir string) {
	t.Helper()
	dir := filepath.Join(configDir, "projects", "-tmp-proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLookup(t *testing.T) {
	t.Run("default config dir", func(t *testing.T) {
		home := t.TempDir()
		writeTranscript(t, filepath.Join(home, ".claude"))
		got := Lookup(id, "herdr:claude", "/tmp/my proj", home, "")
		if !got.Verified || got.Command != "cd '/tmp/my proj' && claude --resume "+id {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("alternate config dir found by glob", func(t *testing.T) {
		home := t.TempDir()
		alt := filepath.Join(home, ".claude-personal")
		writeTranscript(t, alt)
		got := Lookup(id, "herdr:claude", "/tmp/p", home, "")
		if !got.Verified || !strings.Contains(got.Command, "CLAUDE_CONFIG_DIR="+Quote(alt)+" claude --resume") {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("CLAUDE_CONFIG_DIR with tilde", func(t *testing.T) {
		home := t.TempDir()
		writeTranscript(t, filepath.Join(home, "cfg"))
		if got := Lookup(id, "", "", home, "~/cfg"); !got.Verified || strings.HasPrefix(got.Command, "cd ") {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("missing transcript is unverified", func(t *testing.T) {
		got := Lookup(id, "herdr:claude", "/tmp/p", t.TempDir(), "")
		if got.Verified || got.Command == "" || !strings.Contains(got.Note, "unverified") {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("rejects non-uuid and foreign sources", func(t *testing.T) {
		for _, tc := range []struct{ id, src string }{{"", ""}, {"../../etc/passwd", ""}, {"*", ""}, {id, "plugin:other"}} {
			if got := Lookup(tc.id, tc.src, "/tmp", t.TempDir(), ""); got.Command != "" || got.Verified {
				t.Errorf("Lookup(%q, %q) = %+v", tc.id, tc.src, got)
			}
		}
	})
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/tmp/a":      "/tmp/a",
		"/tmp/a b":    "'/tmp/a b'",
		"it's":        `'it'\''s'`,
		"$(rm -rf /)": "'$(rm -rf /)'",
		"":            "''",
		"/x;echo pwn": "'/x;echo pwn'",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}
