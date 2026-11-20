package node

import (
	"bytes"
	"testing"

	"defios.com/internal/base58"
	"defios.com/internal/config"
	"defios.com/internal/key"
)

func TestKeyAddressIsTheChainAddressOfItsCurve(t *testing.T) {
	pub := bytes.Repeat([]byte{0x66}, 32)
	eddsa := keyOf(key.Key{Curve: "eddsa", PublicKey: pub}, nil, config.KeyAlias{}, nil)
	if want := base58.Encode(pub); eddsa.Address != want {
		t.Fatalf("EdDSA Address = %q, want the Solana address %q", eddsa.Address, want)
	}

	// secp256k1 generator point, compressed; its address is well known.
	g := []byte{0x02, 0x79, 0xbe, 0x66, 0x7e, 0xf9, 0xdc, 0xbb, 0xac, 0x55, 0xa0, 0x62, 0x95, 0xce, 0x87, 0x0b, 0x07,
		0x02, 0x9b, 0xfc, 0xdb, 0x2d, 0xce, 0x28, 0xd9, 0x59, 0xf2, 0x81, 0x5b, 0x16, 0xf8, 0x17, 0x98}
	ecdsa := keyOf(key.Key{Curve: "ecdsa", PublicKey: g}, nil, config.KeyAlias{}, nil)
	if want, _ := key.EthereumAddress(g); ecdsa.Address != want || want == "" {
		t.Fatalf("ECDSA Address = %q, want the Ethereum address %q", ecdsa.Address, want)
	}
}
