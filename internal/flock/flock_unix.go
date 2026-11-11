//go:build unix

package flock

import (
	"errors"
	"os"
	"syscall"
)

func lock(f *os.File, exclusive, wait bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if !wait {
		how |= syscall.LOCK_NB
	}
	err := syscall.Flock(int(f.Fd()), how)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrWouldBlock
	}
	return err
}
