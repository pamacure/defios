// Package signreq builds, validates, and reads Sign Requests off the COSL
// (docs/adr/0009-sign-request-on-cosl.md). There is no requests table: a
// Sign Request is a sign.request row, and its status is derived from its
// deadline. No signatures are produced here.
package signreq

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/network"
	coslv1 "defios.com/proto/cosl/v1"
)

var (
	// ErrInvalid is a Sign Request that fails validation.
	ErrInvalid = errors.New("sign: invalid request")
	// ErrCurveMismatch is a transaction whose Network needs a different curve
	// than the target Key's.
	ErrCurveMismatch = errors.New("sign: network curve does not match the key's curve")
	// ErrUnknown is an inspect query that matches no Sign Request.
	ErrUnknown = errors.New("sign: no such sign request")
	// ErrAmbiguous is a msg id prefix matching more than one Sign Request.
	ErrAmbiguous = errors.New("sign: ambiguous sign request")
)

// DefaultDeadlineSeconds is the window a request stays open when the
// proposer names none.
const DefaultDeadlineSeconds = 300

// MaxDeadlineSeconds bounds a request's window (one year), keeping deadline
// arithmetic far from overflow.
const MaxDeadlineSeconds = 365 * 24 * 3600

// MaxReasonBytes bounds a request's reason.
const MaxReasonBytes = 4096

var (
	decimal = regexp.MustCompile(`^[0-9]+$`)
	base58  = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)
	txid    = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// TxSpec is one transaction as a proposer writes it: a Network by alias and
// exactly one intent as protojson.
type TxSpec struct {
	Network  string  `json:"network"`
	Ethereum rawJSON `json:"ethereum,omitempty"`
	Bitcoin  rawJSON `json:"bitcoin,omitempty"`
	Solana   rawJSON `json:"solana,omitempty"`
}

// Spec is a proposal as the CLI sends it to serve.
type Spec struct {
	Reason          string   `json:"reason"`
	DeadlineSeconds uint64   `json:"deadline_seconds,omitempty"`
	Transactions    []TxSpec `json:"transactions"`
}

type rawJSON = json.RawMessage

// Build resolves a Spec's Network aliases against the live Networks into a
// SignRequest, defaulting the deadline. It does not validate the request.
func Build(spec Spec, nets []network.Network) (*coslv1.SignRequest, error) {
	out := &coslv1.SignRequest{Reason: spec.Reason, DeadlineSeconds: spec.DeadlineSeconds}
	if out.DeadlineSeconds == 0 {
		out.DeadlineSeconds = DefaultDeadlineSeconds
	}
	for i, t := range spec.Transactions {
		n, err := network.Get(nets, t.Network)
		if err != nil {
			return nil, err
		}
		tx := &coslv1.SignTransaction{NetworkConfigId: n.ConfigID[:]}
		set := 0
		for _, in := range []struct {
			raw rawJSON
			msg proto.Message
		}{
			{t.Ethereum, &coslv1.EthereumIntent{}},
			{t.Bitcoin, &coslv1.BitcoinIntent{}},
			{t.Solana, &coslv1.SolanaIntent{}},
		} {
			if len(in.raw) == 0 {
				continue
			}
			if string(bytes.TrimSpace(in.raw)) == "null" {
				return nil, invalidf("transaction %d: intent must not be null", i)
			}
			set++
			if err := protojson.Unmarshal(in.raw, in.msg); err != nil {
				return nil, invalidf("transaction %d: %v", i, err)
			}
			switch m := in.msg.(type) {
			case *coslv1.EthereumIntent:
				tx.Intent = &coslv1.SignTransaction_Ethereum{Ethereum: m}
			case *coslv1.BitcoinIntent:
				tx.Intent = &coslv1.SignTransaction_Bitcoin{Bitcoin: m}
			case *coslv1.SolanaIntent:
				tx.Intent = &coslv1.SignTransaction_Solana{Solana: m}
			}
		}
		if set != 1 {
			return nil, invalidf("transaction %d: give exactly one of ethereum, bitcoin, solana", i)
		}
		out.Transactions = append(out.Transactions, tx)
	}
	return out, nil
}

