// Package errlog logs a long-running loop's errors when they change, not on
// every wake (ADR 0014).
package errlog

// Reporter remembers the last error one loop reported. Not safe for
// concurrent use; give each loop its own.
type Reporter struct {
	logf   func(format string, args ...any)
	prefix string
	last   string // "" while the loop is healthy
}

// New reports through logf, each line starting with prefix.
func New(logf func(format string, args ...any), prefix string) *Reporter {
	return &Reporter{logf: logf, prefix: prefix}
}

// Report is the outcome of one wake: an error is logged when it first
// appears and whenever its text changes, and a nil after an error logs once
// that the loop is ok again. Repeats log nothing.
func (r *Reporter) Report(err error) {
	switch {
	case err == nil && r.last != "":
		r.last = ""
		r.logf("%s: ok again", r.prefix)
	case err != nil && err.Error() != r.last:
		r.last = err.Error()
		r.logf("%s: %v", r.prefix, err)
	}
}
