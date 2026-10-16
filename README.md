# Claude Code Session Health (Herdr plugin)

Watches every Claude Code session running in Herdr for **sustained memory
growth, possible leaks and system memory pressure**. You get a compact verdict
in the sidebar, a dashboard pane with history and reasons, and notifications
when something needs attention.

It complements [herdr-agent-status](https://github.com/yankewei/herdr-agent-status)
instead of duplicating it. That plugin shows live CPU and memory numbers;
this one answers *"is this session's memory behaving?"* over minutes and hours.
Its sidebar token shows a verdict (`ok`, `learning`, `high 2.3G`,
`↗ +420M/20m`, `leak? +1.1G/60m`, `stale`), not usage.

## What it detects

- **Sustained growth.** Resident memory (RSS) of the Claude process tree rises
  steadily across the growth window (20 min by default) and is still rising.
- **Suspected leak.** Sustained growth across the leak window (60 min), or
  steady growth while the session is idle.
- **Elevated memory / budget exceeded.** High but stable RSS above the warning
  threshold, or above the critical budget, for a minimum duration.
- **System memory pressure.** The kernel's pressure level persisting at warn
  or critical, and rapid swap growth.
- **Stale or failed monitoring.** Sampling failures, or a session whose Claude
  process can't be attributed.

## What it does not detect

- **It cannot prove a leak.** Every warning is a heuristic and is worded as
  one ("possible leak").
- **It ignores leaks smaller than the thresholds.** Defaults are ≥150 MB *and*
  ≥15% over the window.
- **It ignores one-off spikes and one-time steps.** These are deliberate
  non-signals.
- **It does not see inside Node.** It has no heap profiling and cannot tell
  JavaScript heap from native memory.
- **It only watches what Herdr detects.** Claude processes outside Herdr, and
  panes Herdr doesn't recognise as `claude`, are not monitored.

## Requirements

- macOS (verified on macOS 27 / Apple Silicon). Linux code paths compile but
  system metrics are not implemented yet.
- Herdr **0.9.5 or newer** (verified on 0.9.5).
- Go **1.23+** to build (verified with Go 1.27.2). There are no third-party
  dependencies.
- No root privileges. It uses `ps`, `sysctl`, `vm_stat`, `pbcopy` and the
  Herdr socket.

## Install

### From a local checkout (verified)

```sh
cd herdr-claude-session-health
make build                       # go build -trimpath -o bin/claude-session-health ./cmd/claude-session-health
herdr plugin link "$PWD"
herdr plugin action invoke claude-session-health.start
```

`herdr plugin link` does not run build steps or startup hooks, which is why
you build first and start once by hand. After that, Herdr starts the monitor
on every server start through the `[[startup]]` hook.

If Go comes from mise rather than your PATH, build with
`PATH="$(mise where go)/bin:$PATH" make build`.

### From GitHub (not yet verified)

After the repository is published with the `herdr-plugin` topic:

```sh
herdr plugin install <owner>/herdr-claude-session-health
```

Herdr runs the `[[build]]` step (`go build …`), so `go` must be on the PATH
that Herdr sees. Then invoke **Start Claude session health monitor** once, or
restart the Herdr server.

### Show the sidebar token

Add `$memhealth` to your agent sidebar rows in `~/.config/herdr/config.toml`,
then run `herdr server reload-config`:

```toml
[ui.sidebar.agents]
rows = [
  ["state_icon", "machine", "workspace", "tab"],
  ["agent", "$memhealth"],   # add "$usage" too if you use herdr-agent-status
]
```

The sidebar layout belongs to your Herdr config, so the plugin only reports
the value. The token expires automatically (TTL of 3 sampling intervals + 2 s)
if the monitor stops.

## Using it

| Action (Herdr command palette / `herdr plugin action invoke …`) | What it does |
|---|---|
| `claude-session-health.open` | Open the dashboard as a split pane |
| `claude-session-health.start` / `.stop` / `.restart` | Monitor lifecycle (`restart` reloads config) |
| `claude-session-health.diagnostics` | Copy a diagnostic report to the clipboard |

Dashboard keys: `↑↓`/`jk` select · `enter` details · `f` focus that
session's pane · `c` copy session diagnostics · `d` copy full report ·
`i` copy session id · `r` copy resume command · `x` terminate (asks for the
PID) · `?` help · `q` quit.

The list is sorted by severity. Each row shows the state, pane, directory,
RSS, peak, CPU, a 60-minute sparkline and the age of the last sample. The
detail view shows:
- process and attribution (with confidence and notes)
- the Claude vs. children memory split, top children and orphans
- history sparklines and both growth windows with numbers
- thresholds, recent alerts for the session, and the resume command

From a shell: `bin/claude-session-health status`, `… diag`, `… config-check`.

## Configuration

Copy `config.example.json` to the plugin config directory, then restart the monitor:

```sh
cp config.example.json "$(herdr plugin config-dir claude-session-health)/config.json"
bin/claude-session-health config-check          # validates and prints the effective config
herdr plugin action invoke claude-session-health.restart
```

Missing keys keep their defaults. Unknown keys reject the whole file
(defaults are used instead). An out-of-range value falls back to its default
with a warning, which is shown in the dashboard header, `diag` and the log.

| Key | Default | Meaning |
|---|---|---|
| `sample_interval_sec` | 5 | Sampling interval (2–60) |
| `warn_rss_mb` / `critical_rss_mb` | 2048 / 4096 | Elevated threshold / memory budget for one session tree |
| `elevated_min_minutes` | 5 | Time continuously above a threshold before it counts |
| `growth_window_minutes` / `leak_window_minutes` | 20 / 60 | Sustained-growth and leak windows |
| `min_growth_mb` / `min_growth_percent` | 150 / 15 | Both must be exceeded over a window |
| `min_up_fraction` | 0.6 | Share of minute-to-minute steps that must rise |
| `idle_after_minutes` / `idle_cpu_percent` | 5 / 3 | Idle detection (agent status first, CPU as fallback) |
| `recovery_minutes` | 10 | Hysteresis: how long a lower verdict must persist |
| `alert_cooldown_minutes` | 30 | Minimum gap between notifications for the same condition |
| `retention_hours` / `raw_retention_minutes` | 24 / 60 | History kept (minute buckets / raw samples) |
| `verbosity` | `normal` | `quiet`: leak, budget, critical pressure and monitor failure only. `normal`: adds growth, warn pressure, swap, stale and recoveries. `verbose`: adds elevated |
| `notifications` / `sidebar_token` | true / true | Turn Herdr notifications / the sidebar token off |
| `system_pressure_warnings` | true | System pressure and swap alerts |
| `pressure_sustain_minutes` | 2 | How long pressure must persist |
| `swap_growth_mb` / `swap_growth_window_minutes` | 1024 / 10 | Rapid swap growth threshold |

## Health states

| State | Sidebar | Meaning |
|---|---|---|
| Healthy | `ok` (`ok ↗` if trending up) | No sustained growth; below the warning threshold |
| Insufficient data | `learning` | Not enough history to judge the growth window, e.g. right after start or after a gap |
| Elevated memory | `high 2.3G` / `over 4.3G` | Above the warning threshold (or critical budget) for `elevated_min_minutes`; can be stable |
| Sustained growth | `↗ +420M/20m` | Steady, still-rising growth over the growth window |
| Suspected leak | `leak? +1.1G/60m` | Sustained growth over the leak window, or over the growth window while idle |
| *(stale)* | `stale` | No current sample. Values are shown as old, never as current |

Every non-healthy state carries a reason, for example *"RSS rose 412 MB (+37%)
steadily over 20 min (84% of minutes up) and is still rising at 21 MB/min"*.

## How metrics are calculated

- **Discovery.** Herdr's `agent.list` provides the panes running `claude`.
- **Attribution.** `pane.process_info` provides the pane's shell and
  foreground processes. Claude is recognised from its argv (Claude retitles
  itself to its version number). The root is checked against one `ps` snapshot:
  - **verified**: a direct child of the pane shell, in the foreground group
  - **probable**: reached through a wrapper process, or found by name only
  - **uncertain**: not under the shell, or several unrelated Claudes in one pane

  Every attribution carries a note explaining it.
- **Session RSS** = Claude's RSS + the RSS of all its descendants (the
  `ps` parent/child tree):
  - The walk never descends into another session's root.
  - A PID is attributed to at most one session.
  - Processes in the pane's group that are *not* under Claude (for example
    helpers reparented to launchd) are shown as "orphans" and never added.
