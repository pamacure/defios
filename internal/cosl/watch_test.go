package cosl_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

func sameSeqs(t *testing.T, got []cosl.Row, want ...uint64) {
	t.Helper()
	g := seqs(got)
	if len(g) != len(want) {
		t.Fatalf("seqs %v, want %v", g, want)
	}
	for i := range g {
		if g[i] != want[i] {
			t.Fatalf("seqs %v, want %v", g, want)
		}
	}
}

// A read can ask for a set of message types and for rows after a seq, so a
// reader decodes only what it folds and only what it has not seen.
func TestQueryNarrowsToTypesAndAfterSeq(t *testing.T) {
	f := newFixture(t)
	f.apply(1, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("a"))))
	f.apply(2, 1, f.signed(0, f.envelope(0, cosl.TypeSessionAbort, &coslv1.SessionAbort{SessionId: make([]byte, 32)})))
	f.apply(3, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("b"))))
	f.apply(4, 1, f.signed(0, f.envelope(0, cosl.TypeSessionAbort, &coslv1.SessionAbort{SessionId: make([]byte, 32)})))
	f.apply(5, 1, f.signed(0, f.envelope(0, cosl.TypeAppRecap, &coslv1.AppRecap{})))

	read := func(q cosl.Query) []cosl.Row {
		t.Helper()
		rows, err := f.store.Log(q)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	sameSeqs(t, read(cosl.Query{MsgTypes: []uint32{cosl.TypeSessionAbort, cosl.TypeAppRecap}}), 2, 4, 5)
	sameSeqs(t, read(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}}), 1, 3)
	sameSeqs(t, read(cosl.Query{AfterSeq: 3}), 4, 5)
	sameSeqs(t, read(cosl.Query{MsgTypes: []uint32{cosl.TypeSessionAbort}, AfterSeq: 2}), 4)
	sameSeqs(t, read(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}, AfterSeq: 3}))
	sameSeqs(t, read(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}, Limit: 1}), 3)
}

// A Follower hands its reader a wake only when the replica has moved since
// the reader last acted: first on startup, as a rescan; then for rows after
// the seq it acted on; and as a rescan again after a restore. A wake the
// reader did not finish is handed out again.
func TestFollowerWakesOnlyWhenTheReplicaMoved(t *testing.T) {
	leader := newFixture(t)
	f := leader.sibling()
	fw := f.store.Follow(time.Hour)

	poll := func() (cosl.Wake, bool) {
		t.Helper()
		w, ok, err := fw.Poll()
		if err != nil {
			t.Fatal(err)
		}
		return w, ok
	}
	// both commits a row on the leader and on f alike, so f's replica stays
	// a prefix of the leader's.
	both := func(seq uint64, msg *coslv1.AppFreeform) {
		t.Helper()
		data := leader.signed(0, leader.envelope(0, cosl.TypeAppFreeform, msg))
		leader.apply(seq, 1, data)
		f.apply(seq, 1, data)
	}
	if w, ok := poll(); !ok || !w.Rescan {
		t.Fatalf("first poll = %+v, %v; want a rescan", w, ok)
	}
	fw.Done()
	if w, ok := poll(); ok {
		t.Fatalf("poll with nothing new = %+v", w)
	}

	both(1, freeform("a"))
	both(2, freeform("b"))
	w, ok := poll()
	if !ok || w.Rescan || w.Since != 0 || w.Mark.LastAppliedSeq != 2 {
		t.Fatalf("poll after two rows = %+v, %v", w, ok)
	}
	fw.Done()
	both(3, freeform("c"))
	if w, ok := poll(); !ok || w.Rescan || w.Since != 2 {
		t.Fatalf("poll after a third row = %+v, %v", w, ok)
	}
	// The reader failed and did not call Done: the same rows again.
	if w, ok := poll(); !ok || w.Since != 2 {
		t.Fatalf("poll after an unfinished wake = %+v, %v", w, ok)
	}
	fw.Done()

	leader.fill(5)
	if err := f.store.Restore(leader.snapshotOf()); err != nil {
		t.Fatal(err)
	}
	if w, ok := poll(); !ok || !w.Rescan {
		t.Fatalf("poll after a restore = %+v, %v; want a rescan", w, ok)
	}
	fw.Done()
	if w, ok := poll(); ok {
		t.Fatalf("poll after acting on the restore = %+v", w)
	}
}

