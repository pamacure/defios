package errlog_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"defios.com/internal/errlog"
)

// A loop that fails the same way on every wake logs it once, again if the
// error changes, and once when it clears (ADR 0014).
func TestReporterLogsAnErrorWhenItChanges(t *testing.T) {
	var lines []string
	r := errlog.New(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }, "serve: Workspaces")

	r.Report(nil)
	r.Report(errors.New("disk full"))
	r.Report(errors.New("disk full"))
	r.Report(errors.New("disk full"))
	r.Report(errors.New("permission denied"))
	r.Report(nil)
	r.Report(nil)
	r.Report(errors.New("disk full"))

	want := []string{
		"serve: Workspaces: disk full",
		"serve: Workspaces: permission denied",
		"serve: Workspaces: ok again",
		"serve: Workspaces: disk full",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("logged\n%q\nwant\n%q", lines, want)
	}
}
