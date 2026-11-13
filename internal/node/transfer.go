package node

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"

	"defios.com/internal/base58"
	"defios.com/internal/network"
)

// Transfer is a payment to propose, as a person states it: on Network, one
// of the Key's Conversation's Networks by alias, To an address, of Amount
// in the chain's least unit (wei, lamports), with Data, Ethereum call data
// as hex. A Bitcoin payment needs its inputs chosen, so it is no Transfer:
// its intent is written out.
type Transfer struct {
	Network, To, Amount, Data string
}

// systemProgram is Solana's System Program; its instruction 2 is a
// transfer of lamports.
const systemProgram = "11111111111111111111111111111111"

// TransferTx is the transaction that pays tr from the Key keyQuery names in
// groupAlias, in the form ProposeSignRequest and FillTransactions take:
// what the chain needs besides (nonce, fees, a blockhash) is left for
// FillTransactions. It reads only this Node's config and Keys.
func (h *Host) TransferTx(ctx context.Context, groupAlias, keyQuery string, tr Transfer) (TxSpec, error) {
	k, err := h.InspectKey(ctx, groupAlias, keyQuery, nil)
	if err != nil {
		return TxSpec{}, err
	}
	nets, err := h.Networks(ctx, groupAlias, k.ConversationID)
	if err != nil {
		return TxSpec{}, err
	}
	for _, n := range nets {
		if n.Alias == tr.Network {
			pub, err := hex.DecodeString(k.PublicKey)
			if err != nil {
				return TxSpec{}, err
			}
			return transferTx(n.Type, pub, tr)
		}
	}
	return TxSpec{}, network.ErrUnknown
}

// transferTx is tr on a Network of type typ, from the Key whose public key
// is pub.
func transferTx(typ string, pub []byte, tr Transfer) (TxSpec, error) {
	tx := TxSpec{Network: tr.Network}
	amount, ok := new(big.Int).SetString(strings.TrimSpace(tr.Amount), 10)
	if !ok || amount.Sign() < 0 {
		return tx, invalidf("the amount must be a whole number of the chain's least unit")
	}
	var intent map[string]any
	switch typ {
	case network.Ethereum:
		to, err := hexBytes(tr.To)
		if err != nil || len(to) != 20 {
			return tx, invalidf("the recipient must be a 0x address of 20 bytes")
		}
		intent = map[string]any{"to": base64.StdEncoding.EncodeToString(to), "value": amount.String()}
		if strings.TrimSpace(tr.Data) != "" {
			data, err := hexBytes(tr.Data)
			if err != nil {
				return tx, invalidf("the data must be hex")
			}
			intent["data"] = base64.StdEncoding.EncodeToString(data)
		}
	case network.Solana:
		if len(pub) != 32 {
			return tx, invalidf("this Key does not sign Solana transactions")
		}
		to, err := base58.DecodeN(strings.TrimSpace(tr.To), 32)
		if err != nil {
			return tx, invalidf("the recipient must be a Solana address")
		}
		if !amount.IsUint64() {
			return tx, invalidf("the amount is too large")
		}
		data := binary.LittleEndian.AppendUint32(nil, 2)
		data = binary.LittleEndian.AppendUint64(data, amount.Uint64())
		intent = map[string]any{"instructions": []any{map[string]any{
			"programId": systemProgram,
			"accounts": []any{
				map[string]any{"pubkey": base58.Encode(pub), "signer": true, "writable": true},
				map[string]any{"pubkey": base58.Encode(to), "writable": true},
			},
			"data": base64.StdEncoding.EncodeToString(data),
		}}}
	default:
		return tx, invalidf("a %s payment needs its inputs chosen: write its intent out", typ)
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		return tx, err
	}
	if typ == network.Ethereum {
		tx.Ethereum = raw
	} else {
		tx.Solana = raw
	}
	return tx, nil
}

func hexBytes(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	return hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X"))
}
