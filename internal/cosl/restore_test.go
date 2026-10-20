package cosl_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"defios.com/internal/cosl"
)

// fill applies n more freeform rows from alice at consecutive seqs and
// returns every row.
func (f *fixture) fill(n int) []cosl.Row {
	f.t.Helper()
	have := len(f.rows())
	for i := have + 1; i <= have+n; i++ {
		if res := f.apply(uint64(i), 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("m")))); !res.Inserted {
			f.t.Fatalf("apply %d: %+v", i, res)
		}
	}
	return f.rows()
}

// sibling is another Node's empty replica of the same Group: same Roster
// and keys, its own file.
func (f *fixture) sibling() *fixture {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "cosl.sqlite")
	store, err := cosl.Open(path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { store.Close() })
	return &fixture{t: f.t, nodes: f.nodes, roster: f.roster, store: store, fsm: cosl.NewFSM(store, f.roster), path: path}
}

// snapshotOf is what the leader ships: a VACUUM INTO copy.
func (f *fixture) snapshotOf() string {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "snap.sqlite")
	if err := f.store.VacuumInto(path); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func sameRows(t *testing.T, a, b []cosl.Row) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%d rows vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Seq != b[i].Seq || a[i].MsgID != b[i].MsgID || a[i].PrevHash != b[i].PrevHash {
			t.Fatalf("row %d differs:\n%+v\n%+v", i, a[i], b[i])
		}
	}
}

// Restore replaces an empty replica with the snapshot: rows, chain head,
// and last_applied all come across (§4.4), and a reader that opened the
// file before the restore sees the new rows.
func TestRestoreIntoEmptyReplica(t *testing.T) {
	leader := newFixture(t)
	want := leader.fill(5)
	snap := leader.snapshotOf()

	path := filepath.Join(t.TempDir(), "cosl.sqlite")
	fresh, err := cosl.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	reader, err := cosl.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if rows, _ := reader.Log(cosl.Query{}); len(rows) != 0 {
		t.Fatal("fresh replica has rows")
	}

	if err := fresh.Restore(snap); err != nil {
		t.Fatal(err)
	}
	got, err := fresh.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	sameRows(t, want, got)
	lm, _ := leader.store.Meta()
	fm, _ := fresh.Meta()
	if lm != fm {
		t.Fatalf("meta differs: %+v vs %+v", lm, fm)
	}
	seen, err := reader.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	sameRows(t, want, seen)
}

// A replica behind the snapshot is extended by it; the chain it already
// holds must be the snapshot's prefix.
func TestRestoreExtendsPrefixAndRejectsDivergence(t *testing.T) {
	leader := newFixture(t)
	leader.fill(6)
	snap := leader.snapshotOf()

	// Same first 3 entries, then the follower stopped.
	follower := leader.sibling()
	lrows := leader.rows()
	for _, r := range lrows[:3] {
		follower.apply(r.Seq, r.Term, r.Envelope)
	}
	if err := follower.store.Restore(snap); err != nil {
		t.Fatal(err)
	}
	sameRows(t, lrows, follower.rows())

	// A replica whose row 2 is a different envelope is not a prefix.
	stranger := leader.sibling()
	stranger.apply(1, 1, lrows[0].Envelope)
	stranger.apply(2, 1, stranger.signed(0, stranger.envelope(0, cosl.TypeAppFreeform, freeform("other"))))
	before := stranger.rows()
	err := stranger.store.Restore(snap)
	if !errors.Is(err, cosl.ErrBadChain) {
		t.Fatalf("diverged restore = %v, want ErrBadChain", err)
	}
	sameRows(t, before, stranger.rows())
}

// A snapshot whose prev_hash chain does not verify is rejected and the
// live replica untouched.
func TestRestoreRejectsBrokenChain(t *testing.T) {
	leader := newFixture(t)
	leader.fill(4)
	snap := leader.snapshotOf()

	// Tamper with row 3's envelope in the staging copy. The triggers guard
	// the live replica, not a copy, so drop them there first.
	db, err := sql.Open("sqlite", "file:"+snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DROP TRIGGER cosl_no_update",
		"UPDATE cosl SET envelope = X'00' WHERE seq = 3",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	victim := leader.sibling()
	victim.apply(1, 1, leader.rows()[0].Envelope)
	before := victim.rows()
	err = victim.store.Restore(snap)
	if !errors.Is(err, cosl.ErrBadChain) {
		t.Fatalf("tampered restore = %v, want ErrBadChain", err)
	}
	sameRows(t, before, victim.rows())
}

// A snapshot no further along than this replica changes nothing.
func TestRestoreOfOlderSnapshotIsNoOp(t *testing.T) {
	f := newFixture(t)
	f.fill(2)
	snap := f.snapshotOf()
	want := f.fill(2)
	if err := f.store.Restore(snap); err != nil {
		t.Fatal(err)
	}
	sameRows(t, want, f.rows())
}
