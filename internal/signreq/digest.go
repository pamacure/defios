package signreq

import (
	"bytes"
	"fmt"
	"math/big"
	"slices"
	"time"

	"golang.org/x/crypto/sha3"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/session"
	coslv1 "defios.com/proto/cosl/v1"
)

// EthereumDigest reconstructs the canonical unsigned EIP-1559 transaction
// (0x02 || rlp([chainId, nonce, maxPriorityFeePerGas, maxFeePerGas, gasLimit,
// to, value, data, accessList])) and its keccak256 Digest. The access list is
// always empty. Pure and deterministic, so every Node derives the same bytes.
func EthereumDigest(chainID uint64, in *coslv1.EthereumIntent) (unsigned []byte, digest [32]byte, err error) {
	fields, err := ethereumFields(chainID, in)
	if err != nil {
		return nil, digest, err
	}
	unsigned = append([]byte{0x02}, rlpList(fields)...)
	h := sha3.NewLegacyKeccak256()
	h.Write(unsigned)
	h.Sum(digest[:0])
	return unsigned, digest, nil
}

// ethereumFields is the RLP-encoded unsigned field list of the transaction.
func ethereumFields(chainID uint64, in *coslv1.EthereumIntent) ([][]byte, error) {
	value, err := amount(in.Value, true)
	if err != nil {
		return nil, fmt.Errorf("value %q: %v", in.Value, err)
	}
	maxFee, err := amount(in.MaxFeePerGas, false)
	if err != nil {
		return nil, fmt.Errorf("max_fee_per_gas %q: %v", in.MaxFeePerGas, err)
	}
	tip, err := amount(in.MaxPriorityFeePerGas, true)
	if err != nil {
		return nil, fmt.Errorf("max_priority_fee_per_gas %q: %v", in.MaxPriorityFeePerGas, err)
	}
	return [][]byte{
		rlpUint(new(big.Int).SetUint64(chainID)),
		rlpUint(new(big.Int).SetUint64(in.Nonce)),
		rlpUint(tip),
		rlpUint(maxFee),
		rlpUint(new(big.Int).SetUint64(in.GasLimit)),
		rlpString(in.To),
		rlpUint(value),
		rlpString(in.Data),
		rlpList(nil), // access list
	}, nil
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

// LookupMsgTypes are every row Lookup reads: the sign.request and its
// accepts, and the Session rows that mint its Key. A caller asks the replica
// for these only (cosl.Query.MsgTypes), so talk and round traffic are never
// read to look up a Sign Request (ADR 0014).
var LookupMsgTypes = slices.Concat([]uint32{cosl.TypeSignRequest, cosl.TypeSignAccept}, key.MsgTypes)

// MsgTypes are every row this package's readers fold (List, Inspect, Resolve,
// Signed and Lookup) and the Session fold shown beside them: LookupMsgTypes
// and the sign.broadcast receipts. A Sign Request verb asks the replica for
// these only, so talk and round traffic are never read (ADR 0014).
var MsgTypes = slices.Concat(LookupMsgTypes, []uint32{cosl.TypeSignBroadcast})

// Lookup reads the sign.request with msgID off rows and derives what signing
// it needs (the Key, its Digests, the deadline, who accepted): the Digests are
// flattened across its transactions in order, a keccak256 hash for Ethereum
// the compiled message for Solana, and one BIP-143 sighash per input for
// Bitcoin. Every Node reaches the same answer from the request alone: the
// chain id and Bitcoin network are its intents' (ADR 0023). A transaction
// that names no valid chain makes the request invalid.
func Lookup(rows []cosl.Row, msgID [32]byte) (session.SignInfo, error) {
	var row *cosl.Row
	for i := range rows {
		if rows[i].MsgType == cosl.TypeSignRequest && rows[i].MsgID == msgID {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		return session.SignInfo{}, ErrUnknown
	}
	req, err := decodeRow(*row)
	if err != nil {
		return session.SignInfo{}, err
	}
	k, err := keyOf(rows, row.ConversationID)
	if err != nil {
		return session.SignInfo{}, err
	}
	out := session.SignInfo{Curve: k.Curve, PublicKey: k.PublicKey, T: k.T, Conversation: row.ConversationID, Accepted: Accepts(rows, *row, req)}
	for i, tx := range req.Transactions {
		if err := ChainOf(tx).Validate(); err != nil {
			return session.SignInfo{}, fmt.Errorf("%w: transaction %d: %v", ErrInvalid, i, err)
		}
		var dgs [][]byte
		switch in := tx.Intent.(type) {
		case *coslv1.SignTransaction_Ethereum:
			_, h, err := EthereumDigest(in.Ethereum.ChainId, in.Ethereum)
			if err != nil {
				return session.SignInfo{}, fmt.Errorf("%w: transaction %d: %v", ErrInvalid, i, err)
			}
			dgs = [][]byte{h[:]}
		case *coslv1.SignTransaction_Solana:
			msg, err := SolanaMessage(in.Solana)
			if err != nil {
				return session.SignInfo{}, fmt.Errorf("%w: transaction %d: %v", ErrInvalid, i, err)
			}
			dgs = [][]byte{msg}
		case *coslv1.SignTransaction_Bitcoin:
			hs, err := BitcoinDigests(in.Bitcoin.Network, in.Bitcoin)
			if err != nil {
				return session.SignInfo{}, fmt.Errorf("%w: transaction %d: %v", ErrInvalid, i, err)
			}
			for _, h := range hs {
				dgs = append(dgs, h[:])
			}
		default:
			return session.SignInfo{}, fmt.Errorf("%w: transaction %d: no intent to sign", ErrInvalid, i)
		}
		out.Digests = append(out.Digests, dgs...)
	}
	window := min(req.DeadlineSeconds, MaxDeadlineSeconds)
	out.Deadline = time.Unix(0, row.CreatedAt).Add(time.Duration(window) * time.Second)
	return out, nil
}

// Request is the sign.request msgID off rows, and the Conversation of the
// Key it targets.
func Request(rows []cosl.Row, msgID [32]byte) (*coslv1.SignRequest, []byte, error) {
	for _, r := range rows {
		if r.MsgType == cosl.TypeSignRequest && r.MsgID == msgID {
			req, err := decodeRow(r)
			return req, r.ConversationID, err
		}
	}
	return nil, nil, ErrUnknown
}

// Resolve finds the one sign.request whose msg id starts with prefix.
func Resolve(rows []cosl.Row, prefix string) ([32]byte, error) {
	m, err := match(rows, prefix)
	if err != nil {
		return [32]byte{}, err
	}
	return m.MsgID, nil
}

// keyOf is the Key whose Conversation is conversation.
func keyOf(rows []cosl.Row, conversation []byte) (key.Key, error) {
	keys, err := key.List(rows)
	if err != nil {
		return key.Key{}, err
	}
	for _, c := range keys {
		if conv := key.ConversationID(c.Curve, c.PublicKey); bytes.Equal(conv[:], conversation) {
			return c, nil
		}
	}
	return key.Key{}, key.ErrUnknown
}