// Validate is the writer's check at creation: shape, the reason, that every
// Network is live and of the Key's curve, and that each intent is complete
// for its Network type (every consensus-critical field is inside the Digest
// and serve does no chain RPC to fill it).
func Validate(r *coslv1.SignRequest, k key.Key, nets []network.Network) error {
	switch {
	case len(r.Transactions) == 0:
		return invalidf("at least one transaction is required")
	case strings.TrimSpace(r.Reason) == "":
		return invalidf("a reason is required")
	case !utf8.ValidString(r.Reason) || len(r.Reason) > MaxReasonBytes:
		return invalidf("reason must be UTF-8 of at most %d bytes", MaxReasonBytes)
	case r.DeadlineSeconds == 0 || r.DeadlineSeconds > MaxDeadlineSeconds:
		return invalidf("deadline must be 1–%d seconds", MaxDeadlineSeconds)
	}
	if ref := r.ReasonRef; ref != nil {
		if (len(ref.ConversationId) != 0 && len(ref.ConversationId) != 32) || len(ref.MsgId) != 32 {
			return invalidf("reason_ref needs a 32-byte msg_id and an unset or 32-byte conversation_id")
		}
	}
	for i, tx := range r.Transactions {
		n, ok := liveByID(nets, tx.NetworkConfigId)
		if !ok {
			return invalidf("transaction %d: network config %x is not a live Network", i, tx.NetworkConfigId)
		}
		if n.Curve() != k.Curve {
			return fmt.Errorf("%w: transaction %d targets %s (%s) but the key is %s", ErrCurveMismatch, i, n.Alias, n.Curve(), k.Curve)
		}
		var err error
		switch in := tx.Intent.(type) {
		case *coslv1.SignTransaction_Ethereum:
			err = checkEthereum(n, in.Ethereum)
		case *coslv1.SignTransaction_Bitcoin:
			err = checkBitcoin(n, in.Bitcoin)
		case *coslv1.SignTransaction_Solana:
			err = checkSolana(n, in.Solana)
		default:
			err = errors.New("no intent")
		}
		if err != nil {
			return invalidf("transaction %d (%s): %v", i, n.Alias, err)
		}
	}
	return nil
}

func liveByID(nets []network.Network, id []byte) (network.Network, bool) {
	for _, n := range nets {
		if bytes.Equal(n.ConfigID[:], id) {
			return n, true
		}
	}
	return network.Network{}, false
}

func checkEthereum(n network.Network, in *coslv1.EthereumIntent) error {
	switch {
	case n.Type != network.Ethereum:
		return fmt.Errorf("an ethereum intent on a %s network", n.Type)
	case len(in.To) != 20:
		return fmt.Errorf("to must be 20 bytes, got %d", len(in.To))
	case in.GasLimit == 0:
		return errors.New("gas_limit must be > 0")
	}
	if _, err := amount(in.Value, true); err != nil {
		return fmt.Errorf("value %q: %v", in.Value, err)
	}
	maxFee, err := amount(in.MaxFeePerGas, false)
	if err != nil || maxFee.Sign() == 0 {
		return fmt.Errorf("max_fee_per_gas %q must be an integer > 0 below 2^256", in.MaxFeePerGas)
	}
	tip, err := amount(in.MaxPriorityFeePerGas, true)
	if err != nil {
		return fmt.Errorf("max_priority_fee_per_gas %q: %v", in.MaxPriorityFeePerGas, err)
	}
	if tip.Cmp(maxFee) > 0 {
		return errors.New("max_priority_fee_per_gas exceeds max_fee_per_gas")
	}
	return nil
}

// amount parses a base-10 integer; blank is zero when allowed.
func amount(s string, blankOK bool) (*big.Int, error) {
	if s == "" && blankOK {
		return new(big.Int), nil
	}
	if !decimal.MatchString(s) {
		return nil, errors.New("not a base-10 integer")
	}
	v, _ := new(big.Int).SetString(s, 10)
	if v.BitLen() > 256 {
		return nil, errors.New("exceeds 2^256-1")
	}
	return v, nil
}

const maxSats = 21_000_000 * 100_000_000

