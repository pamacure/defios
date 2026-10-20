package tssl

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/bnb-chain/tss-lib/v4/tss"
)

func TestPartyIDsUseNodeIDKeyAndHexID(t *testing.T) {
	// 0x00ff < 0x0100 as integers, so the leading-zero Node sorts first
	// even though its raw bytes are not lexicographically smaller at byte 0
	// after the integer's own encoding drops the zero.
	low := node(0x00, 0xff)
	high := node(0x01, 0x00)
	sorted, self, byNode, err := partyIDs([][32]byte{high, low}, low)
	if err != nil {
		t.Fatal(err)
	}
	if len(sorted) != 2 || sorted[0].Index != 0 || sorted[1].Index != 1 {
		t.Fatalf("indices = %d, %d", sorted[0].Index, sorted[1].Index)
	}
	if got := nodeOf(t, sorted[0]); got != low {
		t.Fatalf("first party = %x, want the smaller Node ID %x", got, low)
	}
	if sorted[0].Id != hex.EncodeToString(low[:]) || sorted[1].Id != hex.EncodeToString(high[:]) {
		t.Fatalf("ids = %s, %s", sorted[0].Id, sorted[1].Id)
	}
	if !sameInt(sorted[0].KeyInt(), new(big.Int).SetBytes(low[:])) {
		t.Fatal("PartyID.Key is not the big-endian Node ID")
	}
	if byNode[low] != self || self.Index != 0 {
		t.Fatal("self was not the low Node")
	}
	// SortPartyIDs is what assigned the indices; a hand-rolled order would
	// disagree when the input is reversed.
	again, _, _, err := partyIDs([][32]byte{low, high}, high)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Id != sorted[0].Id || again[1].Index != 1 {
		t.Fatal("sort order changed with input order")
	}
}

func node(b0, b1 byte) [32]byte {
	var id [32]byte
	id[0], id[1] = b0, b1
	return id
}

func nodeOf(t *testing.T, id *tss.PartyID) [32]byte {
	t.Helper()
	n, ok := nodeID(id)
	if !ok {
		t.Fatal("no node id")
	}
	return n
}

func sameInt(a, b *big.Int) bool { return a.Cmp(b) == 0 }
