package signreq

import (
	"fmt"
	"math/big"

	"golang.org/x/crypto/sha3"

	coslv1 "defios.com/proto/cosl/v1"
)

// EthereumDigest reconstructs the canonical unsigned EIP-1559 transaction
// (0x02 || rlp([chainId, nonce, maxPriorityFeePerGas, maxFeePerGas, gasLimit,
// to, value, data, accessList])) and its keccak256 Digest. The access list is
// always empty. Pure and deterministic, so every Node derives the same bytes.
func EthereumDigest(chainID uint64, in *coslv1.EthereumIntent) (unsigned []byte, digest [32]byte, err error) {
	value, err := amount(in.Value, true)
	if err != nil {
		return nil, digest, fmt.Errorf("value %q: %v", in.Value, err)
	}
	maxFee, err := amount(in.MaxFeePerGas, false)
	if err != nil {
		return nil, digest, fmt.Errorf("max_fee_per_gas %q: %v", in.MaxFeePerGas, err)
	}
	tip, err := amount(in.MaxPriorityFeePerGas, true)
	if err != nil {
		return nil, digest, fmt.Errorf("max_priority_fee_per_gas %q: %v", in.MaxPriorityFeePerGas, err)
	}
	fields := [][]byte{
		rlpUint(new(big.Int).SetUint64(chainID)),
		rlpUint(new(big.Int).SetUint64(in.Nonce)),
		rlpUint(tip),
		rlpUint(maxFee),
		rlpUint(new(big.Int).SetUint64(in.GasLimit)),
		rlpString(in.To),
		rlpUint(value),
		rlpString(in.Data),
		rlpList(nil), // access list
	}
	unsigned = append([]byte{0x02}, rlpList(fields)...)
	h := sha3.NewLegacyKeccak256()
	h.Write(unsigned)
	h.Sum(digest[:0])
	return unsigned, digest, nil
}

// rlpUint encodes a non-negative integer as a big-endian string with no leading zeros.
func rlpUint(v *big.Int) []byte { return rlpString(v.Bytes()) }

func rlpString(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return []byte{b[0]}
	}
	return append(rlpLength(len(b), 0x80), b...)
}

// rlpList wraps already-encoded items.
func rlpList(items [][]byte) []byte {
	var body []byte
	for _, it := range items {
		body = append(body, it...)
	}
	return append(rlpLength(len(body), 0xc0), body...)
}

func rlpLength(n int, offset byte) []byte {
	if n < 56 {
		return []byte{offset + byte(n)}
	}
	var be []byte
	for ; n > 0; n >>= 8 {
		be = append([]byte{byte(n)}, be...)
	}
	return append([]byte{offset + 55 + byte(len(be))}, be...)
}
