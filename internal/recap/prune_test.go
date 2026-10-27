package recap

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// entry is one message to commit to a test replica.
type entry struct {
	typ  uint32
	conv []byte
	msg  proto.Message
}

// replica commits entries at seqs 1.. through the FSM of a one-Node Group.
func replica(t *testing.T, entries ...entry) *cosl.Store {
	t.Helper()
	n, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(group.RosterNode{Public: n.Public, DisplayName: "alice", TsnetHostname: "defios-alice"})
	store, err := cosl.Open(filepath.Join(t.TempDir(), "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fsm := cosl.NewFSM(store, roster)
	author := cosl.Author{Node: n, GroupID: roster.GroupID()}
	for i, e := range entries {
		s, err := author.Sign(e.typ, e.conv, e.msg)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := fsm.Apply(uint64(i+1), 1, s.Bytes); err != nil || !res.Inserted {
			t.Fatalf("apply %d: %+v %v", i+1, res, err)
		}
	}
	return store
}

func said(conv []byte, text string) entry {
	return entry{cosl.TypeAppFreeform, conv, &coslv1.AppFreeform{Text: text}}
}

func recapped(conv []byte, base uint64, text string) entry {
	return entry{cosl.TypeAppRecap, conv, &coslv1.AppRecap{Text: text, BaseSeq: base}}
}

func pruned(t *testing.T, store *cosl.Store) []uint64 {
	t.Helper()
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	var out []uint64
	for _, r := range rows {
		if r.Pruned() {
			out = append(out, r.Seq)
		}
	}
	return out
}

// Prune destroys exactly the talk the live Recap accounts for: never a
// Recap, never talk after its base, never another Conversation's, and a
// Recap the fold skips widens nothing.
func TestPruneDestroysWhatTheLiveRecapCovers(t *testing.T) {
	other := bytes.Repeat([]byte{3}, 32)
	store := replica(t,
		said(nil, "a"),             // 1
		said(nil, "b"),             // 2
		recapped(nil, 1, "a"),      // 3
		recapped(nil, 2, "a b"),    // 4: live
		said(other, "elsewhere"),   // 5
		said(nil, "c"),             // 6
		recapped(nil, 5, "stolen"), // 7: names another Conversation's talk, skipped
	)
	base, n, err := Prune(store, nil)
	if err != nil || base != 2 || n != 2 {
		t.Fatalf("Prune = %d, %d, %v; want base 2, 2 pruned", base, n, err)
	}
	if got := pruned(t, store); !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("pruned seqs = %v, want [1 2]", got)
	}
	if base, n, err := Prune(store, nil); err != nil || base != 2 || n != 0 {
		t.Fatalf("second Prune = %d, %d, %v; want base 2, none pruned", base, n, err)
	}
}

// With no live Recap nothing is accounted for, so nothing is prunable.
func TestPruneWithoutRecapRefuses(t *testing.T) {
	conv := bytes.Repeat([]byte{4}, 32)
	store := replica(t,
		said(conv, "a"),
		recapped(nil, 1, "the Group-level Recap names this talk, so the fold skips it"),
	)
	if _, _, err := Prune(store, conv); !errors.Is(err, ErrNoRecap) {
		t.Fatalf("Prune = %v, want ErrNoRecap", err)
	}
	if _, _, err := Prune(store, nil); !errors.Is(err, ErrNoRecap) {
		t.Fatalf("Group-level Prune = %v, want ErrNoRecap", err)
	}
	if got := pruned(t, store); len(got) != 0 {
		t.Fatalf("pruned seqs = %v, want none", got)
	}
}
