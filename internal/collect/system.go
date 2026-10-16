package collect

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Pressure is the kernel's memory pressure verdict.
type Pressure string

// Pressure levels. Unknown means the metric could not be read.
const (
	PressureNormal   Pressure = "normal"
	PressureWarn     Pressure = "warn"
	PressureCritical Pressure = "critical"
	PressureUnknown  Pressure = "unknown"
)

// Rank orders pressure levels; unknown ranks below normal.
func (p Pressure) Rank() int {
	switch p {
	case PressureNormal:
		return 1
	case PressureWarn:
		return 2
	case PressureCritical:
		return 3
	default:
		return 0
	}
}

// SystemSample holds system-wide memory metrics. Nil pointers mean the metric
// was unavailable; the name is then listed in Unavailable.
type SystemSample struct {
	TakenAt         time.Time `json:"taken_at"`
	TotalBytes      *uint64   `json:"total_bytes"`
	AvailableBytes  *uint64   `json:"available_bytes"` // approximation, see README
	CompressedBytes *uint64   `json:"compressed_bytes"`
	SwapUsedBytes   *uint64   `json:"swap_used_bytes"`
	SwapTotalBytes  *uint64   `json:"swap_total_bytes"`
	FreePercent     *int      `json:"free_percent"` // kern.memorystatus_level
	Pressure        Pressure  `json:"pressure"`
	Unavailable     []string  `json:"unavailable,omitempty"`
	Errors          []string  `json:"errors,omitempty"`
}

// VMStat is parsed vm_stat output.
type VMStat struct {
	PageSize uint64
	Pages    map[string]uint64
}

var pageSizeRE = regexp.MustCompile(`page size of (\d+) bytes`)

// ParseVMStat parses macOS vm_stat output.
func ParseVMStat(out []byte) (VMStat, error) {
	v := VMStat{Pages: map[string]uint64{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if m := pageSizeRE.FindStringSubmatch(line); m != nil {
			n, err := strconv.ParseUint(m[1], 10, 64)
			if err == nil && n > 0 {
				v.PageSize = n
			}
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.Trim(strings.TrimSpace(key), `"`)
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(val), "."), 10, 64)
		if err != nil || key == "" {
			continue
		}
		v.Pages[key] = n
	}
	if v.PageSize == 0 {
		return v, fmt.Errorf("vm_stat output has no page size")
	}
	if len(v.Pages) == 0 {
		return v, fmt.Errorf("vm_stat output has no page counts")
	}
	return v, nil
}

// Bytes returns the named page count converted to bytes.
func (v VMStat) Bytes(key string) (uint64, bool) {
	n, ok := v.Pages[key]
	return n * v.PageSize, ok
}

// ParseSysctl parses "name: value" lines.
func ParseSysctl(out []byte) map[string]string {
	kv := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "" && !strings.ContainsAny(key, " \t") {
			kv[key] = strings.TrimSpace(val)
		}
	}
	return kv
}

var swapRE = regexp.MustCompile(`total\s*=\s*([\d.]+)([KMGT])\s+used\s*=\s*([\d.]+)([KMGT])`)

// ParseSwapUsage parses vm.swapusage, e.g. "total = 1024.00M  used = 2.25M  free = 1021.75M  (encrypted)".
func ParseSwapUsage(s string) (total, used uint64, err error) {
	m := swapRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, fmt.Errorf("unrecognised vm.swapusage %q", s)
	}
	total, err = scaled(m[1], m[2])
	if err != nil {
		return 0, 0, err
	}
	used, err = scaled(m[3], m[4])
	if err != nil {
		return 0, 0, err
	}
	return total, used, nil
}

func scaled(num, unit string) (uint64, error) {
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("bad size %q", num)
	}
	mult := map[string]float64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}[unit]
	return uint64(f * mult), nil
}

// PressureFromLevel maps kern.memorystatus_vm_pressure_level (1, 2, 4).
func PressureFromLevel(s string) Pressure {
	switch strings.TrimSpace(s) {
	case "1":
		return PressureNormal
	case "2":
		return PressureWarn
	case "4":
		return PressureCritical
	default:
		return PressureUnknown
	}
}

// BuildSystemSample combines sysctl and vm_stat output. Either may be empty;
// missing metrics are reported as unavailable rather than zero.
func BuildSystemSample(now time.Time, sysctlOut, vmStatOut []byte) SystemSample {
	s := SystemSample{TakenAt: now, Pressure: PressureUnknown}
	kv := ParseSysctl(sysctlOut)
	if v, err := strconv.ParseUint(kv["hw.memsize"], 10, 64); err == nil && v > 0 {
		s.TotalBytes = &v
	}
	if raw, ok := kv["vm.swapusage"]; ok {
		if total, used, err := ParseSwapUsage(raw); err == nil {
			s.SwapTotalBytes, s.SwapUsedBytes = &total, &used
		} else {
			s.Errors = append(s.Errors, err.Error())
		}
	}
	s.Pressure = PressureFromLevel(kv["kern.memorystatus_vm_pressure_level"])
	if v, err := strconv.Atoi(kv["kern.memorystatus_level"]); err == nil && v >= 0 && v <= 100 {
		s.FreePercent = &v
	}
	if len(vmStatOut) > 0 {
		vm, err := ParseVMStat(vmStatOut)
		if err != nil {
			s.Errors = append(s.Errors, err.Error())
		} else {
			if c, ok := vm.Bytes("Pages occupied by compressor"); ok {
				s.CompressedBytes = &c
			}
			var avail uint64
			complete := true
			for _, k := range []string{"Pages free", "Pages inactive", "Pages speculative", "Pages purgeable"} {
				b, ok := vm.Bytes(k)
				if !ok {
					complete = false
					break
				}
				avail += b
			}
			if complete {
				s.AvailableBytes = &avail
			}
		}
	}
	if s.TotalBytes == nil {
		s.Unavailable = append(s.Unavailable, "total_ram")
	}
	if s.AvailableBytes == nil {
		s.Unavailable = append(s.Unavailable, "available_ram")
	}
	if s.CompressedBytes == nil {
		s.Unavailable = append(s.Unavailable, "compressed")
	}
	if s.SwapUsedBytes == nil {
		s.Unavailable = append(s.Unavailable, "swap")
	}
	if s.FreePercent == nil {
		s.Unavailable = append(s.Unavailable, "free_percent")
	}
	if s.Pressure == PressureUnknown {
		s.Unavailable = append(s.Unavailable, "pressure")
	}
	return s
}
