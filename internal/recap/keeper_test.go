package recap

import (
	"bytes"
	"path/filepath"
	"slices"
	"testing"
)

// The first Step prunes every Conversation's covered talk; a Step with no new
// Recap prunes nothing; a new Recap extends what is pruned.
func TestKeeperPrunesWhenARecapCommits(t *testing.T) {
	conv := bytes.Repeat([]byte{5}, 32)
	l := newLog(t)
	l.add(
		said(nil, "a"),         // 1
		said(conv, "b"),        // 2
		recapped(nil, 1, "a"),  // 3
		recapped(conv, 2, "b"), // 4
		said(nil, "c"),         // 5
	)
	store := l.store
	k := NewKeeper(store)
	got, err := k.Step()
	if err != nil {
		t.Fatal(err)
	}
	want := []Pruned{{ConversationID: nil, BaseSeq: 1, Messages: 1}, {ConversationID: conv, BaseSeq: 2, Messages: 1}}
	if !samePruned(got, want) {
		t.Fatalf("first Step = %+v, want %+v", got, want)
	}
	if got, err := k.Step(); err != nil || len(got) != 0 {
		t.Fatalf("idle Step = %+v, %v; want nothing", got, err)
	}

	l.add(recapped(nil, 5, "a c")) // 6
	got, err = k.Step()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Pruned{{BaseSeq: 5, Messages: 1}}; !samePruned(got, want) {
		t.Fatalf("Step after a Recap = %+v, want %+v", got, want)
	}
	if got := pruned(t, store); !slices.Equal(got, []uint64{1, 2, 5}) {
		t.Fatalf("pruned seqs = %v, want [1 2 5]", got)
	}
}

// A restore from a replica that kept the talk resurrects it; the next Step
// prunes it again even though no new Recap came with it.
func TestKeeperPrunesAgainAfterARestore(t *testing.T) {
	archive := newLog(t)
	archive.add(said(nil, "a"), said(nil, "b"), recapped(nil, 2, "a b"), said(nil, "c"))
	light := archive.sibling(3).store
	k := NewKeeper(light)
	if got, err := k.Step(); err != nil || !samePruned(got, []Pruned{{BaseSeq: 2, Messages: 2}}) {
		t.Fatalf("first Step = %+v, %v", got, err)
	}

	snap := filepath.Join(t.TempDir(), "snap.sqlite")
	if err := archive.store.VacuumInto(snap); err != nil {
		t.Fatal(err)
	}
	if err := light.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if got := pruned(t, light); len(got) != 0 {
		t.Fatalf("pruned seqs after restore = %v, want the restore to bring them back", got)
	}
	got, err := k.Step()
	if err != nil || !samePruned(got, []Pruned{{BaseSeq: 2, Messages: 2}}) {
		t.Fatalf("Step after restore = %+v, %v; want 2 pruned up to seq 2", got, err)
	}
	if got := pruned(t, light); !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("pruned seqs = %v, want [1 2]", got)
	}
}

func samePruned(a, b []Pruned) bool {
	return slices.EqualFunc(a, b, func(x, y Pruned) bool {
		return bytes.Equal(x.ConversationID, y.ConversationID) && x.BaseSeq == y.BaseSeq && x.Messages == y.Messages
	})
}
