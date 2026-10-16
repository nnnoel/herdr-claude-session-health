// Package alerting turns conditions into user-visible alerts with
// cooldowns, verbosity filtering and recovery notices.
//
// Each alert key is a condition episode: Raise while the condition holds,
// Resolve when it clears. An episode notifies at most once; a new episode
// within the cooldown of the previous notification is recorded but not
// notified until the cooldown expires (flap protection).
package alerting

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"claude-session-health/internal/config"
	"claude-session-health/internal/logx"
	"claude-session-health/internal/ring"
)

// Kind classifies alerts.
type Kind string

// Alert kinds.
const (
	KindLeak           Kind = "suspected_leak"
	KindGrowth         Kind = "sustained_growth"
	KindBudget         Kind = "budget_exceeded"
	KindElevated       Kind = "elevated"
	KindSystemPressure Kind = "system_pressure"
	KindSwapGrowth     Kind = "swap_growth"
	KindStale          Kind = "monitor_stale"
	KindFailed         Kind = "monitor_failed"
	KindRecovery       Kind = "recovery"
)

// Level is alert severity.
type Level string

// Alert levels.
const (
	Info     Level = "info"
	Warning  Level = "warn"
	Critical Level = "critical"
)

// Alert is one recorded alert event.
type Alert struct {
	At         time.Time `json:"at"`
	Key        string    `json:"key"`
	SessionKey string    `json:"session_key,omitempty"`
	Kind       Kind      `json:"kind"`
	Level      Level     `json:"level"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Notified   bool      `json:"notified"`
	Suppressed string    `json:"suppressed,omitempty"`
}

// Notifier delivers a notification and reports whether it was shown.
type Notifier interface {
	Notify(ctx context.Context, title, body string) (shown bool, reason string, err error)
}

// minVerbosity is the least verbose setting at which a kind notifies.
func minVerbosity(kind Kind, level Level) config.Verbosity {
	switch kind {
	case KindLeak, KindBudget, KindFailed:
		return config.Quiet
	case KindSystemPressure:
		if level == Critical {
			return config.Quiet
		}
		return config.Normal
	case KindElevated:
		return config.Verbose
	default:
		return config.Normal
	}
}

type episode struct {
	active       bool
	notified     bool // this episode produced a notification
	lastNotified time.Time
	retryAt      time.Time // when an undelivered episode may try again; zero = never
	kind         Kind
	level        Level
	sessionKey   string
	title, body  string
}

// Engine tracks alert episodes. It is not safe for concurrent use; the
// monitor drives it from its single sampling goroutine.
type Engine struct {
	Cooldown      time.Duration
	Verbosity     config.Verbosity
	Notifications bool
	notifier      Notifier
	log           *logx.Logger
	episodes      map[string]*episode
	recent        *ring.Ring[Alert]
}

// New creates an engine keeping at most recentCap alerts in memory.
func New(cooldown time.Duration, verbosity config.Verbosity, notifications bool, n Notifier, log *logx.Logger, recentCap int) *Engine {
	return &Engine{
		Cooldown: cooldown, Verbosity: verbosity, Notifications: notifications,
		notifier: n, log: log, episodes: map[string]*episode{}, recent: ring.New[Alert](recentCap),
	}
}

// Raise asserts that the condition for key holds now. It returns the alert
// recorded by this call; repeat raises of an active episode record nothing.
func (e *Engine) Raise(ctx context.Context, now time.Time, key, sessionKey string, kind Kind, level Level, title, body string) (Alert, bool) {
	ep, ok := e.episodes[key]
	if !ok {
		ep = &episode{}
		e.episodes[key] = ep
	}
	ep.kind, ep.level, ep.sessionKey, ep.title, ep.body = kind, level, sessionKey, title, body
	if ep.active {
		// An episode held back by the cooldown or a failed delivery tries
		// again once, when its retry time arrives.
		if ep.notified || ep.retryAt.IsZero() || now.Before(ep.retryAt) || !e.allowed(kind, level) {
			return Alert{}, false
		}
		return e.deliver(ctx, now, key, ep, "Still active: "+title), true
	}
	ep.active, ep.notified, ep.retryAt = true, false, time.Time{}
	return e.deliver(ctx, now, key, ep, title), true
}

// Resolve declares that the condition for key cleared. A recovery notice is
// sent only if the episode had produced a notification.
func (e *Engine) Resolve(ctx context.Context, now time.Time, key, body string) (Alert, bool) {
	ep, ok := e.episodes[key]
	if !ok || !ep.active {
		return Alert{}, false
	}
	ep.active = false
	a := Alert{
		At: now, Key: key, SessionKey: ep.sessionKey, Kind: KindRecovery, Level: Info,
		Title: "Recovered: " + ep.title, Body: body,
	}
	switch {
	case !ep.notified:
		a.Suppressed = "original alert was not notified"
	case !e.Notifications:
		a.Suppressed = "notifications disabled"
	case e.Verbosity.Rank() < config.Normal.Rank():
		a.Suppressed = "verbosity " + string(e.Verbosity)
	default:
		a.Notified, a.Suppressed = e.send(ctx, a.Title, a.Body)
	}
	e.record(a)
	return a, true
}

// Active reports whether key is in an active episode.
func (e *Engine) Active(key string) bool {
	ep, ok := e.episodes[key]
	return ok && ep.active
}

// ActiveKeys returns active keys with the given prefix, sorted.
func (e *Engine) ActiveKeys(prefix string) []string {
	var keys []string
	for k, ep := range e.episodes {
		if ep.active && strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// Forget silently drops episodes for a session that no longer exists.
func (e *Engine) Forget(sessionKey string) {
	for k, ep := range e.episodes {
		if ep.sessionKey == sessionKey {
			delete(e.episodes, k)
		}
	}
}

// Recent returns recorded alerts, oldest first.
func (e *Engine) Recent() []Alert { return e.recent.Slice() }

// EpisodeCount reports how many episodes are tracked.
func (e *Engine) EpisodeCount() int { return len(e.episodes) }

func (e *Engine) allowed(kind Kind, level Level) bool {
	return e.Notifications && e.Verbosity.Rank() >= minVerbosity(kind, level).Rank()
}

func (e *Engine) deliver(ctx context.Context, now time.Time, key string, ep *episode, title string) Alert {
	a := Alert{At: now, Key: key, SessionKey: ep.sessionKey, Kind: ep.kind, Level: ep.level, Title: title, Body: ep.body}
	ep.retryAt = time.Time{}
	switch {
	case !e.Notifications:
		a.Suppressed = "notifications disabled"
	case !e.allowed(ep.kind, ep.level):
		a.Suppressed = "verbosity " + string(e.Verbosity)
	case !ep.lastNotified.IsZero() && now.Sub(ep.lastNotified) < e.Cooldown:
		a.Suppressed = fmt.Sprintf("cooldown: last notified %s ago", now.Sub(ep.lastNotified).Round(time.Second))
		ep.retryAt = ep.lastNotified.Add(e.Cooldown)
	default:
		a.Notified, a.Suppressed = e.send(ctx, a.Title, a.Body)
		if a.Notified {
			ep.notified, ep.lastNotified = true, now
		} else {
			ep.retryAt = now.Add(e.Cooldown)
		}
	}
	e.record(a)
	return a
}

func (e *Engine) send(ctx context.Context, title, body string) (bool, string) {
	if e.notifier == nil {
		return false, "no notifier"
	}
	shown, reason, err := e.notifier.Notify(ctx, title, body)
	if err != nil {
		return false, "notify failed: " + err.Error()
	}
	if !shown {
		if reason == "" {
			reason = "not shown"
		}
		return false, "herdr: " + reason
	}
	return true, ""
}

func (e *Engine) record(a Alert) {
	e.recent.Push(a)
	if a.Notified {
		e.log.Logf(logx.Alert, "notified kind=%s key=%s", a.Kind, a.Key)
	} else {
		e.log.Logf(logx.Alert, "recorded without notification kind=%s key=%s reason=%q", a.Kind, a.Key, a.Suppressed)
	}
}
