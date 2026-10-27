package cosl_test

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

// Prune destroys the payload of one Conversation's app.freeform rows at or
// below a seq and nothing else: every row, its hash and its place in the
// chain stay, nothing is appended, and doing it twice changes nothing
// (ADR 0012).
func TestPruneDestroysOnlyCoveredTalk(t *testing.T) {
	f := newFixture(t)
	conv := bytes.Repeat([]byte{7}, 32)
	talk := func(c []byte, text string) []byte {
		env := f.envelope(0, cosl.TypeAppFreeform, freeform(text))
		env.ConversationId = c
		return f.signed(0, env)
	}
	f.apply(1, 1, talk(nil, "one"))
	f.apply(2, 1, talk(conv, "scoped"))
	f.apply(3, 1, f.signed(0, f.envelope(0, cosl.TypeAppRecap, &coslv1.AppRecap{Text: "one", BaseSeq: 1})))
	f.apply(4, 1, talk(nil, "two"))
	f.apply(5, 1, talk(nil, "after"))
	before := f.rows()
	meta, _ := f.store.Meta()

	n, err := f.store.Prune(nil, 4)
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v; want 2", n, err)
	}
	after := f.rows()
	sameRows(t, before, after)
	for i, r := range after {
		if want := r.Seq == 1 || r.Seq == 4; r.Pruned() != want {
			t.Errorf("seq %d pruned = %v, want %v", r.Seq, r.Pruned(), want)
		}
		if r.EnvelopeHash != before[i].EnvelopeHash {
			t.Errorf("seq %d envelope_hash changed", r.Seq)
		}
	}
	if m, _ := f.store.Meta(); m != meta {
		t.Fatalf("meta %+v, want %+v unchanged", m, meta)
	}
	if n, err := f.store.Prune(nil, 4); err != nil || n != 0 {
		t.Fatalf("second Prune = %d, %v; want 0", n, err)
	}
	if n, err := f.store.Prune(conv, 5); err != nil || n != 1 {
		t.Fatalf("Prune of the scoped Conversation = %d, %v; want 1", n, err)
	}

	// The pruned replica verifies from genesis as a snapshot, and an
	// unpruned Node's snapshot further along restores over it.
	verifier := f.sibling()
	if err := verifier.store.Restore(f.snapshotOf()); err != nil {
		t.Fatalf("restore of pruned snapshot: %v", err)
	}
	archive := f.sibling()
	for _, r := range before {
		archive.apply(r.Seq, r.Term, r.Envelope)
	}
	archive.apply(6, 1, talk(nil, "later"))
	if err := f.store.Restore(archive.snapshotOf()); err != nil {
		t.Fatalf("restore from unpruned Node: %v", err)
	}
	sameRows(t, archive.rows(), f.rows())
}

// The append-only trigger admits exactly Pruning: an app.freeform's
// envelope set to NULL with every other column unchanged.
func TestTriggerAdmitsOnlyPruning(t *testing.T) {
	f := newFixture(t)
	f.apply(1, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("talk"))))
	f.apply(2, 1, f.signed(0, f.envelope(0, cosl.TypeAppRecap, &coslv1.AppRecap{Text: "t", BaseSeq: 1})))
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"UPDATE cosl SET envelope = NULL WHERE seq = 2",
		"UPDATE cosl SET envelope = X'00' WHERE seq = 1",
		"UPDATE cosl SET envelope = NULL, term = 9 WHERE seq = 1",
		"UPDATE cosl SET envelope = NULL, envelope_hash = zeroblob(32) WHERE seq = 1",
		"UPDATE cosl SET envelope = NULL, msg_type = 6 WHERE seq = 2",
	} {
		if _, err := db.Exec(stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want append-only abort", stmt, err)
		}
	}
	if _, err := db.Exec("UPDATE cosl SET envelope = NULL WHERE seq = 1"); err != nil {
		t.Fatalf("pruning update refused: %v", err)
	}
}

// Opening a replica that already has this binary's triggers writes nothing:
// a CLI read must never contend with serve's FSM for the write lock.
func TestOpenLeavesCurrentSchemaAlone(t *testing.T) {
	f := newFixture(t)
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	version := func() int {
		var v int
		if err := db.QueryRow("PRAGMA schema_version").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := version()
	again, err := cosl.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	if after := version(); after != before {
		t.Fatalf("schema_version %d after Open, want %d unchanged", after, before)
	}
}
