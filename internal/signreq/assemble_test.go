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
// order, on the Network it names; a signature set of the wrong size is refused.
func TestSignedTransactions(t *testing.T) {
	now := time.Unix(10_000, 0)
	conv := key.ConversationID("ecdsa", ecdsaKey.PublicKey)
	r := rowOf(t, 1, 0xa1, now, conv[:], req(ethTx()))
	sig := append(bytes.Repeat([]byte{0x05}, 64), 0)
	history := map[[32]byte]network.Network{ethID: nets[0]}

	got, err := Signed([]cosl.Row{r}, r.MsgID, [][]byte{sig}, history)
	if err != nil {
		t.Fatal(err)
	}
	wantRaw, wantHash, _ := AssembleEthereum(1, ethTx().Intent.(*coslv1.SignTransaction_Ethereum).Ethereum, sig)
	if len(got) != 1 || !bytes.Equal(got[0].Raw, wantRaw) || got[0].Hash != wantHash || got[0].Network.Alias != "eth" || got[0].NetworkConfigID != ethID {
		t.Fatalf("signed = %+v", got)
	}
	if _, err := Signed([]cosl.Row{r}, r.MsgID, nil, history); !errors.Is(err, ErrInvalid) {
		t.Errorf("no signatures: %v", err)
	}
	if _, err := Signed([]cosl.Row{r}, [32]byte{9}, [][]byte{sig}, history); !errors.Is(err, ErrUnknown) {
		t.Errorf("unknown request: %v", err)
	}
	if _, err := Signed([]cosl.Row{r}, r.MsgID, [][]byte{sig}, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown network: %v", err)
	}
}
