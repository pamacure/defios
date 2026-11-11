//go:build windows

package flock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lock(f *os.File, exclusive, wait bool) error {
	var flags uint32
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	// Lock the whole file: every byte from offset 0.
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, ^uint32(0), ^uint32(0), new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrWouldBlock
	}
	return err
}
