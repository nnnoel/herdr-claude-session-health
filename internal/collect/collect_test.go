package collect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func TestParsePSHandlesMalformedOutput(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	table, err := ParsePS(readFixture(t, "ps_malformed.txt"), now)
	if err != nil {
		t.Fatalf("ParsePS: %v", err)
	}
	for _, pid := range []int{1, 100, 101, 102, 103} {
		if _, ok := table.Procs[pid]; !ok {
			t.Errorf("pid %d missing", pid)
		}
	}
	if got := len(table.Procs); got != 5 {
		t.Errorf("parsed %d processes, want 5", got)
	}
	if table.Skipped != 8 {
		t.Errorf("skipped %d lines, want 8", table.Skipped)
	}
	if p := table.Procs[102]; p.Name != "Google" || p.RSSKB != 2048 {
		t.Errorf("pid 102 = %+v (only the first name token is kept)", p)
	}
	if p := table.Procs[101]; p.RSSKB != 50000 {
		t.Errorf("duplicate pid line must not overwrite the first: %+v", p)
	}
	if p := table.Procs[100]; p.CPUTime != 26*time.Second+270*time.Millisecond || p.Elapsed != 18*time.Minute+59*time.Second {
		t.Errorf("pid 100 times = %v / %v", p.CPUTime, p.Elapsed)
	}
	if got := table.Children(100); len(got) != 2 || got[0] != 101 || got[1] != 102 {
		t.Errorf("children of 100 = %v", got)
	}
	start, ok := table.StartTime(100)
	if !ok || !start.Equal(now.Add(-(18*time.Minute + 59*time.Second))) {
		t.Errorf("start = %v", start)
	}
}

