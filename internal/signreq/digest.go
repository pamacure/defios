package signreq

import (
	"bytes"
	"fmt"
	"math/big"
	"time"

	"golang.org/x/crypto/sha3"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/network"
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

// Signable is what signing a sign.request needs: the Key, the Digests every
// Node derives from the request, and when it lapses.
type Signable struct {
	Curve     string
	PublicKey []byte
	T         int // the Key's t
	Digests   [][32]byte
	Deadline  time.Time
}

// Lookup reads the sign.request with msgID off rows and derives its Digests,
// flattened across its transactions in order. Only Ethereum transactions can
// be signed yet. Every Node reaches the same answer from the same log.
func Lookup(rows []cosl.Row, msgID [32]byte) (Signable, error) {
	var row *cosl.Row
	for i := range rows {
		if rows[i].MsgType == cosl.TypeSignRequest && rows[i].MsgID == msgID {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		return Signable{}, ErrUnknown
	}
	req, err := decodeRow(*row)
	if err != nil {
		return Signable{}, err
	}
	keys, err := key.List(rows)
	if err != nil {
		return Signable{}, err
	}
	var k key.Key
	found := false
	for _, c := range keys {
		if conv := key.ConversationID(c.Curve, c.PublicKey); bytes.Equal(conv[:], row.ConversationID) {
			k, found = c, true
			break
		}
	}
	if !found {
		return Signable{}, key.ErrUnknown
	}
	history, err := network.History(rows)
	if err != nil {
		return Signable{}, err
	}
	out := Signable{Curve: k.Curve, PublicKey: k.PublicKey, T: k.T}
	for i, tx := range req.Transactions {
		var id [32]byte
		copy(id[:], tx.NetworkConfigId)
		n, known := history[id]
		in, isEth := tx.Intent.(*coslv1.SignTransaction_Ethereum)
		switch {
		case !known:
			return Signable{}, fmt.Errorf("%w: transaction %d: unknown network config", ErrInvalid, i)
		case !isEth:
			return Signable{}, fmt.Errorf("%w: transaction %d: only ethereum can be signed", ErrInvalid, i)
		}
		_, dg, err := EthereumDigest(n.ChainID, in.Ethereum)
		if err != nil {
			return Signable{}, fmt.Errorf("%w: transaction %d: %v", ErrInvalid, i, err)
		}
		out.Digests = append(out.Digests, dg)
	}
	window := min(req.DeadlineSeconds, MaxDeadlineSeconds)
	out.Deadline = time.Unix(0, row.CreatedAt).Add(time.Duration(window) * time.Second)
	return out, nil
}

// Resolve finds the one sign.request whose msg id starts with prefix.
func Resolve(rows []cosl.Row, prefix string) ([32]byte, error) {
	m, err := match(rows, prefix)
	if err != nil {
		return [32]byte{}, err
	}
	return m.MsgID, nil
}
