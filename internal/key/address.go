package key

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"golang.org/x/crypto/sha3"

	"defios.com/internal/evmabi"
)

// EthereumAddress is the EIP-55 checksummed address of a secp256k1 public key:
// the last 20 bytes of the keccak256 of its uncompressed point.
func EthereumAddress(pub []byte) (string, error) {
	pk, err := btcec.ParsePubKey(pub)
	if err != nil {
		return "", err
	}
	h := sha3.NewLegacyKeccak256()
	h.Write(pk.SerializeUncompressed()[1:])
	return evmabi.Checksum(h.Sum(nil)[12:]), nil
}
