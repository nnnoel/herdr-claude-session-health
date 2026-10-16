//go:build darwin

package collect

import (
	"context"
	"time"
)

// SystemSupported reports whether system memory collection is implemented here.
const SystemSupported = true

// System collects macOS memory metrics with sysctl and vm_stat (no root needed).
type System struct {
	Runner Runner
	Now    func() time.Time
}

// Sample runs one sysctl and one vm_stat. Partial failures yield a partial sample.
func (s System) Sample(ctx context.Context) SystemSample {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	sysctlOut, sysErr := s.Runner.Run(ctx, "sysctl",
		"hw.memsize", "vm.swapusage", "kern.memorystatus_vm_pressure_level", "kern.memorystatus_level")
	vmOut, vmErr := s.Runner.Run(ctx, "vm_stat")
	sample := BuildSystemSample(now(), sysctlOut, vmOut)
	// sysctl exits non-zero when any one key is missing but still prints the rest.
	if sysErr != nil {
		sample.Errors = append(sample.Errors, sysErr.Error())
	}
	if vmErr != nil {
		sample.Errors = append(sample.Errors, vmErr.Error())
	}
	return sample
}
