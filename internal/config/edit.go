package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Tables of a Group's config.toml SetEntry edits: its Jobs and the
// Triggers that run them.
const (
	TableJobs      = "jobs"
	TableHooks     = "hooks"
	TableSchedules = "schedules"
)

// ErrNotInPlace is a SetEntry refused because config.toml defines the
// table in a form it cannot change in place, such as an inline table:
// the file is to be edited as text.
var ErrNotInPlace = errors.New("defined in a form that cannot be changed in place; edit config.toml as text")

// CheckFile parses and validates data as path's Group config.toml, as
// ReadFile would, writing nothing.
func CheckFile(path string, data []byte) error {
	_, err := parseConfig(path, data)
	return err
}

// SetEntry sets [<table>.<name>] in the Group config.toml at path to v,
// a Job, Hook or Schedule as table says, or removes it if v is nil; an
// absent file is empty. Only that table's lines change, so the rest of
// the file, comments included, stays as it was. A file that would not
// read back is not written: the error wraps ErrNotWritten.
func SetEntry(path, table, name string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config: %w", err)
	}
	var want Config
	if len(data) > 0 {
		if want, err = parseConfig(path, data); err != nil {
			return err
		}
	}
	if err := setIn(&want, table, name, v); err != nil {
		return err
	}
	encoded, err := canonical(want)
	if err != nil {
		return err
	}
	// Compared as the file reads back, so what parsing normalises is no
	// difference.
	want, err = parseConfig(path, []byte(encoded))
	if err != nil {
		return fmt.Errorf("%w; %w", err, ErrNotWritten)
	}
	wantText, err := canonical(want)
	if err != nil {
		return err
	}
	var block string
	if v != nil {
		if block, err = encodeTable(table, name, v); err != nil {
			return err
		}
	}
	out := spliceTable(string(data), table, name, block)
	got, err := parseConfig(path, []byte(out))
	if err != nil {
		return fmt.Errorf("config: %s: [%s.%s] %w", path, table, name, ErrNotInPlace)
	}
	if gotText, err := canonical(got); err != nil || gotText != wantText {
		return fmt.Errorf("config: %s: [%s.%s] %w", path, table, name, ErrNotInPlace)
	}
	return os.WriteFile(path, []byte(out), 0o600)
}

// setIn sets or, for a nil v, deletes c's table entry name.
func setIn(c *Config, table, name string, v any) error {
	switch table {
	case TableJobs:
		return setEntry(&c.Jobs, name, v)
	case TableHooks:
		return setEntry(&c.Hooks, name, v)
	case TableSchedules:
		return setEntry(&c.Schedules, name, v)
	}
	return fmt.Errorf("config: no trigger table %q", table)
}

func setEntry[T any](m *map[string]T, name string, v any) error {
	if v == nil {
		delete(*m, name)
		return nil
	}
	x, ok := v.(T)
	if !ok {
		var zero T
		return fmt.Errorf("config: a %T is no %T", v, zero)
	}
	if *m == nil {
		*m = map[string]T{}
	}
	(*m)[name] = x
	return nil
}

// canonical is c as the encoder writes it, to compare two configs by.
func canonical(c Config) (string, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return "", fmt.Errorf("config: %w; %w", err, ErrNotWritten)
	}
	return buf.String(), nil
}

// encodeTable is [<table>.<name>] holding v, its sub-tables included.
func encodeTable(table, name string, v any) (string, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(map[string]map[string]any{table: {name: v}}); err != nil {
		return "", fmt.Errorf("config: %w; %w", err, ErrNotWritten)
	}
	return strings.TrimPrefix(buf.String(), "["+table+"]\n"), nil
}