// Act runs the reader only when the replica moved, and marks the wake done
// only if the reader succeeded: a failed one is handed out again.
func TestFollowerActRunsOnlyOnAMoveAndRetriesAFailure(t *testing.T) {
	f := newFixture(t)
	fw := f.store.Follow(time.Hour)
	var got []cosl.Wake
	fail := errors.New("reader failed")
	act := func(err error) func(cosl.Wake) error {
		return func(w cosl.Wake) error { got = append(got, w); return err }
	}
	if err := fw.Act(act(nil)); err != nil || len(got) != 1 || !got[0].Rescan {
		t.Fatalf("first Act: %v, wakes %+v", err, got)
	}
	if err := fw.Act(act(nil)); err != nil || len(got) != 1 {
		t.Fatalf("Act with nothing new: %v, wakes %+v", err, got)
	}
	f.apply(1, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("a"))))
	if err := fw.Act(act(fail)); !errors.Is(err, fail) || len(got) != 2 {
		t.Fatalf("failing Act: %v, wakes %+v", err, got)
	}
	if err := fw.Act(act(nil)); err != nil || len(got) != 3 || got[2].Since != 0 {
		t.Fatalf("Act after a failure: %v, wakes %+v", err, got)
	}
	if err := fw.Act(act(nil)); err != nil || len(got) != 3 {
		t.Fatalf("Act after the retry succeeded: %v, wakes %+v", err, got)
	}
}

// A Follower's Wait returns at the next change, or at the fallback tick if
// no change is announced, and reports false once ctx ends.
func TestFollowerWaitsForAChangeOrTheFallbackTick(t *testing.T) {
	f := newFixture(t)
	fw := f.store.Follow(time.Hour)
	if _, _, err := fw.Poll(); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.fsm.Apply(1, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("a"))))
	}()
	start := time.Now()
	if !fw.Wait(context.Background()) {
		t.Fatal("Wait reported ctx done")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Wait took %v to see a change", d)
	}

	ticking := f.store.Follow(30 * time.Millisecond)
	if _, _, err := ticking.Poll(); err != nil {
		t.Fatal(err)
	}
	if !ticking.Wait(context.Background()) {
		t.Fatal("Wait reported ctx done")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if fw.Wait(ctx) {
		t.Fatal("Wait after ctx ended reported true")
	}
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// The replica announces every row the FSM commits, to any number of
// waiters, and nothing else wakes them.
func TestStoreAnnouncesEachCommittedRow(t *testing.T) {
	f := newFixture(t)
	first, second := f.store.Changed(), f.store.Changed()
	if closed(first) || closed(second) {
		t.Fatal("announced before anything committed")
	}
	if _, err := f.store.Log(cosl.Query{}); err != nil {
		t.Fatal(err)
	}
	if closed(first) {
		t.Fatal("a read announced a change")
	}
	f.apply(1, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("a"))))
	if !closed(first) || !closed(second) {
		t.Fatal("a committed row woke no one")
	}
	next := f.store.Changed()
	if closed(next) {
		t.Fatal("a waiter after the row was woken by it")
	}
	f.apply(2, 1, f.signed(0, f.envelope(0, cosl.TypeAppFreeform, freeform("b"))))
	if !closed(next) {
		t.Fatal("the second row woke no one")
	}
}

// A restore that rewrites the replica announces it; one that changes
// nothing does not.
func TestStoreAnnouncesARestore(t *testing.T) {
	leader := newFixture(t)
	leader.fill(3)
	snap := leader.snapshotOf()

	follower := leader.sibling()
	ch := follower.store.Changed()
	if err := follower.store.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if !closed(ch) {
		t.Fatal("a restore woke no one")
	}
	ch = follower.store.Changed()
	if err := follower.store.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if closed(ch) {
		t.Fatal("a restore that changed nothing announced a change")
	}
}
