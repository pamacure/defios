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
	Network         network.Network
	NetworkConfigID [32]byte
	Raw             []byte
	Hash            [32]byte
}

// Signed assembles every transaction of the sign.request msgID from sigs, one
// signature per transaction in order (as a Session completes with them).
// history resolves each transaction's Network. Only Ethereum can be assembled yet.
func Signed(rows []cosl.Row, msgID [32]byte, sigs [][]byte, history map[[32]byte]network.Network) ([]SignedTx, error) {
	row, err := match(rows, fmt.Sprintf("%x", msgID))
	if err != nil {
		return nil, err
	}
	req, err := decodeRow(row)
	if err != nil {
		return nil, err
	}
	if len(sigs) != len(req.Transactions) {
		return nil, invalidf("%d signatures for %d transactions", len(sigs), len(req.Transactions))
	}
	out := make([]SignedTx, len(req.Transactions))
	for i, tx := range req.Transactions {
		var id [32]byte
		copy(id[:], tx.NetworkConfigId)
		n, known := history[id]
		in, isEth := tx.Intent.(*coslv1.SignTransaction_Ethereum)
		switch {
		case !known:
			return nil, invalidf("transaction %d: unknown network config", i)
		case !isEth:
			return nil, invalidf("transaction %d: only ethereum can be broadcast", i)
		}
		raw, hash, err := AssembleEthereum(n.ChainID, in.Ethereum, sigs[i])
		if err != nil {
			return nil, invalidf("transaction %d: %v", i, err)
		}
		out[i] = SignedTx{Network: n, NetworkConfigID: id, Raw: raw, Hash: hash}
	}
	return out, nil
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
