//go:build !linux

package procstat

func currentRSS() *uint64 { return nil }
