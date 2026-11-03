// Package recap is a Node's Recap of a Conversation
// (docs/adr/0024-a-recap-is-a-local-file.md); package talk reads the talk
// under it. A Recap
// is the file RECAP.md in the Conversation's data directory: this Node's own
// account of the talk up to a seq, never on the COSL and replaced whole each
// time it is written.
package recap

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"defios.com/internal/cosl"
)

// ErrBadBase is a Recap whose base seq names no talk of its Conversation.
var ErrBadBase = errors.New("recap: base seq must name an app.freeform of this Conversation")

// Recap is one RECAP.md.
type Recap struct {
	BaseSeq uint64 // the highest seq it accounts for
	Text    string
}

// Covers reports whether r, this Node's Recap or nil for none, accounts for
// the talk up to seq: Pruning past it leaves this Node's agents without talk
// that nothing stands in for.
func (r *Recap) Covers(seq uint64) bool { return r != nil && seq <= r.BaseSeq }

// fence opens and closes RECAP.md's front matter.
const fence = "---"

// Read is the Recap at path, nil if there is none. A RECAP.md that does not
// parse is an error naming it: a Recap is never silently ignored.
func Read(path string) (*Recap, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recap: %w", err)
	}
	r, err := parse(b)
	if err != nil {
		return nil, fmt.Errorf("recap: %s: %w", path, err)
	}
	return &r, nil
}

// parse reads RECAP.md: a front matter fenced by "---" lines giving
// base_seq and nothing else, then the text. Line ends may be CRLF, as an
// editor may leave them; blank lines in the front matter are skipped, and
// those around the text are not part of it.
func parse(b []byte) (Recap, error) {
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if len(lines) == 0 || lines[0] != fence {
		return Recap{}, errors.New(`it must open with a front matter, a "---" line`)
	}
	end := -1
	for i, l := range lines[1:] {
		if l == fence {
			end = i + 1
			break
		}
	}
	if end < 0 {
		return Recap{}, errors.New(`its front matter is not closed by a "---" line`)
	}
	var r Recap
	for _, l := range lines[1:end] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		k, v, _ := strings.Cut(l, ":")
		if k = strings.TrimSpace(k); k != "base_seq" {
			return Recap{}, fmt.Errorf("its front matter may give only base_seq, not %q", k)
		}
		if r.BaseSeq != 0 {
			return Recap{}, errors.New("its front matter gives base_seq twice")
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil || n == 0 {
			return Recap{}, fmt.Errorf("base_seq %q must be a seq, a whole number above 0", strings.TrimSpace(v))
		}
		r.BaseSeq = n
	}
	if r.BaseSeq == 0 {
		return Recap{}, errors.New("its front matter must give base_seq")
	}
	r.Text = strings.Trim(strings.Join(lines[end+1:], "\n"), "\n")
	if err := cosl.CheckText(r.Text); err != nil {
		return Recap{}, err
	}
	return r, nil
}

// format is r as RECAP.md.
func (r Recap) format() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\nbase_seq: %d\n%s\n%s\n", fence, r.BaseSeq, fence, r.Text)
	return b.Bytes()
}

// Write replaces the Recap at path with r, whole: it writes a temporary file
// beside it, syncs it and renames it over RECAP.md, so a crash leaves the old
// Recap or the new one, never part of either. It makes the directory if need
// be. Its text is bounded as app.freeform's is.
func Write(path string, r Recap) error {
	if r.BaseSeq == 0 {
		return fmt.Errorf("%w: seq 0 is no message", ErrBadBase)
	}
	if err := cosl.CheckText(r.Text); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("recap: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".RECAP.md.*")
	if err != nil {
		return fmt.Errorf("recap: %w", err)
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	_, err = tmp.Write(r.format())
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("recap: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync() // best effort: the rename is durable once the directory is
		d.Close()
	}
	return nil
}
