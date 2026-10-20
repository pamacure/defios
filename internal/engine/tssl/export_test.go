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

func TestCanonicalEd25519IsRFC8032(t *testing.T) {
	// Coordinates are the RFC 8032 §5.1 base point and 5·G. The encodings
	// are independent literals: little-endian Y, with X's least bit in the
	// high bit of the last byte. G's X is even; 5·G's X is odd.
	cases := []struct {
		name string
		x, y string
		want string
	}{
		{
			name: "base point",
			x:    "15112221349535400772501151409588531511454012693041857206046113283949847762202",
			y:    "46316835694926478169428394003475163141307993866256225615783033603165251855960",
			want: "5866666666666666666666666666666666666666666666666666666666666666",
		},
		{
			name: "odd X",
			x:    "33467004535436536005251147249499675200073690106659565782908757308821616914995",
			y:    "43097193783671926753355113395909008640284023746042808659097434958891230611693",
			want: "edc876d6831fd2105d0b4389ca2e283166469289146e2ce06faefe98b22548df",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, ok := new(big.Int).SetString(tc.x, 10)
			if !ok {
				t.Fatal("x")
			}
			y, ok := new(big.Int).SetString(tc.y, 10)
			if !ok {
				t.Fatal("y")
			}
			point, err := crypto.NewECPoint(tss.Edwards(), x, y)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tssl.CanonicalEd25519(point)
			if err != nil {
				t.Fatal(err)
			}
			if hex := encode(got); hex != tc.want {
				t.Fatalf("canonical = %s, want %s", hex, tc.want)
			}
			if len(got) != 32 {
				t.Fatalf("len = %d", len(got))
			}
		})
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