- **CPU** is the summed CPU-time delta of the tree between samples, so it can
  exceed 100%. The first sample uses `ps %cpu` and is marked as an estimate.
- **History.** Raw samples are kept for `raw_retention_minutes`, and one-minute
  buckets (median, min, max, idle count) for `retention_hours`. History
  is per *run* (PID + start time), so a restart or reused PID gets a fresh
  baseline. Baseline = median of the first 12 samples of a run. Missing
  samples are gaps, never zeros.
- **Growth window test.** A window passes only if all of these hold:
  - coverage ≥ 70%
  - a positive Theil–Sen slope on the minute medians
  - an endpoint rise ≥ `min_growth_mb` *and* ≥ `min_growth_percent`
  - ≥ `min_up_fraction` of the minutes rising
  - no single minute accounting for more than half the rise
  - a second-half slope that is still ≥ 40% of the overall slope (not levelling off)
- **Hysteresis.** Escalation is immediate, because the evidence is already
  sustained. De-escalation needs `recovery_minutes` of lower verdicts, except
  when memory is clearly released (a drop of ≥ max(half the minimum growth,
  10% of the peak), with the last 5 minutes not rising). Data gaps hold the
  last verdict and never count as recovery.
- **System.**
  - `kern.memorystatus_vm_pressure_level` gives the pressure (normal, warn
    or critical).
  - `vm.swapusage` gives swap usage.
  - `vm_stat` gives compressed memory, and available memory approximated as
    free + inactive + speculative + purgeable pages.
  - `kern.memorystatus_level` gives the free percentage.

  Low free memory alone never warns, because macOS uses spare RAM as cache.
  Unreadable metrics show as *unavailable*, never as zero. System metrics are
  kept separate from session totals.

