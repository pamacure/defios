package tssl_test

import (
	"math/big"
	"testing"

	"github.com/bnb-chain/tss-lib/v4/crypto"
	"github.com/bnb-chain/tss-lib/v4/tss"

	"defios.com/internal/engine/tssl"
)

// secp256k1 generator, compressed SEC1. Independent of btcec's serializer.
const secp256k1G = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

func TestCanonicalECDSAIsSEC1Compressed(t *testing.T) {
	gx, ok := new(big.Int).SetString("79BE667EF9DCBBAC55A06295CE870B07029BFCDB2DCE28D959F2815B16F81798", 16)
	if !ok {
		t.Fatal("Gx")
	}
	gy, ok := new(big.Int).SetString("483ADA7726A3C4655DA4FBFC0E1108A8FD17B448A68554199C47D08FFB10D4B8", 16)
	if !ok {
		t.Fatal("Gy")
	}
	point, err := crypto.NewECPoint(tss.S256(), gx, gy)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tssl.CanonicalECDSA(point)
	if err != nil {
		t.Fatal(err)
	}
	if hex := encode(got); hex != secp256k1G {
		t.Fatalf("canonical = %s, want %s", hex, secp256k1G)
	}
	if len(got) != 33 || (got[0] != 0x02 && got[0] != 0x03) {
		t.Fatalf("bytes = %x", got)
	}
}

func encode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0x0f]
	}
	return string(out)
}
