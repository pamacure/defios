//go:build !unix

package procstat

func rusage() Stat { return Stat{} }
