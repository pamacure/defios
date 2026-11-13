package node

import (
	"encoding/json"
	"errors"
	"testing"

	"defios.com/internal/base58"
)

func TestAnEthereumTransferIsAnIntentToTheRecipient(t *testing.T) {
	tx, err := transferTx("ethereum", nil, Transfer{Network: "eth", To: "0x0102030405060708090a0b0c0d0e0f1011121314", Amount: "1000", Data: "0xdeadbeef"})
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]any
	json.Unmarshal(tx.Ethereum, &in)
	// protojson: bytes are base64.
	if tx.Network != "eth" || in["to"] != "AQIDBAUGBwgJCgsMDQ4PEBESExQ=" || in["value"] != "1000" || in["data"] != "3q2+7w==" || len(in) != 3 {
		t.Fatalf("tx = %+v, intent %v", tx, in)
	}
}

func TestASolanaTransferIsASystemProgramInstructionFromTheKey(t *testing.T) {
	pub := make([]byte, 32)
	pub[31] = 7
	tx, err := transferTx("solana", pub, Transfer{Network: "sol", To: "11111111111111111111111111111112", Amount: "5000"})
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		Instructions []struct {
			ProgramID string `json:"programId"`
			Accounts  []struct {
				Pubkey           string
				Signer, Writable bool
			}
			Data string
		}
	}
	json.Unmarshal(tx.Solana, &in)
	if len(in.Instructions) != 1 {
		t.Fatalf("intent: %s", tx.Solana)
	}
	ix := in.Instructions[0]
	// Transfer is instruction 2, then the lamports (5000 = 0x1388), little-endian.
	if ix.ProgramID != "11111111111111111111111111111111" || ix.Data != "AgAAAIgTAAAAAAAA" || len(ix.Accounts) != 2 {
		t.Fatalf("instruction: %+v", ix)
	}
	if a := ix.Accounts[0]; !a.Signer || !a.Writable || a.Pubkey != base58.Encode(pub) {
		t.Fatalf("from: %+v", a)
	}
	if a := ix.Accounts[1]; a.Signer || !a.Writable || a.Pubkey != "11111111111111111111111111111112" {
		t.Fatalf("to: %+v", a)
	}
}

func TestATransferWithBadFieldsIsInvalid(t *testing.T) {
	key := make([]byte, 32)
	for _, c := range []struct {
		typ string
		pub []byte
		tr  Transfer
	}{
		{"ethereum", nil, Transfer{To: "0x12", Amount: "1"}},
		{"ethereum", nil, Transfer{To: "0x0102030405060708090a0b0c0d0e0f1011121314", Amount: "ten"}},
		{"ethereum", nil, Transfer{To: "0x0102030405060708090a0b0c0d0e0f1011121314", Amount: "-1"}},
		{"ethereum", nil, Transfer{To: "0x0102030405060708090a0b0c0d0e0f1011121314", Amount: "1", Data: "zz"}},
		{"solana", key, Transfer{To: "not base58!", Amount: "1"}},
		{"solana", key, Transfer{To: "11111111111111111111111111111112", Amount: "18446744073709551616"}},
		{"solana", make([]byte, 33), Transfer{To: "11111111111111111111111111111112", Amount: "1"}},
		{"bitcoin", nil, Transfer{To: "bc1q", Amount: "1"}},
	} {
		if _, err := transferTx(c.typ, c.pub, c.tr); !errors.As(err, new(*InvalidError)) {
			t.Errorf("%s %+v: %v; want InvalidError", c.typ, c.tr, err)
		}
	}
}
