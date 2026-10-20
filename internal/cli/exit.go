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

func usagef(format string, args ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, args...)}
}

func operational(err error) error { return &ExitError{Code: ExitOperational, Err: err} }

func operationalf(format string, args ...any) error {
	return operational(fmt.Errorf(format, args...))
}

func verify(err error) error { return &ExitError{Code: ExitVerify, Err: err} }
