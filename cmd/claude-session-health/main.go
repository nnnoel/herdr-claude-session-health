// Command claude-session-health is the Herdr plugin binary. Herdr runs it as
// a startup hook (start), as actions (start/stop/restart/open/diag) and as a
// pane entrypoint (ui). `run` is the detached background monitor.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"claude-session-health/internal/analysis"
	"claude-session-health/internal/collect"
	"claude-session-health/internal/config"
	"claude-session-health/internal/daemon"
	"claude-session-health/internal/diag"
	"claude-session-health/internal/fsutil"
	"claude-session-health/internal/herdr"
	"claude-session-health/internal/logx"
	"claude-session-health/internal/monitor"
	"claude-session-health/internal/resume"
	"claude-session-health/internal/state"
	"claude-session-health/internal/ui"
)

const (
	version        = "0.1.0"
	defaultID      = "claude-session-health"
	paneEntrypoint = "dashboard"
	logMaxBytes    = 1 << 20
)

const usage = `usage: claude-session-health <command>

  start                start the background monitor (idempotent)
  stop                 stop the background monitor
  restart              stop, then start (use after editing config.json)
  run                  run the monitor in the foreground (used by start)
  ui                   show the dashboard in this terminal
  open                 open the dashboard as a Herdr plugin pane
  status               print a one-screen summary
  diag [--copy]        print (or copy to the clipboard) a diagnostic report
  config-check [path]  validate a config file and print the effective config
  version              print the version`

type paths struct {
	pluginID  string
	socket    string
	stateDir  string
	configDir string
	exe       string
	home      string
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	p, err := resolvePaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-session-health:", err)
		return 1
	}
	cmd, rest := args[0], args[1:]
	switch {
	case cmd == "start" && len(rest) == 0:
		return report(daemon.Start(p.stateDir, p.exe, version))
	case cmd == "stop" && len(rest) == 0:
		return report(daemon.Stop(p.stateDir, 5*time.Second))
	case cmd == "restart" && len(rest) == 0:
		if code := report(daemon.Stop(p.stateDir, 5*time.Second)); code != 0 {
			return code
		}
		return report(daemon.Start(p.stateDir, p.exe, version))
	case cmd == "run" && len(rest) == 0:
		return runDaemon(p)
	case cmd == "ui" && len(rest) == 0:
		return runUI(p)
	case cmd == "open" && len(rest) == 0:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := herdr.NewClient(p.socket).OpenPluginPane(ctx, p.pluginID, paneEntrypoint); err != nil {
			fmt.Fprintln(os.Stderr, "open dashboard:", err)
			return 1
		}
		return 0
	case cmd == "status" && len(rest) == 0:
		return printStatus(p)
	case cmd == "diag" && (len(rest) == 0 || (len(rest) == 1 && rest[0] == "--copy")):
		return runDiag(p, len(rest) == 1)
	case cmd == "config-check" && len(rest) <= 1:
		path := filepath.Join(p.configDir, config.FileName)
		if len(rest) == 1 {
			path = rest[0]
		}
		return configCheck(path)
	case (cmd == "version" || cmd == "--version") && len(rest) == 0:
		fmt.Println("claude-session-health", version)
		return 0
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
}

func report(msg string, err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-session-health:", err)
		return 1
	}
	fmt.Println(msg)
	return 0
}

