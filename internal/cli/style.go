package cli

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// SGR codes for the output roles (docs/style.md, #60). Basic codes only, so
// the terminal theme picks the actual shades.
const (
	sgrPrimary = "34" // blue: the identity of a record
	sgrCaution = "33" // yellow: in progress, may need someone
	sgrConcern = "31" // red: failed or terminal-bad
	sgrDim     = "2"  // faint: annotation
)

// palette colors human output by role. The zero value is plain, so every
// helper is safe to call whether or not color is on.
type palette struct{ on bool }

// paletteFor is on only when w is a terminal, NO_COLOR is unset and TERM is
// not dumb.
func paletteFor(w io.Writer, getenv func(string) string) palette {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return palette{}
	}
	if getenv != nil && (getenv("NO_COLOR") != "" || getenv("TERM") == "dumb") {
		return palette{}
	}
	return palette{on: true}
}

// out is the palette for human output on stdout; plain under --json.
func (g *globals) out() palette {
	if g.json {
		return palette{}
	}
	return paletteFor(g.app.Stdout, g.app.Getenv)
}

func (p palette) wrap(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p palette) primary(s string) string { return p.wrap(sgrPrimary, s) }
func (p palette) caution(s string) string { return p.wrap(sgrCaution, s) }
func (p palette) concern(s string) string { return p.wrap(sgrConcern, s) }
func (p palette) dim(s string) string     { return p.wrap(sgrDim, s) }

// status colors a status word or COSL type name by what it asks of the
// reader: caution while something is in progress, concern once it has gone
// wrong, plain when it is healthy.
func (p palette) status(s string) string {
	switch {
	case s == "open", s == "waiting", s == "signing", s == "pending", s == "unformed", s == "stale", s == "down":
		return p.caution(s)
	case s == "aborted", s == "declined", s == "expired", s == "error", s == "session.abort", strings.HasPrefix(s, "unknown("):
		return p.concern(s)
	}
	return s
}

// yesNo colors a yes/no where no is the state that wants attention.
func (p palette) yesNo(b bool) string {
	if b {
		return "yes"
	}
	return p.caution("no")
}
