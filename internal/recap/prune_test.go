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
	l := newLog(t)
	l.add(entries...)
	return l.store
}

// testLog is a one-Node Group's replica that entries can be committed to.
type testLog struct {
	t      *testing.T
	store  *cosl.Store
	roster group.Roster
	fsm    *cosl.FSM
	author cosl.Author
	signed [][]byte // every entry committed, in seq order
}

func newLog(t *testing.T) *testLog {
	t.Helper()
	n, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(group.RosterNode{Public: n.Public, DisplayName: "alice", TsnetHostname: "defios-alice"})
	return openLog(t, roster, cosl.Author{Node: n, GroupID: roster.GroupID()})
}

func openLog(t *testing.T, roster group.Roster, author cosl.Author) *testLog {
	t.Helper()
	store, err := cosl.Open(filepath.Join(t.TempDir(), "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &testLog{t: t, store: store, roster: roster, fsm: cosl.NewFSM(store, roster), author: author}
}

// sibling is another Node's replica of the same Group holding the first n
// entries this one committed, so the two chains agree that far.
func (l *testLog) sibling(n int) *testLog {
	l.t.Helper()
	s := openLog(l.t, l.roster, l.author)
	for _, b := range l.signed[:n] {
		s.apply(b)
	}
	return s
}

// add commits entries at the next seqs.
func (l *testLog) add(entries ...entry) {
	l.t.Helper()
	for _, e := range entries {
		s, err := l.author.Sign(e.typ, e.conv, e.msg)
		if err != nil {
			l.t.Fatal(err)
		}
		l.apply(s.Bytes)
	}
}

func (l *testLog) apply(b []byte) {
	l.t.Helper()
	l.signed = append(l.signed, b)
	seq := uint64(len(l.signed))
	if res, err := l.fsm.Apply(seq, 1, b); err != nil || !res.Inserted {
		l.t.Fatalf("apply %d: %+v %v", seq, res, err)
	}
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
