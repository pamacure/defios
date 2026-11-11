// Package flock takes advisory locks on open files the same way on every
// OS the Node runs on: flock(2) on unix, LockFileEx on Windows. A lock is
// released when its file is closed.
package flock

import (
	"errors"
	"os"
)

// ErrWouldBlock: the lock is held the other way and the caller asked not
// to wait.
var ErrWouldBlock = errors.New("flock: lock is held")

// Lock locks f, exclusive or shared. With wait it blocks until the lock is
// free; without, a held lock is ErrWouldBlock.
func Lock(f *os.File, exclusive, wait bool) error {
	return lock(f, exclusive, wait)
}
