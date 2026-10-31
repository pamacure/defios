package signreq

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/network"
	coslv1 "defios.com/proto/cosl/v1"
)

// The vector is `cast mktx` (foundry) signing the same intent with a known key:
// the assembled bytes and hash must match what an independent signer produced.
func TestAssembleEthereumMatchesIndependentSigner(t *testing.T) {
	in := &coslv1.EthereumIntent{
		To: bytes.Repeat([]byte{0x11}, 20), Value: "1000", Nonce: 7, GasLimit: 21000,
		MaxFeePerGas: "30000000000", MaxPriorityFeePerGas: "1",
	}
	sig, _ := hex.DecodeString("86c72e3d7af0de0ffcbc9e1c83b4ee33ecb4cc0c7c02f7a22e688fceb1f02ed4" +
		"6d600f2e944b0a3e20394d4de923705fb508018ced594aa800174c757fb99744" + "01")
	raw, hash, err := AssembleEthereum(1, in, sig)
	if err != nil {
		t.Fatal(err)
	}
	const wantRaw = "02f8690107018506fc23ac008252089411111111111111111111111111111111111111118203e880c001a086c72e3d7af0de0ffcbc9e1c83b4ee33ecb4cc0c7c02f7a22e688fceb1f02ed4a06d600f2e944b0a3e20394d4de923705fb508018ced594aa800174c757fb99744"
	const wantHash = "4f1d74cd1550681ba1fd8b8ba3d7d02c86323956807537ea6fd7da85dbd05570"
	if got := hex.EncodeToString(raw); got != wantRaw {
		t.Errorf("raw tx\n got %s\nwant %s", got, wantRaw)
	}
	if got := hex.EncodeToString(hash[:]); got != wantHash {
		t.Errorf("hash %s, want %s", got, wantHash)
	}
}

func TestAssembleEthereumRejectsBadSignature(t *testing.T) {
	in := &coslv1.EthereumIntent{To: bytes.Repeat([]byte{0x11}, 20), GasLimit: 21000, MaxFeePerGas: "1"}
	if _, _, err := AssembleEthereum(1, in, make([]byte, 64)); err == nil {
		t.Fatal("a 64-byte signature must be rejected")
	}
	if _, _, err := AssembleEthereum(1, in, append(make([]byte, 64), 2)); err == nil {
		t.Fatal("recovery id 2 must be rejected")
	}
}

// A request's signed transactions pair each transaction with its signature, in
// order, on the chain it names; a signature set of the wrong size is refused.
func TestSignedTransactions(t *testing.T) {
	now := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	r := rowOf(t, 1, 0xa1, now, conv[:], req(ethTx()))
	sig := append(bytes.Repeat([]byte{0x05}, 64), 0)

	got, err := Signed([]cosl.Row{r}, r.MsgID, [][]byte{sig})
	if err != nil {
		t.Fatal(err)
	}
	wantRaw, wantHash, _ := AssembleEthereum(1, ethTx().Intent.(*coslv1.SignTransaction_Ethereum).Ethereum, sig)
	if len(got) != 1 || !bytes.Equal(got[0].Raw, wantRaw) || !bytes.Equal(got[0].ID, wantHash[:]) || got[0].Chain != (network.Chain{Type: "ethereum", ChainID: 1}) {
		t.Fatalf("signed = %+v", got)
	}
	if _, err := Signed([]cosl.Row{r}, r.MsgID, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("no signatures: %v", err)
	}
	if _, err := Signed([]cosl.Row{r}, [32]byte{9}, [][]byte{sig}); !errors.Is(err, ErrUnknown) {
		t.Errorf("unknown request: %v", err)
	}
	noChain := ethTx()
	noChain.GetEthereum().ChainId = 0
	bad := rowOf(t, 2, 0xb2, now, conv[:], req(noChain))
	if _, err := Signed([]cosl.Row{bad}, bad.MsgID, [][]byte{sig}); !errors.Is(err, ErrInvalid) {
		t.Errorf("no chain id: %v", err)
	}
}

// Several transactions on different chains of one curve assemble in order,
// each bound to its own chain id and signature.
func TestSignedMultipleTransactionsAcrossNetworks(t *testing.T) {
	second := ethTx()
	second.GetEthereum().ChainId = 10
	third := ethTx()
	third.Intent.(*coslv1.SignTransaction_Ethereum).Ethereum.Nonce = 1

	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	r := rowOf(t, 1, 0xa1, time.Unix(10_000, 0), conv[:], req(ethTx(), second, third))
	sigs := [][]byte{append(bytes.Repeat([]byte{1}, 64), 0), append(bytes.Repeat([]byte{2}, 64), 1), append(bytes.Repeat([]byte{3}, 64), 0)}

	got, err := Signed([]cosl.Row{r}, r.MsgID, sigs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Chain.ChainID != 1 || got[1].Chain.ChainID != 10 || got[2].Chain.ChainID != 1 {
		t.Fatalf("signed = %+v", got)
	}
	for i, tx := range []*coslv1.SignTransaction{ethTx(), second, third} {
		raw, _, _ := AssembleEthereum(tx.GetEthereum().ChainId, tx.Intent.(*coslv1.SignTransaction_Ethereum).Ethereum, sigs[i])
		if !bytes.Equal(got[i].Raw, raw) {
			t.Errorf("transaction %d is not bound to its own Network and signature", i)
		}
	}
	if bytes.Equal(got[0].Raw, got[2].Raw) {
		t.Error("distinct nonces must give distinct transactions")
	}
}
