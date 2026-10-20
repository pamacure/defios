package cli

import "fmt"

// Exit codes (docs/spec/mvp.md §9).
const (
	ExitOK          = 0
	ExitUsage       = 1 // bad flags, bad arguments, missing passphrase source
	ExitOperational = 2 // no serve, wrong dir, Node missing or already present
	ExitProtocol    = 3 // protocol reject
	ExitVerify      = 4 // signature or passphrase verification failed
)

// ExitError carries the process exit code alongside the cause.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// exitUsagef tags err with ExitUsage.
func exitUsagef(format string, args ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, args...)}
}

// exitOperational tags err with ExitOperational.
func exitOperational(err error) error { return &ExitError{Code: ExitOperational, Err: err} }

// exitOperationalf is exitOperational over a formatted error.
func exitOperationalf(format string, args ...any) error {
	return exitOperational(fmt.Errorf(format, args...))
}

// exitVerify tags err with ExitVerify.
func exitVerify(err error) error { return &ExitError{Code: ExitVerify, Err: err} }
