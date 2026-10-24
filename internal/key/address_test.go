package key

import (
	"encoding/hex"
	"testing"
)

// Private key 1 is the well-known vector: its compressed public key is the
// secp256k1 generator, and its Ethereum address is published widely.
func TestEthereumAddress(t *testing.T) {
	pub, _ := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	got, err := EthereumAddress(pub)
	if err != nil {
		t.Fatal(err)
	}
	if want := "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"; got != want {
		t.Errorf("address = %s, want %s", got, want)
	}
	if _, err := EthereumAddress([]byte{1, 2, 3}); err == nil {
		t.Error("a malformed public key must be rejected")
	}
}