// resolvePaths uses Herdr's plugin environment and falls back to fixed
// locations so commands also work from a plain shell.
func resolvePaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, fmt.Errorf("find home directory: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return paths{}, fmt.Errorf("find executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	p := paths{
		pluginID:  envOr("HERDR_PLUGIN_ID", defaultID),
		socket:    envOr("HERDR_SOCKET_PATH", filepath.Join(home, ".config", "herdr", "herdr.sock")),
		configDir: os.Getenv("HERDR_PLUGIN_CONFIG_DIR"),
		exe:       exe,
		home:      home,
	}
	// Fallbacks match the directories Herdr 0.9.5 assigns, so status/diag
	// also work from a plain shell.
	stateRoot := os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if stateRoot == "" {
		stateRoot = filepath.Join(home, ".local", "state", "herdr", "plugins", p.pluginID)
	}
	if p.configDir == "" {
		p.configDir = filepath.Join(home, ".config", "herdr", "plugins", "config", p.pluginID)
	}
	p.stateDir = state.Dir(stateRoot, p.socket)
	return p, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runDaemon(p paths) (code int) {
	lock, ok, err := daemon.TryLockWait(p.stateDir, 10, 100*time.Millisecond)
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-session-health:", err)
		return 1
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "claude-session-health: another monitor already runs for this Herdr session")
		return 0
	}
	defer lock.Release()

	log, err := logx.New(filepath.Join(p.stateDir, state.LogFile), logMaxBytes, 200)
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-session-health:", err)
		return 1
	}
	defer log.Close()
	defer func() {
		if r := recover(); r != nil {
			log.Logf(logx.Lifecycle, "monitor panicked: %v", r)
			code = 1
		}
	}()
	if err := daemon.WritePID(p.stateDir, version, p.exe); err != nil {
		log.Logf(logx.Lifecycle, "cannot write pid file: %v", err)
		return 1
	}
	defer daemon.RemovePID(p.stateDir)

	cfg, warnings := config.Load(filepath.Join(p.configDir, config.FileName))
	client := herdr.NewClient(p.socket)
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 3*time.Second)
	herdrVersion, err := client.Ping(pingCtx)
	cancelPing()
	if err != nil {
		log.Logf(logx.Lifecycle, "herdr ping failed (continuing): %v", err)
	}
	runner := collect.ExecRunner{Timeout: 3 * time.Second}
	configDirEnv := os.Getenv("CLAUDE_CONFIG_DIR")
	mon := monitor.New(cfg, warnings, monitor.Deps{
		Herdr:        client,
		Procs:        collect.PS{Runner: runner},
		System:       collect.System{Runner: runner},
		Notifier:     client,
		Log:          log,
		StateDir:     p.stateDir,
		PluginID:     p.pluginID,
		Version:      version,
		HerdrVersion: herdrVersion,
		Resume: func(sessionID, source, cwd string) resume.Info {
			return resume.Lookup(sessionID, source, cwd, p.home, configDirEnv)
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	if err := mon.Run(ctx); err != nil && !errors.Is(err, monitor.ErrDisabled) {
		fmt.Fprintln(os.Stderr, "claude-session-health:", err)
		return 1
	}
	return 0
}

func runUI(p paths) int {
	client := herdr.NewClient(p.socket)
	app := &ui.App{
		SnapshotPath: filepath.Join(p.stateDir, state.SnapshotFile),
		LogPath:      filepath.Join(p.stateDir, state.LogFile),
		DiagEnv:      diagEnv(p),
		Focus:        client.FocusAgent,
		Runner:       collect.ExecRunner{Timeout: 3 * time.Second},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT)
	defer stop()
	if err := app.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "claude-session-health:", err)
		return 1
	}
	return 0
}

func diagEnv(p paths) map[string]string {
	return map[string]string{
		"plugin_version": version,
		"platform":       runtime.GOOS + "/" + runtime.GOARCH,
		"go_version":     runtime.Version(),
		"state_dir":      p.stateDir,
		"config_file":    filepath.Join(p.configDir, config.FileName),
		"daemon_running": fmt.Sprint(daemon.Running(p.stateDir)),
	}
}

func runDiag(p paths, copyIt bool) int {
	snap, snapErr := state.Read(filepath.Join(p.stateDir, state.SnapshotFile))
	var tail []string
	if data, err := fsutil.ReadTail(filepath.Join(p.stateDir, state.LogFile), 64*1024); err == nil {
		tail = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(tail) > 100 {
			tail = tail[len(tail)-100:]
		}
	}
	text := diag.Report(snap, snapErr, tail, diagEnv(p), time.Now())
	if !copyIt {
		fmt.Print(text)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ui.CopyToClipboard(ctx, text); err != nil {
		fmt.Fprintln(os.Stderr, "copy diagnostics:", err)
		return 1
	}
	_, _, _ = herdr.NewClient(p.socket).Notify(ctx, "Claude session health", "Diagnostic report copied to the clipboard.")
	fmt.Println("diagnostic report copied to the clipboard")
	return 0
}

func printStatus(p paths) int {
	snap, err := state.Read(filepath.Join(p.stateDir, state.SnapshotFile))
	running := daemon.Running(p.stateDir)
	if err != nil {
		fmt.Printf("monitor running: %t\nno snapshot: %v\n", running, err)
		return 1
	}
	now := time.Now()
	fresh := "current"
	if snap.IsStale(now) {
		fresh = "STALE (not current)"
	}
	fmt.Printf("monitor: %s, running=%t, snapshot %s ago (%s)\n", snap.Monitor.Status, running,
		analysis.Dur(snap.Age(now)), fresh)
	if snap.Monitor.LastError != "" {
		fmt.Printf("last error: %s\n", snap.Monitor.LastError)
	}
	if sys := snap.System; sys != nil {
		fmt.Printf("system: %s — %s\n", sys.Status, strings.Join(sys.Reasons, "; "))
	}
	for _, s := range snap.Sessions {
		tag := string(s.State)
		if snap.SessionStale(s, now) {
			tag += " (stale)"
		}
		reason := ""
		if len(s.Reasons) > 0 {
			reason = s.Reasons[0]
		}
		fmt.Printf("%-8s %-24s rss %-8s peak %-8s cpu %5.1f%%  %s — %s\n", s.PaneID, tag,
			analysis.MB(float64(s.RSSKB)), analysis.MB(float64(s.PeakKB)), s.CPUPercent, filepath.Base(s.Cwd), reason)
	}
	return 0
}

func configCheck(path string) int {
	cfg, warnings := config.Load(path)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode config:", err)
		return 1
	}
	fmt.Printf("effective config (%s):\n%s\n", path, data)
	if len(warnings) > 0 {
		return 1
	}
	return 0
}
