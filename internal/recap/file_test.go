package recap_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"defios.com/internal/recap"
)

// A Recap is written as RECAP.md, its base_seq in a front matter and its
// text after it, and reads back as written.
func TestARecapRoundTripsThroughRECAPmd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations", "group", "RECAP.md")
	if err := recap.Write(path, recap.Recap{BaseSeq: 1234, Text: "alice asked\nbob answered"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "---\nbase_seq: 1234\n---\nalice asked\nbob answered\n"; string(b) != want {
		t.Fatalf("RECAP.md = %q, want %q", b, want)
	}
	got, err := recap.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != (recap.Recap{BaseSeq: 1234, Text: "alice asked\nbob answered"}) {
		t.Fatalf("read %+v", got)
	}
}

// With no RECAP.md there is no Recap, and that is not an error.
func TestNoRECAPmdIsNoRecap(t *testing.T) {
	got, err := recap.Read(filepath.Join(t.TempDir(), "RECAP.md"))
	if err != nil || got != nil {
		t.Fatalf("read = %+v, %v; want nil, nil", got, err)
	}
}

// A RECAP.md an operator may have edited reads with a CRLF or blank lines
// in its front matter or around its text, but one that does not parse is
// an error naming the file and what is wrong, never silently ignored.
func TestARECAPmdThatDoesNotParseIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "RECAP.md")
	for body, want := range map[string]string{
		"no front matter\n":                         "front matter",
		"---\nbase_seq: 3\n":                        "front matter",
		"---\n---\ntext\n":                          "base_seq",
		"---\nbase_seq: three\n---\ntext\n":         "base_seq",
		"---\nbase_seq: 0\n---\ntext\n":             "base_seq",
		"---\nbase_seq: 3\nbase_seq: 4\n---\nt\n":   "base_seq",
		"---\nbase_seq: 3\nauthor: me\n---\ntext\n": "author",
		"---\nbase_seq: 3\n---\n\n":                 "empty",
		"---\nbase_seq: 3\n---\n\xff\n":             "UTF-8",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := recap.Read(path)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), path) {
			t.Errorf("%q: read = %+v, %v; want an error naming %s and %q", body, got, err, path, want)
		}
	}
	if err := os.WriteFile(path, []byte("---\r\n\r\nbase_seq: 7\r\n  \r\n---\r\n\r\nedited by hand\r\n\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := recap.Read(path); err != nil || got == nil || *got != (recap.Recap{BaseSeq: 7, Text: "edited by hand"}) {
		t.Errorf("hand-edited: %+v, %v", got, err)
	}
}

// Writing a Recap replaces RECAP.md whole through a new file renamed over
// it: the old file is never written in place, so a crash before the rename
// leaves it intact, and nothing else is left in the directory.
func TestWriteReplacesRECAPmdWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "RECAP.md")
	if err := recap.Write(path, recap.Recap{BaseSeq: 2, Text: "old"}); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := recap.Write(path, recap.Recap{BaseSeq: 9, Text: "new"}); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(old)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "---\nbase_seq: 2\n---\nold\n" {
		t.Fatalf("the old RECAP.md was written in place: it now reads %q", b)
	}
	if got, err := recap.Read(path); err != nil || got.Text != "new" || got.BaseSeq != 9 {
		t.Fatalf("read %+v, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %v, want RECAP.md alone", entries)
	}
}

// A Recap whose text app.freeform would refuse, or with no base_seq, is not
// written.
func TestWriteRefusesABadRecap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "RECAP.md")
	for _, r := range []recap.Recap{{BaseSeq: 1}, {Text: "no base"}, {BaseSeq: 1, Text: "\xff"}} {
		if err := recap.Write(path, r); err == nil {
			t.Errorf("wrote %+v", r)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("RECAP.md exists: %v", err)
	}
}
