package tssl

import (
	"errors"
	"fmt"

	"github.com/bnb-chain/tss-lib/v4/crypto"
	"github.com/btcsuite/btcd/btcec/v2"
)

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
