package cosl_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"defios.com/internal/cosl"
)

// Group-level talk is the rows whose conversation id is unset (§8);
// Key-scoped rows stay out, and --limit composes with the filter.
func TestLogGroupLevelFilter(t *testing.T) {
	f := newFixture(t)
	scoped := bytes.Repeat([]byte{9}, 32)
	a := cosl.Author{Node: f.nodes[0], GroupID: f.roster.GroupID()}
	b := cosl.Author{Node: f.nodes[1], GroupID: f.roster.GroupID()}
	sign := func(au cosl.Author, conv []byte, text string) []byte {
		s, err := au.Sign(cosl.TypeAppFreeform, conv, freeform(text))
		if err != nil {
			t.Fatal(err)
		}
		return s.Bytes
	}
	f.apply(1, 1, sign(a, nil, "one"))
	f.apply(2, 1, sign(b, scoped, "scoped"))
	f.apply(3, 1, sign(b, nil, "two"))
	f.apply(4, 1, sign(a, nil, "three"))

	rows, err := f.store.Log(cosl.Query{GroupLevel: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Seq != 1 || rows[1].Seq != 3 || rows[2].Seq != 4 {
		t.Fatalf("group-level rows = %+v", rows)
	}
	rows, err = f.store.Log(cosl.Query{GroupLevel: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Seq != 3 || rows[1].Seq != 4 {
		t.Fatalf("latest 2 group-level rows = %+v", rows)
	}
}

// Freeform text is UTF-8 and at most 65536 bytes (§5.2 type 6).
func TestFreeformTextRules(t *testing.T) {
	msg, err := cosl.Freeform("hello")
	if err != nil || msg.Text != "hello" || msg.ContentType != "text/plain" {
		t.Fatalf("Freeform(hello) = %+v, %v", msg, err)
	}
	if _, err := cosl.Freeform(strings.Repeat("x", 65536)); err != nil {
		t.Fatalf("65536 bytes rejected: %v", err)
	}
	if _, err := cosl.Freeform(strings.Repeat("x", 65537)); !errors.Is(err, cosl.ErrBadText) {
		t.Fatalf("65537 bytes: %v", err)
	}
	if _, err := cosl.Freeform("\xff\xfe"); !errors.Is(err, cosl.ErrBadText) {
		t.Fatalf("invalid UTF-8: %v", err)
	}
	if _, err := cosl.Freeform(""); !errors.Is(err, cosl.ErrBadText) {
		t.Fatalf("empty: %v", err)
	}
}
