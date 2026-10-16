# Architecture

Claude Code Session Health is one Go binary (standard library only) with three
roles, selected by subcommand:

| Role | Started by | Does |
|------|-----------|------|
| `run` (daemon) | `start` (startup hook / action), detached with `setsid` | Samples, analyzes, alerts, persists, publishes `snapshot.json` |
| `ui` (dashboard) | Herdr plugin pane `dashboard` | Renders `snapshot.json`; never samples, so it can't block or be blocked |
| `start`/`stop`/`restart`/`open`/`status`/`diag`/`config-check` | Herdr actions or a shell | Lifecycle and diagnostics |

```
            Herdr socket (newline JSON, 1 request/connection)
                 │ agent.list  pane.process_info  pane.report_metadata
                 │ notification.show  plugin.list  agent.focus
                 ▼
┌──────────── daemon: monitor.Tick every interval (single goroutine) ───────────┐
│ collect      ps -axo …ucomm  (1 call)      sysctl + vm_stat (2 calls)        │
│ attribution  SelectRoot (verify claude ↔ pane shell) → Measure (disjoint      │
│              process trees, no PID counted twice) → Orphans (reported only)  │
│ history      per-session runs (PID+start time) → raw ring + 1-min buckets    │
│ analysis     Evaluate (windows, Theil–Sen, thresholds) → Tracker (hysteresis)│
│ systemhealth pressure persistence, swap growth                               │
│ alerting     episodes, cooldown, verbosity, recovery → notification.show     │
│ sidebar      pane.report_metadata token "memhealth" with TTL                 │
│ persistence  snapshot.json each tick, history.json each minute (atomic)      │
└──────────────────────────────────────────────────────────────────────────────┘
                 │ snapshot.json (read each second)
                 ▼
             ui: Render(Model) → raw-mode terminal in a Herdr pane
```

## Packages

| Package | Responsibility |
|---------|---------------|
| `collect` | `ps`/`sysctl`/`vm_stat` runners and parsers. Platform code is in `system_darwin.go`; `system_other.go` is the stub to replace for Linux |
| `herdr` | Socket client. Decodes only the fields it needs; command lines and terminal titles are never decoded |
| `attribution` | Root selection with confidence, tree measurement, orphan reporting |
| `history` | Bounded rings, runs and baselines, persistence |
| `analysis` | Growth windows, state candidates, and the hysteresis tracker |
| `systemhealth` | System pressure and swap verdict |
| `alerting` | Alert episodes, cooldowns, verbosity, recovery notices |
| `monitor` | Orchestration, sidebar tokens, snapshot building |
| `state` | Snapshot schema and state-dir layout |
| `ui` | Pure `Render`/`HandleKey` plus the terminal loop and terminate/clipboard side effects |
| `daemon` | flock, PID file, detached spawn, verified stop |
| `diag`, `resume`, `logx`, `ring`, `fsutil`, `config` | Support |

## Key decisions

- **Socket, not CLI.** One short-lived socket connection per call instead of
  spawning `herdr` each tick. A tick costs one `agent.list` and one `ps`,
  `sysctl` and `vm_stat` each. It costs one `pane.process_info` per pane only
  when the cached root vanishes, its start time changes, or 60 s pass.
- **Session identity.** The key is the Herdr `terminal_id`, which survives
  pane moves. A *run* is the Claude PID plus its start time, so a restart or a
  reused PID starts a new baseline.
- **No overlap.** One goroutine runs the ticker, and an atomic guard rejects
  re-entrant ticks. Each tick's context is bounded by the interval, and
  every subprocess has its own 3 s timeout.
- **No duplicate loops.** `daemon.lock` (flock) is per Herdr socket. `start`
  is idempotent and replaces a daemon from another version or path.
- **Stale is explicit.** The UI compares snapshot and sample timestamps with
  `3 × interval + 2 s`, and the sidebar token has the same TTL, so old data
  can never look current.
- **Privacy.** Process names come from `ps ucomm` (first token only), never
  `comm` or command lines, because macOS `comm` reflects self-assigned
  process titles that can contain arguments and credentials.
- **Platform isolation.** Only `collect/system_*.go`, `ui/term_*.go`,
  `ui/clipboard_*.go` and the unix-only `daemon`/`ui/process_unix.go` touch
  the OS. `GOOS=linux go vet ./...` runs in `make vet`.
