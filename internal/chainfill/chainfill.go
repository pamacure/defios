// Package chainfill completes partial transactions from this Node's Networks
// (ADR 0029): what chain_fill and `sign fill` do. A transaction is in the
// form `sign propose --transactions` reads, and a field is filled only when
// its key is absent from the transaction's JSON; a present field is the
// caller's, whatever its value (nonce 0 is a nonce). It is read-only: it
// reads the chain and never writes to it or the log.
package chainfill

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode"

	"google.golang.org/protobuf/encoding/protojson"

	"defios.com/internal/base58"
	"defios.com/internal/ethrpc"
	"defios.com/internal/jsonrpc"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/signreq"
	"defios.com/internal/solrpc"
	coslv1 "defios.com/proto/cosl/v1"
)

// ErrBitcoin is a Bitcoin transaction, which chain_fill cannot complete.
var ErrBitcoin = errors.New("a bitcoin transaction cannot be filled: choosing inputs (UTXOs) and a fee needs an indexer, which a bitcoind endpoint does not keep; that is a Plugin's job")

// BlockhashWarning is the warning on a Solana transaction whose recentBlockhash
// was filled.
const BlockhashWarning = "recentBlockhash expires in about 60–90 seconds: propose promptly"

// Endpoints are the RPC URLs of a Network, in failover order, each "$NAME"
// resolved.
type Endpoints func(network.Network) ([]string, error)

// Result is the completed transactions, in the input's order and form, and
// a note on each.
type Result struct {
	Transactions []signreq.TxSpec `json:"transactions"`
	Notes        []Note           `json:"notes"`
}

// Note says what was filled in one transaction, and from where.
type Note struct {
	Index   int    `json:"index"`
	Network string `json:"network"` // this Node's alias
	Chain   string `json:"chain"`   // e.g. "ethereum chain 1"
	// Endpoint is the endpoint whose chain identity was checked and which
	// was read, its path and query dropped; empty when none was needed.
	Endpoint string   `json:"endpoint,omitempty"`
	Filled   []string `json:"filled"`
	Warning  string   `json:"warning,omitempty"`
}

// tx is one input transaction, parsed.
type tx struct {
	index  int
	net    network.Network
	fields map[string]json.RawMessage // the intent's JSON object
	eth    *coslv1.EthereumIntent
	sol    *coslv1.SolanaIntent
	note   Note
}

func (t *tx) errorf(format string, args ...any) error {
	return fmt.Errorf("transaction %d (%s): %s", t.index, t.net.Chain(), fmt.Sprintf(format, args...))
}

func (t *tx) wrap(err error) error {
	return fmt.Errorf("transaction %d (%s): %w", t.index, t.net.Chain(), err)
}

// has is whether the intent's JSON gives field, in its JSON (lowerCamel) or
// proto (snake_case) spelling, as protojson reads either. A null is no
// value, as protojson reads it, so it is absent; filling drops it.
func (t *tx) has(field string) bool {
	for _, k := range []string{field, snakeCase(field)} {
		if v, ok := t.fields[k]; ok && string(bytes.TrimSpace(v)) != "null" {
			return true
		}
	}
	return false
}

// set fills field with v, as protojson writes it.
func (t *tx) set(field string, v any) {
	raw, _ := json.Marshal(v)
	delete(t.fields, snakeCase(field))
	t.fields[field] = raw
	t.note.Filled = append(t.note.Filled, field)
}

func snakeCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('_')
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Fill completes txs for Key k from Networks nets, reaching each Network
// through endpoints. Each transaction's network alias picks the Network and
// its type the rules; the sender is k's address on that chain. Several
// Ethereum transactions for one chain get consecutive nonces in input order.
// An error names the transaction and its chain, and nothing is returned
// with it.
func Fill(ctx context.Context, k key.Key, nets []network.Network, txs []signreq.TxSpec, endpoints Endpoints) (Result, error) {
	if len(txs) == 0 {
		return Result{}, fmt.Errorf("%w: at least one transaction is required", signreq.ErrInvalid)
	}
	parsed := make([]*tx, len(txs))
	for i, spec := range txs {
		t, err := parse(i, spec, nets, k)
		if err != nil {
			return Result{}, err
		}
		parsed[i] = t
	}
	// Each chain's transactions are filled together, so their nonces run on.
	var order []string
	groups := map[string][]*tx{}
	for _, t := range parsed {
		a := t.net.Alias
		if _, ok := groups[a]; !ok {
			order = append(order, a)
		}
		groups[a] = append(groups[a], t)
	}
	for _, a := range order {
		g := groups[a]
		var err error
		switch g[0].net.Type {
		case network.Ethereum:
			err = fillEthereum(ctx, k, g, endpoints)
		case network.Solana:
			err = fillSolana(ctx, k, g, endpoints)
		}
		if err != nil {
			return Result{}, err
		}
	}
	out := Result{Transactions: make([]signreq.TxSpec, len(parsed)), Notes: make([]Note, len(parsed))}
	for i, t := range parsed {
		raw, err := json.Marshal(t.fields)
		if err != nil {
			return Result{}, err
		}
		spec := signreq.TxSpec{Network: txs[i].Network}
		switch t.net.Type {
		case network.Ethereum:
			spec.Ethereum = raw
		case network.Solana:
			spec.Solana = raw
		}
		if t.note.Filled == nil {
			t.note.Filled = []string{}
		}
		out.Transactions[i], out.Notes[i] = spec, t.note
	}
	return out, nil
}

