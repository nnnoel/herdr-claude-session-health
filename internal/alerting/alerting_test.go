package alerting

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"claude-session-health/internal/config"
)

type fakeNotifier struct {
	sent   []string
	shown  bool
	reason string
	err    error
}

func (f *fakeNotifier) Notify(_ context.Context, title, _ string) (bool, string, error) {
	f.sent = append(f.sent, title)
	return f.shown, f.reason, f.err
}

var t0 = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)

func newEngine(v config.Verbosity) (*Engine, *fakeNotifier) {
	n := &fakeNotifier{shown: true}
	return New(30*time.Minute, v, true, n, nil, 50), n
}

// 9. Cooldowns, single notification per episode, and recovery transitions.
func TestEpisodeNotifiesOnceAndRecovers(t *testing.T) {
	e, n := newEngine(config.Normal)
	ctx := context.Background()
	a, ok := e.Raise(ctx, t0, "s:growth", "s", KindGrowth, Warning, "Sustained growth", "RSS rose 400 MB over 20 min")
	if !ok || !a.Notified {
		t.Fatalf("first raise: %+v", a)
	}
	for i := 1; i < 100; i++ {
		if _, ok := e.Raise(ctx, t0.Add(time.Duration(i)*5*time.Second), "s:growth", "s", KindGrowth, Warning, "x", "y"); ok {
			t.Fatal("repeat raises of an active episode must not record or notify")
		}
	}
	if len(n.sent) != 1 {
		t.Fatalf("sent %d notifications, want 1", len(n.sent))
	}
	rec, ok := e.Resolve(ctx, t0.Add(20*time.Minute), "s:growth", "Memory growth stopped.")
	if !ok || !rec.Notified || rec.Kind != KindRecovery || !strings.HasPrefix(rec.Title, "Recovered: ") {
		t.Errorf("recovery = %+v", rec)
	}
	if _, ok := e.Resolve(ctx, t0.Add(21*time.Minute), "s:growth", ""); ok {
		t.Error("resolving an inactive episode must be a no-op")
	}
}

func TestCooldownSuppressesFlappingThenRetriesOnce(t *testing.T) {
	e, n := newEngine(config.Normal)
	ctx := context.Background()
	e.Raise(ctx, t0, "k", "s", KindGrowth, Warning, "growth", "b")
	e.Resolve(ctx, t0.Add(5*time.Minute), "k", "ok")
	a, _ := e.Raise(ctx, t0.Add(10*time.Minute), "k", "s", KindGrowth, Warning, "growth", "b")
	if a.Notified || !strings.HasPrefix(a.Suppressed, "cooldown") {
		t.Fatalf("re-raise inside cooldown: %+v", a)
	}
	for m := 11; m < 30; m++ {
		if _, ok := e.Raise(ctx, t0.Add(time.Duration(m)*time.Minute), "k", "s", KindGrowth, Warning, "growth", "b"); ok {
			t.Fatalf("notified/recorded at %d min, before the cooldown expired", m)
		}
	}
	a, ok := e.Raise(ctx, t0.Add(30*time.Minute), "k", "s", KindGrowth, Warning, "growth", "b")
	if !ok || !a.Notified || !strings.HasPrefix(a.Title, "Still active") {
		t.Errorf("after cooldown: %+v", a)
	}
	if _, ok := e.Raise(ctx, t0.Add(31*time.Minute), "k", "s", KindGrowth, Warning, "growth", "b"); ok {
		t.Error("only one retry per episode")
	}
	if len(n.sent) != 3 { // first, recovery, still-active
		t.Errorf("sent %v", n.sent)
	}
}

func TestDeliveryFailureDoesNotSpam(t *testing.T) {
	e, n := newEngine(config.Normal)
	n.shown, n.reason = false, "rate_limited"
	ctx := context.Background()
	a, _ := e.Raise(ctx, t0, "k", "s", KindLeak, Critical, "leak", "b")
	if a.Notified || a.Suppressed != "herdr: rate_limited" {
		t.Fatalf("got %+v", a)
	}
	recorded := 0
	for i := 1; i < 600; i++ { // 50 minutes of 5 s ticks
		if _, ok := e.Raise(ctx, t0.Add(time.Duration(i)*5*time.Second), "k", "s", KindLeak, Critical, "leak", "b"); ok {
			recorded++
		}
	}
	if recorded != 1 {
		t.Errorf("recorded %d retries in 50 min, want exactly 1 after the cooldown", recorded)
	}
	failing := &fakeNotifier{err: errors.New("socket closed")}
	e2 := New(time.Minute, config.Normal, true, failing, nil, 5)
	if a, _ := e2.Raise(ctx, t0, "x", "", KindFailed, Critical, "t", "b"); a.Notified || !strings.Contains(a.Suppressed, "socket closed") {
		t.Errorf("notifier error: %+v", a)
	}
}

func TestVerbosityFiltering(t *testing.T) {
	tests := []struct {
		v      config.Verbosity
		kind   Kind
		level  Level
		notify bool
	}{
		{config.Quiet, KindLeak, Critical, true},
		{config.Quiet, KindBudget, Critical, true},
		{config.Quiet, KindGrowth, Warning, false},
		{config.Quiet, KindSystemPressure, Warning, false},
		{config.Quiet, KindSystemPressure, Critical, true},
		{config.Normal, KindGrowth, Warning, true},
		{config.Normal, KindStale, Warning, true},
		{config.Normal, KindElevated, Info, false},
		{config.Verbose, KindElevated, Info, true},
	}
	for _, tt := range tests {
		e, n := newEngine(tt.v)
		a, _ := e.Raise(context.Background(), t0, "k", "", tt.kind, tt.level, "t", "b")
		if a.Notified != tt.notify || (len(n.sent) == 1) != tt.notify {
			t.Errorf("%s %s/%s: notified=%t, want %t (%s)", tt.v, tt.kind, tt.level, a.Notified, tt.notify, a.Suppressed)
		}
		if !tt.notify && !strings.HasPrefix(a.Suppressed, "verbosity") {
			t.Errorf("suppression reason = %q", a.Suppressed)
		}
	}
	e, n := newEngine(config.Quiet)
	e.Raise(context.Background(), t0, "k", "", KindLeak, Critical, "t", "b")
	if rec, _ := e.Resolve(context.Background(), t0.Add(time.Hour), "k", "ok"); rec.Notified || len(n.sent) != 1 {
		t.Errorf("quiet mode must not send recovery notices: %+v", rec)
	}
}

func TestNotificationsDisabled(t *testing.T) {
	n := &fakeNotifier{shown: true}
	e := New(time.Minute, config.Verbose, false, n, nil, 5)
	a, _ := e.Raise(context.Background(), t0, "k", "", KindLeak, Critical, "t", "b")
	if a.Notified || a.Suppressed != "notifications disabled" || len(n.sent) != 0 {
		t.Errorf("got %+v", a)
	}
	if len(e.Recent()) != 1 {
		t.Error("suppressed alerts are still recorded for the dashboard")
	}
}

func TestRecentAlertsBoundedAndForget(t *testing.T) {
	e, _ := newEngine(config.Normal)
	for i := 0; i < 500; i++ {
		e.Raise(context.Background(), t0.Add(time.Duration(i)*time.Second), fmt.Sprintf("s:%d", i), "s", KindGrowth, Warning, "t", "b")
	}
	if got := len(e.Recent()); got != 50 {
		t.Errorf("recent = %d, want 50", got)
	}
	e.Forget("s")
	if e.EpisodeCount() != 0 {
		t.Errorf("episodes after Forget = %d", e.EpisodeCount())
	}
}
