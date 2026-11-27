package signreq

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"defios.com/internal/base58"
	"defios.com/internal/cosl"
	"defios.com/internal/key"
	coslv1 "defios.com/proto/cosl/v1"
)

func nonceOf(i uint32, n uint64) *coslv1.RefreshedTx {
	return &coslv1.RefreshedTx{TxIndex: i, Field: &coslv1.RefreshedTx_Nonce{Nonce: n}}
}

func blockhashOf(i uint32, h string) *coslv1.RefreshedTx {
	return &coslv1.RefreshedTx{TxIndex: i, Field: &coslv1.RefreshedTx_RecentBlockhash{RecentBlockhash: h}}
}

func TestRefreshedReplacesOnlyTheFieldsItNames(t *testing.T) {
	r := req(ethTx(), solTx(), ethTx())
	fresh := base58.Encode(bytes.Repeat([]byte{7}, 32))
	got, err := Refreshed(r, &coslv1.SignRefresh{Transactions: []*coslv1.RefreshedTx{nonceOf(0, 9), blockhashOf(1, fresh)}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Transactions[0].GetEthereum().Nonce != 9 || got.Transactions[1].GetSolana().RecentBlockhash != fresh ||
		got.Transactions[2].GetEthereum().Nonce != r.Transactions[2].GetEthereum().Nonce {
		t.Errorf("refreshed %v", got)
	}
	if r.Transactions[0].GetEthereum().Nonce == 9 {
		t.Error("the request itself was changed")
	}
}

func TestRefreshedRefusesAMalformedRefresh(t *testing.T) {
	r := req(ethTx(), solTx())
	for name, ref := range map[string][]*coslv1.RefreshedTx{
		"out of range":          {nonceOf(2, 1)},
		"twice":                 {nonceOf(0, 1), nonceOf(0, 2)},
		"nonce on solana":       {nonceOf(1, 1)},
		"blockhash on ethereum": {blockhashOf(0, base58.Encode(make([]byte, 32)))},
		"blockhash not base58":  {blockhashOf(1, "0OIl")},
		"no field":              {{TxIndex: 0}},
	} {
		if _, err := Refreshed(r, &coslv1.SignRefresh{Transactions: ref}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err %v, want ErrInvalid", name, err)
		}
	}
}

// Every Node derives the Digests of the request as its first start
// refreshed it, so the start's signers sign the fresh nonce.
func TestLookupSignsTheRequestAsItsStartRefreshedIt(t *testing.T) {
	start := time.Unix(10000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	r := rowOf(t, 10, 0xa1, start, conv[:], req(ethTx()))
	rows := append(mintedKey(t, start), r)
	before, err := Lookup(rows, r.MsgID)
	if err != nil {
		t.Fatal(err)
	}
	startRow := func(seq uint64, sid byte, ref *coslv1.SignRefresh) cosl.Row {
		return rawRow(t, seq, 1, cosl.TypeSessionStart, start, nil, &coslv1.SessionStart{
			SessionId: bytes32(sid), Curve: "ecdsa", T: 2, N: 2, SignRequestId: r.MsgID[:], SignRefresh: ref})
	}
	rows = append(rows, startRow(11, 0x88, &coslv1.SignRefresh{Transactions: []*coslv1.RefreshedTx{nonceOf(0, 42)}}),
		startRow(12, 0x89, &coslv1.SignRefresh{Transactions: []*coslv1.RefreshedTx{nonceOf(0, 43)}})) // a duplicate start, which does not count
	after, err := Lookup(rows, r.MsgID)
	if err != nil {
		t.Fatal(err)
	}
	fresh := req(ethTx())
	fresh.Transactions[0].GetEthereum().Nonce = 42
	_, want, _ := EthereumDigest(1, fresh.Transactions[0].GetEthereum())
	if bytes.Equal(after.Digests[0], before.Digests[0]) || !bytes.Equal(after.Digests[0], want[:]) {
		t.Errorf("digest %x, want the refreshed request's %x", after.Digests[0], want)
	}

	bad := append(rows[:len(rows)-2:len(rows)-2], startRow(11, 0x88, &coslv1.SignRefresh{Transactions: []*coslv1.RefreshedTx{nonceOf(5, 1)}}))
	if _, err := Lookup(bad, r.MsgID); !errors.Is(err, ErrInvalid) {
		t.Errorf("a malformed refresh: err %v, want ErrInvalid", err)
	}
}
