package procstat

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// currentRSS reads VmRSS from /proc/self/status, which the kernel reports
// in kB (KiB).
func currentRSS() *uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "VmRSS:")
		if !ok {
			continue
		}
		kib, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
		if err != nil {
			return nil
		}
		b := kib * 1024
		return &b
	}
	return nil
}
