package signreq

import (
	"time"

	"bytes"
	"crypto/ed25519"
	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"encoding/hex"
	"strings"
	"testing"

	"defios.com/internal/base58"
	coslv1 "defios.com/proto/cosl/v1"
)

func b58of(b byte) string { return base58.Encode(bytes.Repeat([]byte{b}, 32)) }

// transfer is a System Program transfer of 1000 lamports from payer 0x01 to
// 0x02 at blockhash 0x03.
func transfer() *coslv1.SolanaIntent {
	return &coslv1.SolanaIntent{
		FeePayer: b58of(1), RecentBlockhash: b58of(3),
		Instructions: []*coslv1.SolanaInstruction{{
			ProgramId: b58, // the System Program, 32 zero bytes
			Accounts:  []*coslv1.SolanaAccount{{Pubkey: b58of(1), Signer: true, Writable: true}, {Pubkey: b58of(2), Writable: true}},
			Data:      []byte{2, 0, 0, 0, 0xe8, 0x03, 0, 0, 0, 0, 0, 0},
		}},
	}
}

// The expected bytes are laid out by hand from the legacy message format:
// header, account keys, blockhash, instructions.
func TestSolanaMessageIsTheLegacyCompiledMessage(t *testing.T) {
	want := "010001" + // 1 signature, 0 readonly signed, 1 readonly unsigned
		"03" + strings.Repeat("01", 32) + strings.Repeat("02", 32) + strings.Repeat("00", 32) +
		strings.Repeat("03", 32) +
		"01" + "02" + "02" + "00" + "01" + "0c" + "02000000e803000000000000"
	got, err := SolanaMessage(transfer())
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != want {
		t.Errorf("message\n got %x\nwant %s", got, want)
	}
}

func TestSolanaMessageOrdersAndMergesAccounts(t *testing.T) {
	in := transfer()
	in.Instructions = append(in.Instructions, &coslv1.SolanaInstruction{
		ProgramId: b58of(9),
		Accounts: []*coslv1.SolanaAccount{
			{Pubkey: b58of(4)},                 // readonly
			{Pubkey: b58of(2)},                 // seen writable: stays writable
			{Pubkey: b58of(1), Signer: true},   // the payer again
			{Pubkey: b58of(5), Writable: true}, // new writable
		},
	})
	msg, err := SolanaMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	// payer | writable 2, 5 | readonly: system(0), 9, 4 by first appearance
	if msg[0] != 1 || msg[1] != 0 || msg[2] != 3 || msg[3] != 6 {
		t.Fatalf("header/count = %v", msg[:4])
	}
	var order []byte
	for i := 0; i < 6; i++ {
		order = append(order, msg[4+32*i])
	}
	if want := []byte{1, 2, 5, 0, 9, 4}; !bytes.Equal(order, want) {
		t.Errorf("account order %v, want %v", order, want)
	}
}

func TestSolanaMessageRefusesWhatTheKeyCannotSign(t *testing.T) {
	other := transfer()
	other.Instructions[0].Accounts[1].Signer = true
	big := transfer()
	big.Instructions[0].Data = make([]byte, 1200)
	badBlockhash := transfer()
	badBlockhash.RecentBlockhash = b58of(3)[:20]
	for name, in := range map[string]*coslv1.SolanaIntent{"second signer": other, "too large": big, "short blockhash": badBlockhash} {
		if _, err := SolanaMessage(in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestShortvec(t *testing.T) {
	for n, want := range map[int]string{0: "00", 127: "7f", 128: "8001", 300: "ac02", 16384: "808001"} {
		if got := hex.EncodeToString(shortvec(n)); got != want {
			t.Errorf("shortvec(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestAssembleSolanaPrefixesTheSignature(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	in := transfer()
	in.FeePayer = base58.Encode(priv.Public().(ed25519.PublicKey))
	in.Instructions[0].Accounts[0].Pubkey = in.FeePayer
	msg, err := SolanaMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, msg)
	raw, err := AssembleSolana(in, sig)
	if err != nil {
		t.Fatal(err)
	}
	if raw[0] != 1 || !bytes.Equal(raw[1:65], sig) || !bytes.Equal(raw[65:], msg) {
		t.Errorf("transaction = %x", raw)
	}
	if _, err := AssembleSolana(in, sig[:63]); err == nil {
		t.Error("a short signature was assembled")
	}
}

func TestBuildDefaultsASolanaRequestToATighterDeadline(t *testing.T) {
	spec := func(deadline uint64, networks ...string) Spec {
		s := Spec{Reason: "r", DeadlineSeconds: deadline}
		for _, n := range networks {
			tx := TxSpec{Network: n}
			if n == "sol" {
				tx.Solana = []byte(`{"feePayer":"` + b58 + `"}`)
			} else {
				tx.Ethereum = []byte(`{}`)
			}
			s.Transactions = append(s.Transactions, tx)
		}
		return s
	}
	for name, c := range map[string]struct {
		spec Spec
		want uint64
	}{
		"solana":          {spec(0, "sol"), SolanaDeadlineSeconds},
		"solana with eth": {spec(0, "eth", "sol"), SolanaDeadlineSeconds},
		"ethereum":        {spec(0, "eth"), DefaultDeadlineSeconds},
		"explicit solana": {spec(200, "sol"), 200},
	} {
		r, err := Build(c.spec, nets)
		if err != nil || r.DeadlineSeconds != c.want {
			t.Errorf("%s: deadline %d, err %v; want %d", name, r.GetDeadlineSeconds(), err, c.want)
		}
	}
}

func TestValidateChecksTheFeePayerIsTheKeysAddress(t *testing.T) {
	tx := solTx()
	tx.GetSolana().FeePayer = b58of(1)
	tx.GetSolana().Instructions[0].Accounts = nil
	if err := Validate(req(tx), eddsaKey, nets); err == nil {
		t.Error("a fee payer that is not the Key's address was accepted")
	}
}

func TestSignedSolanaAssemblesAndIdentifiesByTheSignature(t *testing.T) {
	sig := bytes.Repeat([]byte{7}, 64)
	conv := key.ConversationID("eddsa", eddsaKey.PublicKey)
	r := rowOf(t, 1, 0xa1, time.Unix(10_000, 0), conv[:], req(solTx()))
	history := map[[32]byte]network.Network{solID: nets[2]}
	got, err := Signed([]cosl.Row{r}, r.MsgID, [][]byte{sig}, history)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := AssembleSolana(solTx().GetSolana(), sig)
	if len(got) != 1 || !bytes.Equal(got[0].Raw, want) || !bytes.Equal(got[0].ID, sig) || got[0].Network.Alias != "sol" {
		t.Fatalf("signed = %+v", got)
	}
}
