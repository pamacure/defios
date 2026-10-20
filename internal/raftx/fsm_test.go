package raftx

import (
	"path/filepath"
	"testing"

	"github.com/hashicorp/raft"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// A Node restarting with a local snapshot restores it against a replica
// that already holds every row, which must leave it alone; an empty
// replica is rewritten to the snapshot (§4.4).
func TestSnapshotRestoreKeepsOwnReplicaAndFillsEmptyOne(t *testing.T) {
	n, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(group.RosterNode{Public: n.Public, DisplayName: "alice", TsnetHostname: "defios-alice"})
	dir := t.TempDir()
	store, err := cosl.Open(filepath.Join(dir, "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	f := newFSM(store, roster, dir)
	author := cosl.Author{Node: n, GroupID: roster.GroupID()}
	for i, text := range []string{"one", "two"} {
		s, err := author.Sign(cosl.TypeAppFreeform, nil, &coslv1.AppFreeform{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		res := f.Apply(&raft.Log{Index: uint64(i + 2), Term: 1, Type: raft.LogCommand, Data: s.Bytes})
		if a, ok := res.(applied); !ok || !a.result.Inserted {
			t.Fatalf("apply %d: %+v", i, res)
		}
	}

	snap, err := f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	snaps := raft.NewInmemSnapshotStore()
	sink, err := snaps.Create(raft.SnapshotVersionMax, 3, 1, raft.Configuration{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.Persist(sink); err != nil {
		t.Fatal(err)
	}
	snap.Release()

	_, rc, err := snaps.Open(sink.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Restore(rc); err != nil {
		t.Fatalf("restore over own replica: %v", err)
	}

	behind, err := cosl.Open(filepath.Join(dir, "behind.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer behind.Close()
	_, rc, err = snaps.Open(sink.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := newFSM(behind, roster, dir).Restore(rc); err != nil {
		t.Fatalf("restore over an empty replica: %v", err)
	}
	theirs, _ := store.Meta()
	ours, _ := behind.Meta()
	if theirs != ours {
		t.Fatalf("restored meta %+v, want %+v", ours, theirs)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "cosl-snap-*")); len(entries) != 0 {
		t.Fatalf("staging files left behind: %v", entries)
	}
}
