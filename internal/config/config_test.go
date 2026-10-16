package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreValid(t *testing.T) {
	c := Default()
	if w := Validate(&c); len(w) != 0 {
		t.Errorf("defaults invalid: %v", w)
	}
	if c.RawCapacity() != 720 || c.MinuteCapacity() != 1440 {
		t.Errorf("capacities %d %d", c.RawCapacity(), c.MinuteCapacity())
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		warnings int
		check    func(Config) bool
	}{
		{"partial keeps defaults", `{"warn_rss_mb": 1024, "verbosity": "quiet"}`, 0,
			func(c Config) bool { return c.WarnRSSMB == 1024 && c.Verbosity == Quiet && c.CriticalRSSMB == 4096 }},
		{"unknown key rejected", `{"warn_rss": 1}`, 1, func(c Config) bool { return c == Default() }},
		{"syntax error", `{"warn_rss_mb": }`, 1, func(c Config) bool { return c == Default() }},
		{"out of range reset", `{"sample_interval_sec": 0, "min_up_fraction": 2}`, 2,
			func(c Config) bool { return c.SampleIntervalSec == 5 && c.MinUpFraction == 0.6 }},
		{"critical must exceed warn", `{"warn_rss_mb": 5000, "critical_rss_mb": 4000}`, 1,
			func(c Config) bool { return c.WarnRSSMB == 2048 && c.CriticalRSSMB == 4096 }},
		{"leak window must exceed growth window", `{"growth_window_minutes": 60, "leak_window_minutes": 30}`, 1,
			func(c Config) bool { return c.GrowthWindowMinutes == 20 && c.LeakWindowMinutes == 60 }},
		{"leak window must fit retention", `{"leak_window_minutes": 600, "retention_hours": 2}`, 1,
			func(c Config) bool { return c.LeakWindowMinutes == 60 && c.RetentionHours == 24 }},
		{"bad verbosity", `{"verbosity": "loud"}`, 1, func(c Config) bool { return c.Verbosity == Normal }},
		{"growth window too short to ever fill", `{"growth_window_minutes": 5}`, 1,
			func(c Config) bool { return c.GrowthWindowMinutes == 20 }},
		{"elevated minimum must fit raw history", `{"elevated_min_minutes": 90, "raw_retention_minutes": 60}`, 1,
			func(c Config) bool { return c.ElevatedMinMinutes == 5 && c.RawRetentionMinutes == 60 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := Parse([]byte(tt.json))
			if len(w) != tt.warnings || !tt.check(c) {
				t.Errorf("warnings %v config %+v", w, c)
			}
			if again := Validate(&c); len(again) != 0 {
				t.Errorf("result still invalid: %v", again)
			}
		})
	}
}

func TestLoadMissingAndUnreadable(t *testing.T) {
	dir := t.TempDir()
	if c, w := Load(filepath.Join(dir, "missing.json")); len(w) != 0 || c != Default() {
		t.Errorf("missing file: %v", w)
	}
	if c, w := Load(dir); len(w) != 1 || !strings.Contains(w[0], "cannot read") || c != Default() {
		t.Errorf("directory as file: %v", w)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"verbosity":"verbose"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, w := Load(path); len(w) != 0 || c.Verbosity != Verbose {
		t.Errorf("valid file: %v %v", c.Verbosity, w)
	}
}