func checkBitcoin(n network.Network, in *coslv1.BitcoinIntent) error {
	if n.Type != network.Bitcoin {
		return fmt.Errorf("a bitcoin intent on a %s network", n.Type)
	}
	if len(in.Inputs) == 0 || len(in.Outputs) == 0 {
		return errors.New("needs at least one input and one output")
	}
	var sum, out uint64
	for i, x := range in.Inputs {
		if !txid.MatchString(x.Txid) || len(x.ScriptPubkey) == 0 || x.AmountSats == 0 {
			return fmt.Errorf("input %d needs a 64-hex txid, amount_sats and script_pubkey", i)
		}
		if sum += x.AmountSats; sum < x.AmountSats || sum > maxSats {
			return errors.New("input total exceeds 21 million BTC")
		}
	}
	for i, x := range in.Outputs {
		if x.Address == "" || x.AmountSats == 0 {
			return fmt.Errorf("output %d needs an address and amount_sats", i)
		}
		if out += x.AmountSats; out < x.AmountSats || out > maxSats {
			return errors.New("output total exceeds 21 million BTC")
		}
	}
	if out > sum {
		return fmt.Errorf("outputs (%d sats) exceed inputs (%d sats)", out, sum)
	}
	return nil
}

func checkSolana(n network.Network, in *coslv1.SolanaIntent) error {
	switch {
	case n.Type != network.Solana:
		return fmt.Errorf("a solana intent on a %s network", n.Type)
	case !base58.MatchString(in.FeePayer) || !base58.MatchString(in.RecentBlockhash):
		return errors.New("fee_payer and recent_blockhash must be base58 32-byte values")
	case len(in.Instructions) == 0:
		return errors.New("needs at least one instruction")
	}
	for i, ix := range in.Instructions {
		if !base58.MatchString(ix.ProgramId) {
			return fmt.Errorf("instruction %d needs a base58 program_id", i)
		}
		for _, a := range ix.Accounts {
			if !base58.MatchString(a.Pubkey) {
				return fmt.Errorf("instruction %d has a non-base58 account pubkey %q", i, a.Pubkey)
			}
		}
	}
	return nil
}

// Status of a request, derived from its deadline: nothing signs yet, so a
// request is open until its window passes.
const (
	Open    = "open"
	Expired = "expired"
)

// Summary is one `sign list` row.
type Summary struct {
	Seq          uint64 `json:"seq"`
	MsgID        string `json:"msg_id"`
	Conversation string `json:"conversation_id"`
	Proposer     string `json:"proposer"`
	CreatedAt    string `json:"created_at"`
	Status       string `json:"status"`
	Transactions int    `json:"transactions"`
	Reason       string `json:"reason"`
}

// TxDetail is one decoded transaction, human-readable in Lines.
type TxDetail struct {
	Network string   `json:"network"`
	Type    string   `json:"type"`
	Lines   []string `json:"lines"`
	Digests []string `json:"digests,omitempty"` // hex, one per signature the transaction needs
}

// Detail is `sign inspect`.
type Detail struct {
	Summary
	DeadlineSeconds uint64     `json:"deadline_seconds"`
	ReasonRef       string     `json:"reason_ref,omitempty"`
	Details         []TxDetail `json:"details"`
}

func status(r cosl.Row, req *coslv1.SignRequest, now time.Time) string {
	window := min(req.DeadlineSeconds, MaxDeadlineSeconds) // a row from a foreign writer may carry more
	if now.After(time.Unix(0, r.CreatedAt).Add(time.Duration(window) * time.Second)) {
		return Expired
	}
	return Open
}

func decodeRow(r cosl.Row) (*coslv1.SignRequest, error) {
	env, err := r.Decode()
	if err != nil {
		return nil, err
	}
	msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
	if err != nil {
		return nil, fmt.Errorf("sign: seq %d: %w", r.Seq, err)
	}
	return msg.(*coslv1.SignRequest), nil
}

func summarise(r cosl.Row, req *coslv1.SignRequest, now time.Time) Summary {
	return Summary{
		Seq: r.Seq, MsgID: hex.EncodeToString(r.MsgID[:]), Conversation: hex.EncodeToString(r.ConversationID),
		Proposer: r.Sender.String(), CreatedAt: time.Unix(0, r.CreatedAt).UTC().Format(time.RFC3339),
		Status: status(r, req, now), Transactions: len(req.Transactions), Reason: req.Reason,
	}
}

// List reads every Sign Request off rows, in log order.
func List(rows []cosl.Row, now time.Time) ([]Summary, error) {
	out := []Summary{}
	for _, r := range rows {
		if r.MsgType != cosl.TypeSignRequest {
			continue
		}
		req, err := decodeRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, summarise(r, req, now))
	}
	return out, nil
}

