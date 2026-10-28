//go:build unix

package procstat

import (
	"runtime"
	"syscall"
	"time"
)

func rusage() Stat {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return Stat{}
	}
	user := time.Duration(ru.Utime.Nano())
	system := time.Duration(ru.Stime.Nano())
	// ru_maxrss is bytes on Darwin and KiB everywhere else.
	peak := uint64(ru.Maxrss)
	if runtime.GOOS != "darwin" && runtime.GOOS != "ios" {
		peak *= 1024
	}
	return Stat{UserCPU: &user, SystemCPU: &system, PeakRSS: &peak}
}