// spliceTable replaces every section of data whose header is
// [<table>.<name>] or one of its sub-tables with block: the first where
// it stood, the others removed. With none, block goes after the last
// section of table, or at the end. An empty block removes the sections
// and the comment lines directly above them.
func spliceTable(data, table, name, block string) string {
	lines := splitLines(data)
	secs := sections(lines)
	type span struct{ from, to int }
	var spans []span
	lastOfTable := -1
	for i, s := range secs {
		if len(s.key) >= 1 && s.key[0] == table {
			lastOfTable = i
		}
		if len(s.key) < 2 || s.key[0] != table || s.key[1] != name {
			continue
		}
		from, to := s.start, s.end
		if i+1 < len(secs) {
			// Comments and blank lines just above the next header are the
			// next section's.
			for to > from+1 && (blank(lines[to-1]) || comment(lines[to-1])) {
				to--
			}
		}
		if block == "" {
			// Comments directly above are its own when a blank line sets
			// them off from what precedes them; the blank line goes too.
			above := from
			for above > 0 && comment(lines[above-1]) {
				above--
			}
			if above > 0 && blank(lines[above-1]) {
				from = above - 1
			} else if above == from && from > 0 && blank(lines[from-1]) {
				from--
			}
		}
		spans = append(spans, span{from, to})
	}
	blockLines := splitLines(block)
	if len(spans) == 0 {
		if block == "" {
			return data
		}
		at := len(lines)
		if lastOfTable >= 0 {
			at = secs[lastOfTable].end
			for at > secs[lastOfTable].start+1 && (blank(lines[at-1]) || comment(lines[at-1])) {
				at--
			}
		}
		ins := blockLines
		if at > 0 && !blank(lines[at-1]) {
			ins = append([]string{"\n"}, ins...)
		}
		if at < len(lines) && !blank(lines[at]) {
			ins = append(ins, "\n")
		}
		if at > 0 && !strings.HasSuffix(lines[at-1], "\n") {
			lines[at-1] += "\n"
		}
		return strings.Join(append(append(append([]string{}, lines[:at]...), ins...), lines[at:]...), "")
	}
	var out []string
	prev := 0
	for i, sp := range spans {
		if i > 0 && sp.from > prev && blank(lines[sp.from-1]) {
			sp.from-- // the blank line that separated a sub-table
		}
		out = append(out, lines[prev:sp.from]...)
		if i == 0 {
			out = append(out, blockLines...)
		}
		prev = sp.to
	}
	out = append(out, lines[prev:]...)
	return strings.Join(out, "")
}

// section is one table header line and the lines up to the next, as
// line indices [start, end).
type section struct {
	key        []string
	start, end int
}

// sections finds data's table headers, skipping what only looks like one
// inside a multi-line string or array.
func sections(lines []string) []section {
	var out []section
	var sc scanner
	for i, l := range lines {
		if sc.atTop() {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "[") {
				if key, ok := headerKey(t); ok {
					if n := len(out); n > 0 {
						out[n-1].end = i
					}
					out = append(out, section{key: key, start: i, end: len(lines)})
					continue
				}
			}
		}
		sc.line(l)
	}
	return out
}

// scanner tracks whether a line starts inside a multi-line string or an
// open bracket.
type scanner struct {
	depth int
	ml    string // the open multi-line string's quotes, if any
}

func (s *scanner) atTop() bool { return s.depth == 0 && s.ml == "" }

func (s *scanner) line(l string) {
	for i := 0; i < len(l); i++ {
		if s.ml != "" {
			if strings.HasPrefix(l[i:], s.ml) {
				i += len(s.ml) - 1
				s.ml = ""
			} else if s.ml == `"""` && l[i] == '\\' {
				i++
			}
			continue
		}
		switch c := l[i]; c {
		case '#':
			return
		case '[', '{':
			s.depth++
		case ']', '}':
			if s.depth > 0 {
				s.depth--
			}
		case '"', '\'':
			q := string(c)
			if strings.HasPrefix(l[i:], q+q+q) {
				s.ml = q + q + q
				i += 2
				continue
			}
			for i++; i < len(l) && l[i] != c; i++ {
				if c == '"' && l[i] == '\\' {
					i++
				}
			}
		}
	}
}

// headerKey is the dotted key of a [table] or [[array]] header line.
func headerKey(t string) ([]string, bool) {
	inner := strings.TrimPrefix(t, "[")
	arr := strings.HasPrefix(inner, "[")
	inner = strings.TrimPrefix(inner, "[")
	var key []string
	i := 0
	for {
		for i < len(inner) && (inner[i] == ' ' || inner[i] == '\t') {
			i++
		}
		if i >= len(inner) {
			return nil, false
		}
		var part strings.Builder
		switch inner[i] {
		case '"':
			for i++; i < len(inner) && inner[i] != '"'; i++ {
				if inner[i] == '\\' && i+1 < len(inner) {
					i++
				}
				part.WriteByte(inner[i])
			}
			i++
		case '\'':
			for i++; i < len(inner) && inner[i] != '\''; i++ {
				part.WriteByte(inner[i])
			}
			i++
		default:
			for ; i < len(inner) && strings.IndexByte(" \t.]", inner[i]) < 0; i++ {
				part.WriteByte(inner[i])
			}
		}
		key = append(key, part.String())
		for i < len(inner) && (inner[i] == ' ' || inner[i] == '\t') {
			i++
		}
		if i >= len(inner) {
			return nil, false
		}
		switch inner[i] {
		case '.':
			i++
			continue
		case ']':
			if arr && !strings.HasPrefix(inner[i:], "]]") {
				return nil, false
			}
			return key, true
		}
		return nil, false
	}
}

// splitLines is s's lines, each with its newline.
func splitLines(s string) []string {
	out := strings.SplitAfter(s, "\n")
	if out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func blank(l string) bool   { return strings.TrimSpace(l) == "" }
func comment(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "#") }
