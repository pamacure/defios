package procstat_test

import (
	"runtime"
	"testing"

	"defios.com/internal/procstat"
)

// On Unix the OS reports CPU time and peak RSS; on Linux current RSS too,
// and it never exceeds the peak. A field a platform lacks stays nil.
func TestSelfReportsWhatThePlatformHas(t *testing.T) {
	burn := 0
	for i := range 10_000_000 {
		burn += i
	}
	_ = burn
	s := procstat.Self()

	switch runtime.GOOS {
	case "windows", "plan9", "js", "wasip1":
		if s.UserCPU != nil || s.PeakRSS != nil || s.RSS != nil {
			t.Fatalf("Self = %+v on %s, want nothing", s, runtime.GOOS)
		}
		return
	}
	if s.UserCPU == nil || s.SystemCPU == nil || *s.UserCPU <= 0 {
		t.Fatalf("CPU time = %v / %v, want user > 0", s.UserCPU, s.SystemCPU)
	}
	if s.PeakRSS == nil || *s.PeakRSS < 1<<20 {
		t.Fatalf("peak RSS = %v, want at least 1 MiB", s.PeakRSS)
	}
	if runtime.GOOS != "linux" {
		return
	}
	if s.RSS == nil || *s.RSS == 0 || *s.RSS > *s.PeakRSS {
		t.Fatalf("RSS = %v, peak %d; want 0 < RSS <= peak", s.RSS, *s.PeakRSS)
	}
}
