// Package config loads and validates the plugin's user configuration.
//
// The file is JSON (config.json in HERDR_PLUGIN_CONFIG_DIR). Missing keys keep
// their defaults, unknown keys are rejected, and every out-of-range value
// falls back to its default with a warning surfaced in the UI and logs.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// FileName is the config file name inside HERDR_PLUGIN_CONFIG_DIR.
const FileName = "config.json"

// Verbosity controls which alerts become Herdr notifications.
type Verbosity string

// Verbosity levels, from fewest to most notifications.
const (
	Quiet   Verbosity = "quiet"
	Normal  Verbosity = "normal"
	Verbose Verbosity = "verbose"
)

// Rank orders verbosity levels so callers can compare them.
func (v Verbosity) Rank() int {
	switch v {
	case Quiet:
		return 0
	case Verbose:
		return 2
	default:
		return 1
	}
}

// Config holds every tunable. Field names mirror the JSON keys.
type Config struct {
	SampleIntervalSec       int       `json:"sample_interval_sec"`
	WarnRSSMB               int       `json:"warn_rss_mb"`
	CriticalRSSMB           int       `json:"critical_rss_mb"`
	ElevatedMinMinutes      float64   `json:"elevated_min_minutes"`
	GrowthWindowMinutes     int       `json:"growth_window_minutes"`
	LeakWindowMinutes       int       `json:"leak_window_minutes"`
	MinGrowthMB             int       `json:"min_growth_mb"`
	MinGrowthPercent        float64   `json:"min_growth_percent"`
	MinUpFraction           float64   `json:"min_up_fraction"`
	IdleAfterMinutes        float64   `json:"idle_after_minutes"`
	IdleCPUPercent          float64   `json:"idle_cpu_percent"`
	RecoveryMinutes         float64   `json:"recovery_minutes"`
	AlertCooldownMinutes    float64   `json:"alert_cooldown_minutes"`
	RetentionHours          float64   `json:"retention_hours"`
	RawRetentionMinutes     int       `json:"raw_retention_minutes"`
	Verbosity               Verbosity `json:"verbosity"`
	Notifications           bool      `json:"notifications"`
	SidebarToken            bool      `json:"sidebar_token"`
	SidebarShowRSS          bool      `json:"sidebar_show_rss"`
	SystemPressureWarnings  bool      `json:"system_pressure_warnings"`
	PressureSustainMinutes  float64   `json:"pressure_sustain_minutes"`
	SwapGrowthMB            int       `json:"swap_growth_mb"`
	SwapGrowthWindowMinutes int       `json:"swap_growth_window_minutes"`
}

// Default returns the safe defaults.
func Default() Config {
	return Config{
		SampleIntervalSec:       5,
		WarnRSSMB:               2048,
		CriticalRSSMB:           4096,
		ElevatedMinMinutes:      5,
		GrowthWindowMinutes:     20,
		LeakWindowMinutes:       60,
		MinGrowthMB:             150,
		MinGrowthPercent:        15,
		MinUpFraction:           0.6,
		IdleAfterMinutes:        5,
		IdleCPUPercent:          3,
		RecoveryMinutes:         10,
		AlertCooldownMinutes:    30,
		RetentionHours:          24,
		RawRetentionMinutes:     60,
		Verbosity:               Normal,
		Notifications:           true,
		SidebarToken:            true,
		SystemPressureWarnings:  true,
		PressureSustainMinutes:  2,
		SwapGrowthMB:            1024,
		SwapGrowthWindowMinutes: 10,
	}
}

// Load reads path. A missing file yields defaults without warnings; an
// unreadable or unparseable file yields defaults with a warning.
func Load(path string) (Config, []string) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Default(), []string{fmt.Sprintf("cannot read %s: %v; using defaults", path, err)}
	}
	return Parse(data)
}

// Parse decodes data over the defaults and validates the result.
func Parse(data []byte) (Config, []string) {
	cfg := Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Default(), []string{fmt.Sprintf("invalid config: %v; using defaults", err)}
	}
	return cfg, Validate(&cfg)
}

