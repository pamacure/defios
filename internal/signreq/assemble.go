package signreq

import (
	"fmt"
	"math/big"

	"golang.org/x/crypto/sha3"

	"defios.com/internal/cosl"
	"defios.com/internal/network"
	coslv1 "defios.com/proto/cosl/v1"
)

// SignedTx is one transaction of a Sign Request, assembled and ready to
// broadcast to its Network.
type SignedTx struct {
	Index           uint32 // position in the request's transactions
	Network         network.Network
	NetworkConfigID [32]byte
	Raw             []byte
	// ID names the transaction on its Network: the keccak256 hash of an
	// Ethereum transaction, the first signature of a Solana one, the txid of
	// a Bitcoin one (display order).
	ID []byte
}

// Signed assembles every transaction of the sign.request msgID from sigs, one
// signature per Digest, flattened across the transactions in order (as a
// Session completes with them): one per Ethereum or Solana transaction, one
// per input of a Bitcoin one. history resolves each transaction's Network.
func Signed(rows []cosl.Row, msgID [32]byte, sigs [][]byte, history map[[32]byte]network.Network) ([]SignedTx, error) {
	row, err := match(rows, fmt.Sprintf("%x", msgID))
	if err != nil {
		return nil, err
	}
	req, err := decodeRow(row)
	if err != nil {
		return nil, err
	}
	need := 0
	for _, tx := range req.Transactions {
		need += digestCount(tx)
	}
	if len(sigs) != need {
		return nil, invalidf("%d signatures for %d digests", len(sigs), need)
	}
	out := make([]SignedTx, len(req.Transactions))
	next := 0 // the first of sigs this transaction signs
	for i, tx := range req.Transactions {
		mine := sigs[next : next+digestCount(tx)]
		next += len(mine)
		var id [32]byte
		copy(id[:], tx.NetworkConfigId)
		n, known := history[id]
		if !known {
			return nil, invalidf("transaction %d: unknown network config", i)
		}
		stx := SignedTx{Index: uint32(i), Network: n, NetworkConfigID: id}
		switch in := tx.Intent.(type) {
		case *coslv1.SignTransaction_Ethereum:
			raw, hash, err := AssembleEthereum(n.ChainID, in.Ethereum, mine[0])
			if err != nil {
				return nil, invalidf("transaction %d: %v", i, err)
			}
			stx.Raw, stx.ID = raw, hash[:]
		case *coslv1.SignTransaction_Solana:
			raw, err := AssembleSolana(in.Solana, mine[0])
			if err != nil {
				return nil, invalidf("transaction %d: %v", i, err)
			}
			stx.Raw, stx.ID = raw, mine[0]
		case *coslv1.SignTransaction_Bitcoin:
			k, err := keyOf(rows, row.ConversationID)
			if err != nil {
				return nil, err
			}
			raw, txid, err := AssembleBitcoin(n.BitcoinNetwork, k.PublicKey, in.Bitcoin, mine)
			if err != nil {
				return nil, invalidf("transaction %d: %v", i, err)
			}
			stx.Raw, stx.ID = raw, txid[:]
		default:
			return nil, invalidf("transaction %d: no intent to broadcast", i)
		}
		out[i] = stx
	}
	return out, nil
}

// digestCount is how many signatures tx needs: one per Bitcoin input, else one.
func digestCount(tx *coslv1.SignTransaction) int {
	if in := tx.GetBitcoin(); in != nil {
		return len(in.Inputs)
	}
	return 1
}

// AssembleEthereum is the broadcastable EIP-1559 transaction for intent in on
// chainID, signed by sig (r||s||v, v the recovery id 0/1, as a Session
// produces it), and its hash. It re-derives the unsigned fields exactly as
// EthereumDigest does, so the signature covers them. Identical inputs give
// identical bytes, which is what makes a second broadcast harmless.
func AssembleEthereum(chainID uint64, in *coslv1.EthereumIntent, sig []byte) (raw []byte, hash [32]byte, err error) {
	if len(sig) != 65 || sig[64] > 1 {
		return nil, hash, fmt.Errorf("signature must be 65 bytes r||s||v with v 0 or 1")
	}
	body, err := ethereumFields(chainID, in)
	if err != nil {
		return nil, hash, err
	}
	// A signed tx is the unsigned field list plus y_parity, r, s.
	body = append(body,
		rlpUint(new(big.Int).SetUint64(uint64(sig[64]))),
		rlpUint(new(big.Int).SetBytes(sig[:32])),
		rlpUint(new(big.Int).SetBytes(sig[32:64])))
	raw = append([]byte{0x02}, rlpList(body)...)
	h := sha3.NewLegacyKeccak256()
	h.Write(raw)
	h.Sum(hash[:0])
	return raw, hash, nil
}
