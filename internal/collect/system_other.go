//go:build !darwin

package collect

import (
	"context"
	"time"
)

// SystemSupported reports whether system memory collection is implemented here.
// Linux support (/proc/meminfo, /proc/pressure/memory) belongs in a
// system_linux.go file that replaces this stub.
const SystemSupported = false

// System is a placeholder on platforms without an implementation.
type System struct {
	Runner Runner
	Now    func() time.Time
}

// Sample reports every metric as unavailable.
func (s System) Sample(context.Context) SystemSample {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	sample := BuildSystemSample(now(), nil, nil)
	sample.Errors = append(sample.Errors, "system memory metrics are not implemented on this platform")
	return sample
}