// parse resolves spec's Network and reads its one intent, refusing what
// sign propose would and what cannot be filled.
func parse(i int, spec signreq.TxSpec, nets []network.Network, k key.Key) (*tx, error) {
	n, err := network.Get(nets, spec.Network)
	if err != nil {
		return nil, fmt.Errorf("transaction %d: %w", i, err)
	}
	t := &tx{index: i, net: n, note: Note{Index: i, Network: n.Alias, Chain: n.Chain().String()}}
	if n.Type == network.Bitcoin {
		return nil, t.wrap(ErrBitcoin)
	}
	var raw json.RawMessage
	set := 0
	for _, in := range []struct {
		typ string
		raw json.RawMessage
	}{{network.Ethereum, spec.Ethereum}, {network.Bitcoin, spec.Bitcoin}, {network.Solana, spec.Solana}} {
		typ, r := in.typ, in.raw
		if len(r) == 0 {
			continue
		}
		set++
		if typ != n.Type {
			return nil, t.wrap(fmt.Errorf("%w: a %s intent on a %s Network", signreq.ErrInvalid, typ, n.Type))
		}
		raw = r
	}
	if set != 1 {
		return nil, t.wrap(fmt.Errorf("%w: give exactly one of ethereum, bitcoin, solana", signreq.ErrInvalid))
	}
	if c := n.Chain(); c.Curve() != k.Curve {
		return nil, t.wrap(fmt.Errorf("%w: %s needs a %s key but the key is %s", signreq.ErrCurveMismatch, c, c.Curve(), k.Curve))
	}
	if err := json.Unmarshal(raw, &t.fields); err != nil || t.fields == nil {
		return nil, t.wrap(fmt.Errorf("%w: the intent must be a JSON object", signreq.ErrInvalid))
	}
	switch n.Type {
	case network.Ethereum:
		t.eth = &coslv1.EthereumIntent{}
		err = protojson.Unmarshal(raw, t.eth)
	case network.Solana:
		t.sol = &coslv1.SolanaIntent{}
		err = protojson.Unmarshal(raw, t.sol)
	}
	if err != nil {
		return nil, t.wrap(fmt.Errorf("%w: %v", signreq.ErrInvalid, err))
	}
	return t, nil
}

// fillEthereum fills one chain's transactions on one endpoint, failing over
// to the next with nothing kept from the last.
func fillEthereum(ctx context.Context, k key.Key, g []*tx, endpoints Endpoints) error {
	n := g[0].net
	sender, err := key.EthereumAddress(k.PublicKey)
	if err != nil {
		return g[0].wrap(err)
	}
	var reads []*tx // the transactions that need the chain
	next := uint64(0)
	for _, t := range g {
		if !t.has("to") {
			return t.errorf("to is required: contract creation is not supported")
		}
		if !t.has("chainId") {
			t.set("chainId", fmt.Sprint(n.ChainID))
		}
		if t.has("nonce") {
			next = max(next, t.eth.Nonce+1)
		}
		if !t.has("nonce") || !t.has("maxPriorityFeePerGas") || !t.has("maxFeePerGas") || !t.has("gasLimit") {
			reads = append(reads, t)
		}
	}
	if len(reads) == 0 {
		return nil
	}
	urls, err := endpoints(n)
	if err != nil {
		return reads[0].wrap(err)
	}
	at := reads[0] // the transaction an error is about
	var filled map[*tx]map[string]json.RawMessage
	var refused error // an estimate the chain refused: no endpoint will differ
	err = ethrpc.Use(ctx, urls, n.ChainID, func(e ethrpc.Endpoint) error {
		filled = map[*tx]map[string]json.RawMessage{}
		var nonce, tip, baseFee *big.Int
		var err error
		for _, t := range reads {
			at = t
			f := map[string]json.RawMessage{}
			put := func(field string, v string) { f[field], _ = json.Marshal(v) }
			if !t.has("nonce") {
				if nonce == nil {
					if nonce, err = quantity(ctx, e, "eth_getTransactionCount", []any{sender, "pending"}); err != nil {
						return err
					}
					nonce = bigMax(nonce, new(big.Int).SetUint64(next))
				}
				put("nonce", nonce.String())
				nonce = new(big.Int).Add(nonce, big.NewInt(1))
			}
			txTip, ok := new(big.Int).SetString(t.eth.MaxPriorityFeePerGas, 10)
			if !t.has("maxPriorityFeePerGas") {
				if tip == nil {
					if tip, err = quantity(ctx, e, "eth_maxPriorityFeePerGas", nil); err != nil {
						return err
					}
				}
				txTip, ok = tip, true
				put("maxPriorityFeePerGas", tip.String())
			}
			if !t.has("maxFeePerGas") {
				if !ok {
					return fmt.Errorf("maxPriorityFeePerGas %q is not a base-10 integer", t.eth.MaxPriorityFeePerGas)
				}
				if baseFee == nil {
					if baseFee, err = latestBaseFee(ctx, e); err != nil {
						return err
					}
				}
				fee := new(big.Int).Add(new(big.Int).Lsh(baseFee, 1), txTip)
				put("maxFeePerGas", fee.String())
			}
			if !t.has("gasLimit") {
				gas, err := quantity(ctx, e, "eth_estimateGas", []any{estimate(sender, t.eth)})
				if errors.Is(err, jsonrpc.ErrResponse) {
					refused = t.wrap(fmt.Errorf("%s: %w", e, err))
					return nil
				}
				if err != nil {
					return err
				}
				put("gasLimit", gas.String())
			}
			filled[t] = f
		}
		for _, t := range reads {
			t.note.Endpoint = e.String()
		}
		return nil
	})
	if err != nil {
		return at.wrap(err)
	}
	if refused != nil {
		return refused
	}
	for _, t := range reads {
		for _, field := range []string{"nonce", "maxPriorityFeePerGas", "maxFeePerGas", "gasLimit"} {
			if v, ok := filled[t][field]; ok {
				delete(t.fields, snakeCase(field))
				t.fields[field] = v
				t.note.Filled = append(t.note.Filled, field)
			}
		}
	}
	return nil
}