// Processes that retitle themselves can put their arguments, including
// credentials, into ps name fields. Only the first token may survive.
func TestProcNameDropsArguments(t *testing.T) {
	table, err := ParsePS([]byte("200 1 200 10 0.0 0:00.01 01:00 mcp --header Authorization:Bearer phx_SECRET\n"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := table.Procs[200].Name; got != "mcp" {
		t.Errorf("name = %q, want mcp", got)
	}
	for in, want := range map[string]string{
		"/usr/bin/node /srv/app.js --token=abc": "node",
		"2.1.301":                               "2.1.301",
		"  ":                                    "",
		"evil\x1b[31m":                          "evil[31m",
		strings.Repeat("a", 40):                 strings.Repeat("a", 32),
	} {
		if got := ProcName(in); got != want {
			t.Errorf("ProcName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePSRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "\n\n", "garbage line\n", "\x00\x01\x02", "PID PPID PGID RSS %CPU TIME ELAPSED COMMAND\n"} {
		if _, err := ParsePS([]byte(in), time.Now()); err == nil {
			t.Errorf("ParsePS(%q) succeeded, want error", in)
		}
	}
}

func TestParseClock(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "00:05", want: 5 * time.Second},
		{in: "0:26.27", want: 26*time.Second + 270*time.Millisecond},
		{in: "403:43.33", want: 403*time.Minute + 43*time.Second + 330*time.Millisecond},
		{in: "22:50:58", want: 22*time.Hour + 50*time.Minute + 58*time.Second},
		{in: "2-03:04:05", want: 51*time.Hour + 4*time.Minute + 5*time.Second},
		{in: "", bad: true},
		{in: "12", bad: true},
		{in: "1:2:3:4", bad: true},
		{in: "x:10", bad: true},
		{in: "-1:10", bad: true},
		{in: "1:75", bad: true},
		{in: "a-01:02:03", bad: true},
		{in: "1:NaN", bad: true},
	}
	for _, tt := range tests {
		got, err := ParseClock(tt.in)
		if tt.bad {
			if err == nil {
				t.Errorf("ParseClock(%q) = %v, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseClock(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
}

func TestIsAncestorToleratesCycles(t *testing.T) {
	table, err := ParsePS([]byte("10 11 10 1 0.0 0:00.00 00:01 a\n11 10 10 1 0.0 0:00.00 00:01 b\n12 10 10 1 0.0 0:00.00 00:01 c\n"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !table.IsAncestor(10, 12) {
		t.Error("10 should be an ancestor of 12")
	}
	if table.IsAncestor(99, 12) {
		t.Error("99 is not an ancestor of 12 (cycle must terminate)")
	}
}

func TestParseVMStat(t *testing.T) {
	v, err := ParseVMStat(readFixture(t, "vm_stat.txt"))
	if err != nil {
		t.Fatalf("ParseVMStat: %v", err)
	}
	if v.PageSize != 16384 {
		t.Errorf("page size = %d", v.PageSize)
	}
	if b, ok := v.Bytes("Pages occupied by compressor"); !ok || b != 198435*16384 {
		t.Errorf("compressor bytes = %d %v", b, ok)
	}
	if _, ok := v.Bytes("Translation faults"); !ok {
		t.Error("quoted keys should parse")
	}
	for _, bad := range []string{"", "Pages free: 10.\n", "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nnonsense\n"} {
		if _, err := ParseVMStat([]byte(bad)); err == nil {
			t.Errorf("ParseVMStat(%q) succeeded, want error", bad)
		}
	}
}

func TestParseSwapUsage(t *testing.T) {
	total, used, err := ParseSwapUsage("total = 1024.00M  used = 2.25M  free = 1021.75M  (encrypted)")
	if err != nil || total != 1024<<20 || used != uint64(2.25*(1<<20)) {
		t.Errorf("got %d %d %v", total, used, err)
	}
	if _, _, err := ParseSwapUsage("total = 2.00G  used = 1.50G  free = 0.50G"); err != nil {
		t.Errorf("GB units: %v", err)
	}
	for _, bad := range []string{"", "total = lots", "used = 1M total = 2M"} {
		if _, _, err := ParseSwapUsage(bad); err == nil {
			t.Errorf("ParseSwapUsage(%q) succeeded", bad)
		}
	}
}

func TestBuildSystemSampleReportsUnavailableNotZero(t *testing.T) {
	now := time.Now()
	full := BuildSystemSample(now, readFixture(t, "sysctl.txt"), readFixture(t, "vm_stat.txt"))
	if full.TotalBytes == nil || *full.TotalBytes != 34359738368 {
		t.Errorf("total = %v", full.TotalBytes)
	}
	if full.Pressure != PressureNormal || full.FreePercent == nil || *full.FreePercent != 82 {
		t.Errorf("pressure %s free %v", full.Pressure, full.FreePercent)
	}
	if full.AvailableBytes == nil || full.CompressedBytes == nil || full.SwapUsedBytes == nil {
		t.Errorf("missing metrics: %+v", full)
	}
	if len(full.Unavailable) != 0 {
		t.Errorf("unexpected unavailable: %v", full.Unavailable)
	}

	empty := BuildSystemSample(now, []byte("garbage\nkern.memorystatus_vm_pressure_level: 9\n"), []byte("junk"))
	if empty.TotalBytes != nil || empty.AvailableBytes != nil || empty.SwapUsedBytes != nil || empty.FreePercent != nil {
		t.Errorf("malformed input produced values: %+v", empty)
	}
	if empty.Pressure != PressureUnknown {
		t.Errorf("pressure = %s, want unknown", empty.Pressure)
	}
	want := []string{"total_ram", "available_ram", "compressed", "swap", "free_percent", "pressure"}
	if strings.Join(empty.Unavailable, ",") != strings.Join(want, ",") {
		t.Errorf("unavailable = %v, want %v", empty.Unavailable, want)
	}
	if len(empty.Errors) == 0 {
		t.Error("vm_stat parse error should be reported")
	}
}

func TestPressureFromLevel(t *testing.T) {
	for in, want := range map[string]Pressure{"1": PressureNormal, "2": PressureWarn, "4": PressureCritical, "": PressureUnknown, "3": PressureUnknown} {
		if got := PressureFromLevel(in); got != want {
			t.Errorf("PressureFromLevel(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestExecRunnerTimeoutAndOutputLimit(t *testing.T) {
	// Only test-owned child processes are started here.
	r := ExecRunner{Timeout: 200 * time.Millisecond}
	start := time.Now()
	if _, err := r.Run(context.Background(), "sleep", "5"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("want timeout error, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("timeout took %s", time.Since(start))
	}
	limited := ExecRunner{Timeout: 5 * time.Second, MaxOutput: 1000}
	out, err := limited.Run(context.Background(), "head", "-c", "100000", "/dev/zero")
	if err == nil || len(out) != 1000 {
		t.Errorf("want truncation error with 1000 bytes, got %d bytes, %v", len(out), err)
	}
}