## Limitations of RSS and attribution

- **Summed RSS overstates physical memory.** Shared pages (libraries, shared
  memory) are counted once per process, so the session total is higher than
  the memory actually used.
- **RSS ignores compressed and swapped pages.** A leaking process under memory
  pressure can show *flat* RSS while its true footprint grows. System pressure
  and swap growth are reported separately for this reason.
- **Detached descendants are invisible.** Processes that leave the tree
  (double-forked daemons, reparented helpers) can't be attributed to a
  session. They appear as orphans only if they stay in the pane's process
  group.
- **Herdr's view of the pane limits attribution.** If Herdr doesn't report the
  foreground processes, attribution falls back to name matching and is
  marked *probable*.

## Alerts

Each alert names the pane and directory, the values, the time window and the
reason. A condition notifies **once per episode**.
- A new episode within `alert_cooldown_minutes` of the previous notification
  is recorded but notified only after the cooldown.
- Recovery notices are sent when a notified condition clears.
- Herdr refusing a notification (e.g. `rate_limited`) is retried at most once
  per cooldown.

All alerts, including suppressed ones with their reason, appear in the
dashboard and in `diag`. If sampling fails three times in a row, the monitor
raises *"sampling is failing"*, and session values are shown as stale.

## Recovery (manual only)

The plugin never acts on its own. Its recovery tools are:
- **Inspect.** The detail view.
- **Focus.** `f` focuses the session's pane.
- **Copy.** `c`, `d`, `i` and `r` copy diagnostics, the session id, or the
  **verified resume command**. "Verified" means the session's transcript file
  exists under a `~/.claude*` config directory. Only the file name is
  checked, never its contents, and `CLAUDE_CONFIG_DIR` is included when needed.
- **Terminate.** `x` shows the PID, start time, RSS and consequences, and
  requires typing the PID. Immediately before signalling, it re-checks the PID's
  start time and process name, and refuses if the PID now belongs to another
  process. It sends only `SIGTERM`. There is no automatic restart; resume with
  the copied command.

## Troubleshooting

| Symptom | Check |
|---|---|
| Dashboard says "No monitor data" | Invoke `claude-session-health.start`; then `herdr plugin log list --plugin claude-session-health` |
| `STALE` banner | The daemon stopped. Run `bin/claude-session-health status`, then check `daemon.stderr` and `monitor.log` in the state dir |
| A session is stuck on `learning` | Normal for the first `growth_window_minutes × 0.7`; also after gaps |
| `not sampled: no claude process found in pane` | Herdr lists the pane as `claude` but no process matched; see attribution notes in `diag` |
| Sidebar shows nothing | Add `$memhealth` to `ui.sidebar.agents.rows` and reload the config |
| Config changes ignored | Run `restart`; check `config-check` for warnings |

State dir: `~/.local/state/herdr/plugins/claude-session-health/<socket-hash>/`.
It holds `monitor.log` (+ `.1`), `snapshot.json`, `history.json`, `daemon.pid`,
`daemon.lock` and `daemon.stderr`. Run `bin/claude-session-health diag` for a
full report.

## Development and tests

```sh
make check     # gofmt check, go vet (darwin + linux), staticcheck, go test -race, build
make cover     # tests with coverage
make sec       # gosec (findings reviewed; see below)
```

Tests use fixtures and simulated data only. The one test that sends a real
signal terminates a `sleep` child that the test started itself. Required
scenarios and where they are tested:

