package tssl

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/bnb-chain/tss-lib/v4/crypto"
	"github.com/bnb-chain/tss-lib/v4/tss"
	"github.com/btcsuite/btcd/btcec/v2"
)

// ed25519P is the Ed25519 field prime, 2^255 − 19.
var ed25519P = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))

// CanonicalECDSA is the 33-byte SEC1 compressed form of a tss-lib point
// (docs/spec/mvp.md §2.4). btcec does the serialize so the wire bytes match
// the rest of the secp256k1 world.
func CanonicalECDSA(point *crypto.ECPoint) ([]byte, error) {
	if point == nil || point.X() == nil || point.Y() == nil {
		return nil, errors.New("ecdsa: public key is missing")
	}
	xb, yb := point.X().Bytes(), point.Y().Bytes()
	if len(xb) > 32 || len(yb) > 32 {
		return nil, errors.New("ecdsa: public key coordinate is too wide")
	}
	uncompressed := make([]byte, 65)
	uncompressed[0] = 0x04
	copy(uncompressed[1+32-len(xb):33], xb)
	copy(uncompressed[33+32-len(yb):], yb)
	pub, err := btcec.ParsePubKey(uncompressed)
	if err != nil {
		return nil, fmt.Errorf("ecdsa: %w", err)
	}
	return pub.SerializeCompressed(), nil
}

// CanonicalEd25519 is the 32-byte RFC 8032 encoding of a tss-lib Edwards
// point (docs/spec/mvp.md §2.4): little-endian Y, with the least bit of X
// in the high bit of the last byte. tss-lib has no encoder for this.
func CanonicalEd25519(point *crypto.ECPoint) ([]byte, error) {
	if point == nil || point.X() == nil || point.Y() == nil {
		return nil, errors.New("eddsa: public key is missing")
	}
	x := new(big.Int).Mod(point.X(), ed25519P)
	y := new(big.Int).Mod(point.Y(), ed25519P)
	if !tss.Edwards().IsOnCurve(x, y) {
		return nil, errors.New("eddsa: public key is not on the curve")
	}
	out := make([]byte, 32)
	yb := y.Bytes()
	for i := range yb {
		out[i] = yb[len(yb)-1-i]
	}
	if x.Bit(0) == 1 {
		out[31] |= 0x80
	}
	return out, nil
}
