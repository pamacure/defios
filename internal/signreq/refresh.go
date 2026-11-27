package signreq

import (
	"bytes"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/base58"
	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

// Refresh is the sign_refresh of the first session.start for the request
// msgID in rows, which must hold the session.start rows; nil when no Session
// has started, or its start refreshed nothing. The first start is the one
// that counts: a later one is a duplicate every Node aborts.
func Refresh(rows []cosl.Row, msgID [32]byte) *coslv1.SignRefresh {
	for _, r := range rows {
		if r.MsgType != cosl.TypeSessionStart {
			continue
		}
		msg, err := decodePayload(r)
		if err != nil {
			continue
		}
		if st := msg.(*coslv1.SessionStart); bytes.Equal(st.SignRequestId, msgID[:]) {
			return st.SignRefresh
		}
	}
	return nil
}

// Refreshed is req with ref's fields in place of its own, the transactions
// its Session signs (docs/adr/0045-a-signing-session-refreshes-broadcasts-and-confirms.md).
// req itself is not changed. A refresh that names a transaction twice or out
// of range, gives a field of another chain type, or a blockhash that is not
// a base58 hash, is ErrInvalid.
func Refreshed(req *coslv1.SignRequest, ref *coslv1.SignRefresh) (*coslv1.SignRequest, error) {
	if len(ref.GetTransactions()) == 0 {
		return req, nil
	}
	out := proto.Clone(req).(*coslv1.SignRequest)
	seen := map[uint32]bool{}
	for _, r := range ref.Transactions {
		i := r.TxIndex
		if int(i) >= len(out.Transactions) || seen[i] {
			return nil, invalidf("refresh names transaction %d twice or out of range", i)
		}
		seen[i] = true
		switch f := r.Field.(type) {
		case *coslv1.RefreshedTx_Nonce:
			eth := out.Transactions[i].GetEthereum()
			if eth == nil {
				return nil, invalidf("refresh gives a nonce to transaction %d, which is not an ethereum transaction", i)
			}
			eth.Nonce = f.Nonce
		case *coslv1.RefreshedTx_RecentBlockhash:
			sol := out.Transactions[i].GetSolana()
			if sol == nil {
				return nil, invalidf("refresh gives a blockhash to transaction %d, which is not a solana transaction", i)
			}
			if _, err := base58.DecodeN(f.RecentBlockhash, 32); err != nil {
				return nil, invalidf("refresh of transaction %d: recent_blockhash: %v", i, err)
			}
			sol.RecentBlockhash = f.RecentBlockhash
		default:
			return nil, invalidf("refresh of transaction %d gives no field", i)
		}
	}
	return out, nil
}

// signedAs is the request at row as its Session signs it: refreshed by its
// first start, if rows hold one.
func signedAs(rows []cosl.Row, row cosl.Row, req *coslv1.SignRequest) (*coslv1.SignRequest, error) {
	return Refreshed(req, Refresh(rows, row.MsgID))
}

// SignedRequest is the sign.request msgID off rows as its Session signs it
// (refreshed by its first start), and the Conversation of the Key it
// targets. rows must hold the session.start rows.
func SignedRequest(rows []cosl.Row, msgID [32]byte) (*coslv1.SignRequest, []byte, error) {
	for _, r := range rows {
		if r.MsgType == cosl.TypeSignRequest && r.MsgID == msgID {
			req, err := decodeRow(r)
			if err != nil {
				return nil, nil, err
			}
			req, err = signedAs(rows, r, req)
			return req, r.ConversationID, err
		}
	}
	return nil, nil, ErrUnknown
}
