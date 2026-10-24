// Package base58 is the Bitcoin-alphabet base58 Solana writes keys, hashes and
// signatures in.
package base58

import (
	"errors"
	"math/big"
	"strings"
)

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var radix = big.NewInt(58)

// Encode is b in base58; each leading zero byte is a leading '1'.
func Encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	var out []byte
	for mod := new(big.Int); n.Sign() > 0; {
		n.DivMod(n, radix, mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// Decode is the bytes s encodes.
func Decode(s string) ([]byte, error) {
	n := new(big.Int)
	for _, c := range s {
		d := strings.IndexRune(alphabet, c)
		if d < 0 {
			return nil, errors.New("not base58")
		}
		n.Mul(n, radix).Add(n, big.NewInt(int64(d)))
	}
	out := n.Bytes()
	zeros := 0
	for zeros < len(s) && s[zeros] == alphabet[0] {
		zeros++
	}
	return append(make([]byte, zeros), out...), nil
}

// DecodeN is Decode that insists on exactly size bytes.
func DecodeN(s string, size int) ([]byte, error) {
	b, err := Decode(s)
	if err != nil {
		return nil, err
	}
	if len(b) != size {
		return nil, errors.New("wrong length")
	}
	return b, nil
}