| # | Scenario | Test |
|---|---|---|
| 1 | Stable memory → no warning | `analysis.TestStableMemoryNoWarning`, `TestHighButStableIsElevatedNotLeak` |
| 2 | Brief spike → no warning | `analysis.TestBriefSpikeNoWarning`, `TestSingleStepIsNotGrowth`, `TestLevellingOffIsNotGrowth` |
| 3 | Sustained growth warns only after criteria | `analysis.TestSustainedGrowthWarnsOnlyAfterCriteria`, `TestGrowthNeedsAbsoluteAndRelativeRise`, `TestGrowthWhileIdleIsSuspectedLeak` |
| 4 | Declining memory updates state | `analysis.TestDecliningMemoryUpdatesState`, `TestRecoveryHysteresis` |
| 5 | Missing samples aren't zero | `analysis.TestMissingSamplesAreNotZero`, `TestAboveThresholdIgnoresGaps`, `monitor.TestVanishedClaudeIsStaleNotZero` |
| 6 | Restart / reused PID resets baseline | `history.TestRestartAndPIDReuseResetBaseline`, `monitor.TestRestartInSamePaneResetsBaseline` |
| 7 | Sessions attributed independently | `attribution.TestMeasureIndependentAndNoDoubleCounting`, `monitor.TestSessionsAttributedIndependently` |
| 8 | No duplicate child counting | `attribution.TestMeasureIndependentAndNoDoubleCounting`, `TestSelectRootPrefersOutermostClaude`, `TestOrphansReportedNotCounted` |
| 9 | Alert cooldowns and recovery | `alerting.*` |
| 10 | History and logs stay bounded | `history.TestHistoryIsBounded`, `logx.TestLogRotationIsBounded`, `monitor.TestLongRunStaysBounded` |
| 11 | Malformed macOS output | `collect.TestParsePSHandlesMalformedOutput`, `TestProcNameDropsArguments`, `TestParseVMStat`, `TestParseSwapUsage`, `TestBuildSystemSampleReportsUnavailableNotZero` |
| 12 | Loop shuts down cleanly | `monitor.TestRunShutsDownCleanly`, `TestTicksNeverOverlap`, `TestStopsWhenHerdrGoneOrPluginDisabled` |

**gosec** reports 14 findings, all reviewed:
- **G204 (subprocess with a variable).** Only fixed commands are run (`ps`,
  `sysctl`, `vm_stat`, `pbcopy`, this binary), with no shell.
- **G304 (file path from a variable).** Paths come from Herdr's plugin state
  and config dirs.
- **G103 (`unsafe`).** Required for the termios ioctls.

### Manual macOS test with a synthetic workload

This uses a capped, self-terminating workload. It never exhausts memory or
touches real processes.

1. Link and start the plugin, then open the dashboard (`claude-session-health.open`).
2. In a Claude Code session inside Herdr, ask Claude to run, *in the
   background*, `python3 scripts/synthetic-growth.py --mb-per-min 15 --cap-mb 900 --minutes 45`.
   It becomes a descendant of that session's `claude` process, touches pages
   so RSS really grows, never exceeds 900 MB, and exits after 45 minutes.
3. Expect, for that session only:
   - `learning` → `↗` **Sustained growth** after about 14–21 minutes, with a
     reason quoting the rise. The other sessions stay `ok`.
   - **Suspected leak** if growth continues across most of the 60-minute window.
   - A notification (verbosity `normal`) and an entry in the session's
     recent alerts.
4. When the script exits, its memory is released. The state returns to
   healthy promptly (a "memory declining" reason) instead of waiting for the
   recovery hold, and a recovery notice follows.
5. Optional: stop the monitor for 30 s (`claude-session-health.stop`). The
   dashboard and sidebar must show **STALE/stale**, not old numbers as current.
   Then `start` it again; history continues because it persisted.

## Uninstall and clearing history

```sh
herdr plugin action invoke claude-session-health.stop
herdr plugin unlink claude-session-health      # or: herdr plugin uninstall claude-session-health
rm -rf ~/.local/state/herdr/plugins/claude-session-health                      # history, logs, snapshot
rm -rf "$(herdr plugin config-dir claude-session-health)"                       # your config (optional)
```

To clear only history, stop the monitor and delete `history.json` in the state
dir. A running monitor also exits on its own within 30 s of the plugin being
disabled or uninstalled.

## Privacy and retention

- **Never read or stored:** prompts, conversation content, transcripts,
  terminal titles, and process command lines. Process names come from the
  kernel's short accounting name, first token only, because macOS `comm`
  can contain a retitled process's arguments, including credentials.
- **Stored locally:**
  - pane ids
  - Herdr terminal ids
  - Claude session ids
  - working directories
  - PIDs and short process names
  - memory/CPU numbers
  - alert texts
- **Retention:**
  - history: `retention_hours` (24 h by default)
  - raw samples: `raw_retention_minutes`
  - log: at most about 2 MB (1 MB plus one rotated backup)
  - alerts: the last 50
- **Nothing leaves the machine.** Diagnostics are copied to the clipboard
  only when you ask.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design.