// estimate is eth_estimateGas's call object for in, sent from sender.
func estimate(sender string, in *coslv1.EthereumIntent) map[string]any {
	call := map[string]any{"from": sender, "to": "0x" + hex.EncodeToString(in.To)}
	if v, ok := new(big.Int).SetString(in.Value, 10); ok {
		call["value"] = "0x" + v.Text(16)
	}
	if len(in.Data) > 0 {
		call["data"] = "0x" + hex.EncodeToString(in.Data)
	}
	return call
}

// quantity is a call whose result is a hex quantity.
func quantity(ctx context.Context, e ethrpc.Endpoint, method string, params []any) (*big.Int, error) {
	var got string
	if err := e.Call(ctx, method, params, &got); err != nil {
		return nil, err
	}
	return hexQuantity(method, got)
}

func hexQuantity(what, s string) (*big.Int, error) {
	if digits, ok := strings.CutPrefix(strings.ToLower(s), "0x"); ok && digits != "" {
		if v, ok := new(big.Int).SetString(digits, 16); ok {
			return v, nil
		}
	}
	return nil, fmt.Errorf("%s: %q is not a hex quantity", what, s)
}

// latestBaseFee is the latest block's baseFeePerGas.
func latestBaseFee(ctx context.Context, e ethrpc.Endpoint) (*big.Int, error) {
	var block *struct {
		BaseFee string `json:"baseFeePerGas"`
	}
	if err := e.Call(ctx, "eth_getBlockByNumber", []any{"latest", false}, &block); err != nil {
		return nil, err
	}
	if block == nil || block.BaseFee == "" {
		return nil, errors.New("eth_getBlockByNumber: the latest block has no baseFeePerGas (not an EIP-1559 chain?)")
	}
	return hexQuantity("baseFeePerGas", block.BaseFee)
}

func bigMax(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

// fillSolana fills one cluster's transactions, reading one blockhash for
// all of them.
func fillSolana(ctx context.Context, k key.Key, g []*tx, endpoints Endpoints) error {
	n := g[0].net
	var reads []*tx
	for _, t := range g {
		if !t.has("feePayer") {
			t.set("feePayer", base58.Encode(k.PublicKey))
		}
		if !t.has("recentBlockhash") {
			reads = append(reads, t)
		}
	}
	if len(reads) > 0 {
		urls, err := endpoints(n)
		if err != nil {
			return reads[0].wrap(err)
		}
		var hash, endpoint string
		err = solrpc.Use(ctx, urls, n.GenesisHash, func(e solrpc.Endpoint) error {
			var got struct {
				Value struct {
					Blockhash string `json:"blockhash"`
				} `json:"value"`
			}
			if err := e.Call(ctx, "getLatestBlockhash", []any{map[string]any{"commitment": "finalized"}}, &got); err != nil {
				return err
			}
			if got.Value.Blockhash == "" {
				return errors.New("getLatestBlockhash: no blockhash")
			}
			hash, endpoint = got.Value.Blockhash, e.String()
			return nil
		})
		if err != nil {
			return reads[0].wrap(err)
		}
		for _, t := range reads {
			t.set("recentBlockhash", hash)
			t.note.Endpoint, t.note.Warning = endpoint, BlockhashWarning
		}
	}
	for _, t := range g {
		if !t.has("cluster") {
			t.set("cluster", n.Cluster)
		}
		if !t.has("genesisHash") && n.GenesisHash != "" {
			t.set("genesisHash", n.GenesisHash)
		}
	}
	return nil
}