// Validate resets each invalid field to its default and describes what changed.
func Validate(c *Config) []string {
	d := Default()
	var warnings []string
	check := func(bad bool, name, want string, reset func()) {
		if bad {
			warnings = append(warnings, fmt.Sprintf("%s %s; using default", name, want))
			reset()
		}
	}
	check(c.SampleIntervalSec < 2 || c.SampleIntervalSec > 60, "sample_interval_sec", "must be 2-60",
		func() { c.SampleIntervalSec = d.SampleIntervalSec })
	check(c.WarnRSSMB < 64 || c.WarnRSSMB > 1<<20, "warn_rss_mb", "must be 64-1048576",
		func() { c.WarnRSSMB = d.WarnRSSMB })
	check(c.CriticalRSSMB < 64 || c.CriticalRSSMB > 1<<20, "critical_rss_mb", "must be 64-1048576",
		func() { c.CriticalRSSMB = d.CriticalRSSMB })
	if c.CriticalRSSMB <= c.WarnRSSMB {
		warnings = append(warnings, "critical_rss_mb must exceed warn_rss_mb; using defaults for both")
		c.WarnRSSMB, c.CriticalRSSMB = d.WarnRSSMB, d.CriticalRSSMB
	}
	check(c.ElevatedMinMinutes < 0 || c.ElevatedMinMinutes > 240, "elevated_min_minutes", "must be 0-240",
		func() { c.ElevatedMinMinutes = d.ElevatedMinMinutes })
	// Windows below 10 minutes cannot gather the 5 closed one-minute buckets
	// and 70% span the growth test requires.
	check(c.GrowthWindowMinutes < 10 || c.GrowthWindowMinutes > 720, "growth_window_minutes", "must be 10-720",
		func() { c.GrowthWindowMinutes = d.GrowthWindowMinutes })
	check(c.LeakWindowMinutes < 10 || c.LeakWindowMinutes > 1440, "leak_window_minutes", "must be 10-1440",
		func() { c.LeakWindowMinutes = d.LeakWindowMinutes })
	if c.LeakWindowMinutes <= c.GrowthWindowMinutes {
		warnings = append(warnings, "leak_window_minutes must exceed growth_window_minutes; using defaults for both")
		c.GrowthWindowMinutes, c.LeakWindowMinutes = d.GrowthWindowMinutes, d.LeakWindowMinutes
	}
	check(c.MinGrowthMB < 10 || c.MinGrowthMB > 1<<20, "min_growth_mb", "must be 10-1048576",
		func() { c.MinGrowthMB = d.MinGrowthMB })
	check(c.MinGrowthPercent < 1 || c.MinGrowthPercent > 1000, "min_growth_percent", "must be 1-1000",
		func() { c.MinGrowthPercent = d.MinGrowthPercent })
	check(c.MinUpFraction < 0.5 || c.MinUpFraction > 1, "min_up_fraction", "must be 0.5-1",
		func() { c.MinUpFraction = d.MinUpFraction })
	check(c.IdleAfterMinutes < 1 || c.IdleAfterMinutes > 240, "idle_after_minutes", "must be 1-240",
		func() { c.IdleAfterMinutes = d.IdleAfterMinutes })
	check(c.IdleCPUPercent < 0 || c.IdleCPUPercent > 100, "idle_cpu_percent", "must be 0-100",
		func() { c.IdleCPUPercent = d.IdleCPUPercent })
	check(c.RecoveryMinutes < 1 || c.RecoveryMinutes > 240, "recovery_minutes", "must be 1-240",
		func() { c.RecoveryMinutes = d.RecoveryMinutes })
	check(c.AlertCooldownMinutes < 1 || c.AlertCooldownMinutes > 1440, "alert_cooldown_minutes", "must be 1-1440",
		func() { c.AlertCooldownMinutes = d.AlertCooldownMinutes })
	check(c.RetentionHours < 1 || c.RetentionHours > 168, "retention_hours", "must be 1-168",
		func() { c.RetentionHours = d.RetentionHours })
	if float64(c.LeakWindowMinutes) > c.RetentionHours*60 {
		warnings = append(warnings, "leak_window_minutes exceeds retention_hours; using defaults for windows and retention")
		c.GrowthWindowMinutes, c.LeakWindowMinutes, c.RetentionHours =
			d.GrowthWindowMinutes, d.LeakWindowMinutes, d.RetentionHours
	}
	check(c.RawRetentionMinutes < 5 || c.RawRetentionMinutes > 240, "raw_retention_minutes", "must be 5-240",
		func() { c.RawRetentionMinutes = d.RawRetentionMinutes })
	// Time above a threshold is measured on raw samples, so the raw history
	// must outlast the minimum elevated duration.
	if c.ElevatedMinMinutes >= float64(c.RawRetentionMinutes) {
		warnings = append(warnings, "elevated_min_minutes must be below raw_retention_minutes; using defaults for both")
		c.ElevatedMinMinutes, c.RawRetentionMinutes = d.ElevatedMinMinutes, d.RawRetentionMinutes
	}
	check(c.Verbosity != Quiet && c.Verbosity != Normal && c.Verbosity != Verbose, "verbosity",
		`must be "quiet", "normal" or "verbose"`, func() { c.Verbosity = d.Verbosity })
	check(c.PressureSustainMinutes < 0 || c.PressureSustainMinutes > 120, "pressure_sustain_minutes", "must be 0-120",
		func() { c.PressureSustainMinutes = d.PressureSustainMinutes })
	check(c.SwapGrowthMB < 64 || c.SwapGrowthMB > 1<<20, "swap_growth_mb", "must be 64-1048576",
		func() { c.SwapGrowthMB = d.SwapGrowthMB })
	check(c.SwapGrowthWindowMinutes < 1 || c.SwapGrowthWindowMinutes > 240, "swap_growth_window_minutes", "must be 1-240",
		func() { c.SwapGrowthWindowMinutes = d.SwapGrowthWindowMinutes })
	return warnings
}

// Interval is the sampling interval.
func (c Config) Interval() time.Duration { return time.Duration(c.SampleIntervalSec) * time.Second }

// Minutes converts a fractional minute setting to a duration.
func Minutes(m float64) time.Duration { return time.Duration(m * float64(time.Minute)) }

// RawCapacity is the number of raw samples kept per session run.
func (c Config) RawCapacity() int {
	return c.RawRetentionMinutes * 60 / c.SampleIntervalSec
}

// MinuteCapacity is the number of one-minute buckets kept per session run.
func (c Config) MinuteCapacity() int { return int(c.RetentionHours * 60) }
