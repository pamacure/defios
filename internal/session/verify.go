package session

import (
	"bytes"
	"math/big"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

var halfN = new(big.Int).Rsh(btcec.S256().N, 1)

// VerifyECDSA reports whether sig, 65 bytes of r||s||v, is a signature of
// digest that recovers to the compressed secp256k1 publicKey. This is the
// shape an Ethereum transaction carries, so a signature that passes here is
// EOA-indistinguishable.
func VerifyECDSA(publicKey []byte, digest [32]byte, sig []byte) bool {
	if len(sig) != 65 || sig[64] > 1 {
		return false
	}
	r, sv := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:64])
	if r.Sign() == 0 || sv.Sign() == 0 || sv.Cmp(halfN) > 0 { // EIP-2: high-s is not broadcastable
		return false
	}
	compact := append([]byte{27 + 4 + sig[64]}, sig[:64]...)
	got, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		return false
	}
	return bytes.Equal(got.SerializeCompressed(), publicKey)
}
