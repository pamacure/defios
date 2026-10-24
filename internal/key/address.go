package key

import (
	"encoding/hex"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"golang.org/x/crypto/sha3"
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
	addr := hex.EncodeToString(h.Sum(nil)[12:])
	h.Reset()
	h.Write([]byte(addr))
	sum := hex.EncodeToString(h.Sum(nil))
	var out strings.Builder
	out.WriteString("0x")
	for i, c := range addr {
		if c >= 'a' && sum[i] >= '8' {
			c -= 'a' - 'A'
		}
		out.WriteRune(c)
	}
	return out.String(), nil
}
