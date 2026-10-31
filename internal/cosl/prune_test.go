package cosl_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
	f.apply(3, 1, f.signed(0, f.envelope(0, cosl.TypeSessionAbort, &coslv1.SessionAbort{SessionId: make([]byte, 32)})))
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
	if c, err := f.store.Counts(); err != nil || c != (cosl.Counts{Rows: 5, Pruned: 2}) {
		t.Fatalf("Counts = %+v, %v; want 5 rows, 2 pruned", c, err)
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

// Pruning by age destroys the app.freeform payloads whose envelope
// created_at is older than the cutoff, in one Conversation or in every one,
// and nothing else: not newer talk, not another message type (ADR 0024).
func TestPruneOlderThanDestroysOnlyOlderTalk(t *testing.T) {
	f := newFixture(t)
	conv := bytes.Repeat([]byte{7}, 32)
	day := int64(24 * time.Hour)
	at := func(age int64, conv []byte, typ uint32, text string) []byte {
		env := f.envelope(0, typ, freeform(text))
		env.ConversationId = conv
		env.CreatedAt = 1700000000_000000000 - age*day
		return f.signed(0, env)
	}
	f.apply(1, 1, at(40, nil, cosl.TypeAppFreeform, "old"))
	f.apply(2, 1, at(40, conv, cosl.TypeAppFreeform, "old scoped"))
	f.apply(3, 1, at(40, nil, cosl.TypeSignRequest, "not talk"))
	f.apply(4, 1, at(10, nil, cosl.TypeAppFreeform, "recent"))
	f.apply(5, 1, at(35, conv, cosl.TypeAppFreeform, "older scoped"))
	cutoff := time.Unix(0, 1700000000_000000000-30*day)

	if n, err := f.store.PruneOlderThan(nil, cutoff); err != nil || n != 1 {
		t.Fatalf("PruneOlderThan(Group-level) = %d, %v; want 1", n, err)
	}
	if got := prunedSeqs(f); !slices.Equal(got, []uint64{1}) {
		t.Fatalf("pruned seqs = %v, want [1]", got)
	}
	got, err := f.store.PruneAllOlderThan(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if want := []cosl.Pruned{{ConversationID: conv, Messages: 2, Last: 5}}; !slices.EqualFunc(got, want, samePruned) {
		t.Fatalf("PruneAllOlderThan = %+v, want %+v", got, want)
	}
	if got := prunedSeqs(f); !slices.Equal(got, []uint64{1, 2, 5}) {
		t.Fatalf("pruned seqs = %v, want [1 2 5]", got)
	}
	if got, err := f.store.PruneAllOlderThan(cutoff); err != nil || len(got) != 0 {
		t.Fatalf("second PruneAllOlderThan = %+v, %v; want nothing", got, err)
	}
	if err := f.sibling().store.Restore(f.snapshotOf()); err != nil {
		t.Fatalf("restore of pruned snapshot: %v", err)
	}
}

func prunedSeqs(f *fixture) []uint64 {
	var out []uint64
	for _, r := range f.rows() {
		if r.Pruned() {
			out = append(out, r.Seq)
		}
	}
	return out
}

func samePruned(a, b cosl.Pruned) bool {
	return bytes.Equal(a.ConversationID, b.ConversationID) && a.Messages == b.Messages && a.Last == b.Last
}

// Pruning while the FSM appends never fails an append: the prune policy
// prunes on start and after a restore, while the rows after it are still
// being applied, and an entry the FSM fails to apply is a row this Node
// never holds.
func TestPruneAlongsideAppendsFailsNone(t *testing.T) {
	f := newFixture(t)
	talk := func(text string) []byte {
		return f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform(text)))
	}
	f.apply(1, 1, talk("first"))
	f.apply(2, 1, f.signed(0, f.envelope(0, cosl.TypeSessionAbort, &coslv1.SessionAbort{SessionId: make([]byte, 32)})))
	const n = 300
	entries := make([][]byte, n)
	for i := range entries {
		entries[i] = talk(fmt.Sprint("talk ", i))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i, e := range entries {
			if _, err := f.fsm.Apply(uint64(3+i), 1, e); err != nil {
				t.Errorf("Apply(%d) while pruning: %v", 3+i, err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			if got := len(f.rows()); got != n+2 {
				t.Fatalf("%d rows, want %d", got, n+2)
			}
			return
		default:
		}
		if _, err := f.store.Prune(nil, uint64(n+2)); err != nil {
			t.Fatal(err)
		}
	}
}

// The append-only trigger admits exactly Pruning: an app.freeform's
// envelope set to NULL with every other column unchanged.
func TestTriggerAdmitsOnlyPruning(t *testing.T) {
	f := newFixture(t)
	f.apply(1, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("talk"))))
	f.apply(2, 1, f.signed(0, f.envelope(0, cosl.TypeSessionAbort, &coslv1.SessionAbort{SessionId: make([]byte, 32)})))
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
