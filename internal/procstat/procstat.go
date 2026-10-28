// Package procstat reads what the OS knows about this process: CPU time and
// resident memory. A platform that cannot say leaves the field nil, so a
// caller shows nothing rather than a zero.
package procstat

import "time"

// Stat is one reading of this process.
type Stat struct {
	UserCPU   *time.Duration // Unix
	SystemCPU *time.Duration // Unix
	PeakRSS   *uint64        // bytes; Unix
	RSS       *uint64        // bytes; Linux
}

// Self reads this process.
func Self() Stat {
	s := rusage()
	s.RSS = currentRSS()
	return s
}