// Inspect decodes the one Sign Request whose msg id starts with query.
// history resolves each transaction's Network, even if since edited.
func Inspect(rows []cosl.Row, query string, history map[[32]byte]network.Network, now time.Time) (Detail, error) {
	m, err := match(rows, query)
	if err != nil {
		return Detail{}, err
	}
	req, err := decodeRow(m)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Summary: summarise(m, req, now), DeadlineSeconds: req.DeadlineSeconds, Details: []TxDetail{}}
	if ref := req.ReasonRef; ref != nil {
		d.ReasonRef = fmt.Sprintf("%x/%x", ref.ConversationId, ref.MsgId)
	}
	for _, tx := range req.Transactions {
		var id [32]byte
		copy(id[:], tx.NetworkConfigId)
		n, known := history[id]
		td := TxDetail{Network: n.Alias, Type: n.Type}
		if !known {
			td.Network = fmt.Sprintf("unknown(%x)", tx.NetworkConfigId)
			td.Lines = []string{"network config not found on this log; intent not decoded"}
			d.Details = append(d.Details, td)
			continue
		}
		switch in := tx.Intent.(type) {
		case *coslv1.SignTransaction_Ethereum:
			td.Lines = describeEthereum(n, in.Ethereum)
			if _, dg, err := EthereumDigest(n.ChainID, in.Ethereum); err != nil {
				td.Lines = append(td.Lines, "digest unavailable: "+err.Error())
			} else {
				td.Digests = []string{hex.EncodeToString(dg[:])}
			}
		case *coslv1.SignTransaction_Bitcoin:
			td.Lines = describeBitcoin(in.Bitcoin)
		case *coslv1.SignTransaction_Solana:
			td.Lines = describeSolana(in.Solana)
		}
		d.Details = append(d.Details, td)
	}
	return d, nil
}

// match is the one sign.request row whose msg id starts with query.
func match(rows []cosl.Row, query string) (cosl.Row, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return cosl.Row{}, ErrUnknown
	}
	var found []cosl.Row
	for _, r := range rows {
		if r.MsgType == cosl.TypeSignRequest && strings.HasPrefix(hex.EncodeToString(r.MsgID[:]), query) {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return cosl.Row{}, ErrUnknown
	case 1:
		return found[0], nil
	default:
		return cosl.Row{}, ErrAmbiguous
	}
}

func describeEthereum(n network.Network, in *coslv1.EthereumIntent) []string {
	value := in.Value
	if value == "" {
		value = "0"
	}
	lines := []string{
		fmt.Sprintf("chain_id %d", n.ChainID),
		fmt.Sprintf("to 0x%x", in.To),
		fmt.Sprintf("value %s wei", value),
		fmt.Sprintf("nonce %d", in.Nonce),
		fmt.Sprintf("gas_limit %d", in.GasLimit),
		fmt.Sprintf("max_fee_per_gas %s", in.MaxFeePerGas),
		fmt.Sprintf("max_priority_fee_per_gas %s", orZero(in.MaxPriorityFeePerGas)),
	}
	if len(in.Data) > 0 {
		lines = append(lines, fmt.Sprintf("calldata 0x%x (%d bytes)", in.Data, len(in.Data)))
	}
	return lines
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func describeBitcoin(in *coslv1.BitcoinIntent) []string {
	var lines []string
	var sum, out uint64
	for i, x := range in.Inputs {
		lines = append(lines, fmt.Sprintf("input %d %s:%d %d sats", i, x.Txid, x.Vout, x.AmountSats))
		sum += x.AmountSats
	}
	for i, x := range in.Outputs {
		lines = append(lines, fmt.Sprintf("output %d %s %d sats", i, x.Address, x.AmountSats))
		out += x.AmountSats
	}
	if out <= sum && sum <= maxSats {
		lines = append(lines, fmt.Sprintf("fee %d sats", sum-out))
	}
	if in.LockTime != 0 {
		lines = append(lines, fmt.Sprintf("lock_time %d", in.LockTime))
	}
	return lines
}

func describeSolana(in *coslv1.SolanaIntent) []string {
	lines := []string{"fee_payer " + in.FeePayer, "recent_blockhash " + in.RecentBlockhash}
	for i, ix := range in.Instructions {
		lines = append(lines, fmt.Sprintf("instruction %d program %s, %d accounts, %d data bytes", i, ix.ProgramId, len(ix.Accounts), len(ix.Data)))
		for _, a := range ix.Accounts {
			flags := ""
			if a.Signer {
				flags += " signer"
			}
			if a.Writable {
				flags += " writable"
			}
			lines = append(lines, "  account "+a.Pubkey+flags)
		}
	}
	return lines
}
