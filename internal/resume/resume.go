// Package resume builds the `claude --resume` command for a session and
// verifies it by checking that the session's transcript file exists. It only
// stats file names; it never opens or reads transcripts.
package resume

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Info is a resume command and whether it was verified.
type Info struct {
	Command  string `json:"command,omitempty"`
	Verified bool   `json:"verified"`
	Note     string `json:"note"`
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Lookup builds the resume command for sessionID. home is the user's home
// directory and configDirEnv the daemon's CLAUDE_CONFIG_DIR (may be empty).
func Lookup(sessionID, source, cwd, home, configDirEnv string) Info {
	if sessionID == "" {
		return Info{Note: "Herdr has not reported a Claude session id for this pane"}
	}
	if !uuidRE.MatchString(sessionID) {
		return Info{Note: "reported session id is not a UUID; no resume command offered"}
	}
	if source != "" && !strings.HasPrefix(source, "herdr:claude") {
		return Info{Note: fmt.Sprintf("session id came from %q, not Herdr's Claude integration", source)}
	}

	defaultDir := filepath.Clean(filepath.Join(home, ".claude"))
	var dirs []string
	if configDirEnv != "" {
		dirs = append(dirs, expandHome(configDirEnv, home))
	}
	dirs = append(dirs, defaultDir)
	if extra, err := filepath.Glob(filepath.Join(home, ".claude-*")); err == nil {
		dirs = append(dirs, extra...)
	}

	seen := map[string]bool{}
	for _, dir := range dirs {
		dir = filepath.Clean(dir)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		matches, err := filepath.Glob(filepath.Join(dir, "projects", "*", sessionID+".jsonl"))
		if err != nil || len(matches) == 0 {
			continue
		}
		if info, err := os.Stat(matches[0]); err != nil || !info.Mode().IsRegular() {
			continue
		}
		env := ""
		if dir != defaultDir {
			env = "CLAUDE_CONFIG_DIR=" + Quote(dir) + " "
		}
		return Info{Command: command(cwd, env, sessionID), Verified: true, Note: "transcript found under " + dir}
	}
	return Info{
		Command: command(cwd, "", sessionID),
		Note:    "transcript file not found in any ~/.claude* directory; command is unverified",
	}
}

func command(cwd, env, id string) string {
	cmd := env + "claude --resume " + id
	if cwd != "" {
		cmd = "cd " + Quote(cwd) + " && " + cmd
	}
	return cmd
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// Quote single-quotes s for POSIX shells when it contains anything unusual.
func Quote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
