package group_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	"defios.com/internal/group"
	"defios.com/internal/identity"
)

func id(b byte) identity.ID {
	var out identity.ID
	for i := range out {
		out[i] = b
	}
	return out
}

func TestGroupIDSortsNodeIDsBeforeHashing(t *testing.T) {
	// Vector computed independently:
	//   { printf 'defios-group-id-v1'; head -c 32 /dev/zero; printf '\x01%.0s' $(seq 32); } | sha256sum
	const want = "322fdad70cc9edbe3f8a88be69647b87197d3bbad68183dcbff374a7d1576a0c"
	ordered := group.GroupID([]identity.ID{id(0x00), id(0x01)})
	reversed := group.GroupID([]identity.ID{id(0x01), id(0x00)})
	if hex.EncodeToString(ordered[:]) != want {
		t.Fatalf("GroupID = %x, want %s", ordered, want)
	}
	if ordered != reversed {
		t.Fatal("GroupID depends on input order; must sort")
	}
}

func TestRosterLookupAndGroupID(t *testing.T) {
	a := group.RosterNode{Public: identity.Public{NodeID: id(0x0a), AuthPublicKey: bytes.Repeat([]byte{1}, 32)}, DisplayName: "a"}
	b := group.RosterNode{Public: identity.Public{NodeID: id(0x0b), AuthPublicKey: bytes.Repeat([]byte{2}, 32)}, DisplayName: "b"}
	r := group.NewRoster(b, a) // out of order on purpose

	got, ok := r.Lookup(id(0x0a))
	if !ok || got.DisplayName != "a" {
		t.Fatalf("Lookup(a) = %+v, %v", got, ok)
	}
	if _, ok := r.Lookup(id(0x0c)); ok {
		t.Fatal("Lookup of a stranger succeeded")
	}
	if r.GroupID() != group.GroupID([]identity.ID{id(0x0a), id(0x0b)}) {
		t.Fatal("Roster.GroupID disagrees with GroupID over its Node IDs")
	}
	if ids := r.NodeIDs(); ids[0] != id(0x0a) || ids[1] != id(0x0b) {
		t.Fatalf("NodeIDs not sorted: %x", ids)
	}
}
