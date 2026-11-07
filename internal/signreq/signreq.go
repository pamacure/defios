// Package signreq builds, validates, and reads Sign Requests off the COSL
// (docs/adr/0009-sign-request-on-cosl.md). There is no requests table: a
// Sign Request is a sign.request row, and its status is derived from
// the log. Signatures are produced elsewhere (internal/session).
package signreq

import (
	"bytes"
	"crypto/ed25519"
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

	"defios.com/internal/base58"
	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	"defios.com/internal/key"
	"defios.com/internal/mention"
	"defios.com/internal/network"
	"defios.com/internal/session"
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
	// ErrNotSigned is a broadcast of a request no signing Session has completed.
	ErrNotSigned = errors.New("sign: the request has no verified signature yet")
	// ErrAmbiguous is a msg id prefix matching more than one Sign Request.
	ErrAmbiguous = errors.New("sign: ambiguous sign request")
)

// DefaultDeadlineSeconds is the window a request stays open when the
// proposer names none.
const DefaultDeadlineSeconds = 300

// SolanaDeadlineSeconds is the tighter default for a request with a Solana
// transaction: its recentBlockhash expires in about 60–90 seconds, so a
// longer window could only collect signatures that can no longer land.
const SolanaDeadlineSeconds = 60

// MaxDeadlineSeconds bounds a request's window (one year), keeping deadline
// arithmetic far from overflow.
const MaxDeadlineSeconds = 365 * 24 * 3600

// MaxReasonBytes bounds a request's reason.
const MaxReasonBytes = 4096

var (
	decimal = regexp.MustCompile(`^[0-9]+$`)
	txid    = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// TxSpec is one transaction as a proposer writes it: one of the proposing
// Node's Networks by alias, and exactly one intent as protojson. The alias
// stays on that Node: the request carries its chain's identity.
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

// Build resolves a Spec's Network aliases against this Node's Networks into
// a SignRequest whose intents name their chains, defaulting the deadline.
// An intent may name its chain itself, but only its alias's. It does not
// otherwise validate the request.
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
		if n.Type == network.Solana && spec.DeadlineSeconds == 0 {
			out.DeadlineSeconds = min(out.DeadlineSeconds, SolanaDeadlineSeconds)
		}
		tx := &coslv1.SignTransaction{}
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
		if err := name(tx, n.Chain()); err != nil {
			return nil, invalidf("transaction %d (%s): %v", i, n.Alias, err)
		}
		out.Transactions = append(out.Transactions, tx)
	}
	return out, nil
}

// name writes c's identity into tx's intent, which must be of c's type and
// may already name c, but no other chain.
func name(tx *coslv1.SignTransaction, c network.Chain) error {
	if got := ChainOf(tx); got.Type != c.Type {
		return fmt.Errorf("a %s intent on a %s Network", got.Type, c.Type)
	}
	switch in := tx.Intent.(type) {
	case *coslv1.SignTransaction_Ethereum:
		if in.Ethereum.ChainId != 0 && in.Ethereum.ChainId != c.ChainID {
			return fmt.Errorf("the intent names chain_id %d", in.Ethereum.ChainId)
		}
		in.Ethereum.ChainId = c.ChainID
	case *coslv1.SignTransaction_Bitcoin:
		if in.Bitcoin.Network != "" && in.Bitcoin.Network != c.BitcoinNetwork {
			return fmt.Errorf("the intent names network %q", in.Bitcoin.Network)
		}
		in.Bitcoin.Network = c.BitcoinNetwork
	case *coslv1.SignTransaction_Solana:
		s := in.Solana
		if s.Cluster != "" && s.Cluster != c.Cluster || s.GenesisHash != "" && c.GenesisHash != "" && s.GenesisHash != c.GenesisHash {
			return fmt.Errorf("the intent names another cluster")
		}
		s.Cluster = c.Cluster
		if s.GenesisHash == "" {
			s.GenesisHash = c.GenesisHash
		}
	}
	return nil
}

// ChainOf is the chain tx's intent names; its Type is empty for a
// transaction with no intent. It is not validated (Chain.Validate).
func ChainOf(tx *coslv1.SignTransaction) network.Chain {
	switch in := tx.Intent.(type) {
	case *coslv1.SignTransaction_Ethereum:
		return network.Chain{Type: network.Ethereum, ChainID: in.Ethereum.ChainId}
	case *coslv1.SignTransaction_Bitcoin:
		return network.Chain{Type: network.Bitcoin, BitcoinNetwork: in.Bitcoin.Network}
	case *coslv1.SignTransaction_Solana:
		return network.Chain{Type: network.Solana, Cluster: in.Solana.Cluster, GenesisHash: in.Solana.GenesisHash}
	}
	return network.Chain{}
}

// CheckChains is whether this Node, with Networks nets, may accept r: a
// Node's Networks are the chains it signs for, so every transaction's chain
// must be one of them. The error names the first chain that is not.
func CheckChains(r *coslv1.SignRequest, nets []network.Network) error {
	for i, tx := range r.Transactions {
		if _, err := network.Find(nets, ChainOf(tx)); err != nil {
			return fmt.Errorf("transaction %d: %w", i, err)
		}
	}
	return nil
}

// Validate is the writer's check at creation: shape, the reason, that every
// transaction names a valid chain of the Key's curve, and that each intent
// is complete for its chain (every consensus-critical field is inside the
// Digest and serve does no chain RPC to fill it).
func Validate(r *coslv1.SignRequest, k key.Key) error {
	if err := CheckReason(r.Reason); err != nil {
		return err
	}
	if err := CheckHandle(r.Handle); err != nil {
		return err
	}
	switch {
	case len(r.Transactions) == 0:
		return invalidf("at least one transaction is required")
	case r.DeadlineSeconds == 0 || r.DeadlineSeconds > MaxDeadlineSeconds:
		return invalidf("deadline must be 1–%d seconds", MaxDeadlineSeconds)
	}
	for i, tx := range r.Transactions {
		if tx.Intent == nil {
			return invalidf("transaction %d: no intent", i)
		}
		c := ChainOf(tx)
		if err := c.Validate(); err != nil {
			return invalidf("transaction %d: %v", i, err)
		}
		if c.Curve() != k.Curve {
			return fmt.Errorf("%w: transaction %d targets %s (%s) but the key is %s", ErrCurveMismatch, i, c, c.Curve(), k.Curve)
		}
		var err error
		switch in := tx.Intent.(type) {
		case *coslv1.SignTransaction_Ethereum:
			err = checkEthereum(in.Ethereum)
		case *coslv1.SignTransaction_Bitcoin:
			err = checkBitcoin(k, in.Bitcoin)
		case *coslv1.SignTransaction_Solana:
			err = checkSolana(k, in.Solana)
		}
		if err != nil {
			return invalidf("transaction %d (%s): %v", i, c, err)
		}
	}
	return nil
}

func checkEthereum(in *coslv1.EthereumIntent) error {
	switch {
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

// checkBitcoin also holds every input to the Key's own P2WPKH script, the one
// a Key signs for, and every output address to the intent's bitcoin network.
func checkBitcoin(k key.Key, in *coslv1.BitcoinIntent) error {
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
	own := P2WPKHScript(k.PublicKey)
	for i, x := range in.Inputs {
		if !bytes.Equal(x.ScriptPubkey, own) {
			return fmt.Errorf("input %d must spend the Key's P2WPKH output %x", i, own)
		}
	}
	_, err := BitcoinDigests(in.Network, in)
	return err
}

func checkSolana(k key.Key, in *coslv1.SolanaIntent) error {
	payer, err := solanaKey(in.FeePayer)
	switch {
	case err != nil:
		return fmt.Errorf("fee_payer: %v", err)
	case !bytes.Equal(payer, k.PublicKey):
		return errors.New("fee_payer must be the Key's address")
	case len(in.Instructions) == 0:
		return errors.New("needs at least one instruction")
	}
	_, err = SolanaMessage(in)
	return err
}

// Status of a request, derived from the log: open while it collects accepts,
// waiting once t Nodes have accepted but no Session has started (for instance
// while another Session holds the slot), then signing, signed or aborted with
// its Session. A request with no Session is refused once more than n − t
// Nodes have refused it, so t accepts can no longer exist, and else expired
// once its deadline passes; a Session outlives the deadline, so signed and
// aborted beat refused and expired.
const (
	Open    = "open"
	Waiting = "waiting"
	Signing = "signing"
	Done    = "signed"
	Aborted = "aborted"
	Refused = "refused"
	Expired = "expired"
)

// Statuses is every status a Sign Request may have.
var Statuses = []string{Open, Waiting, Signing, Done, Aborted, Refused, Expired}

// This Node's own stance on a request, as list and inspect show it.
const (
	StanceAccepted    = "accepted"
	StanceRefused     = "refused"
	StanceUnaddressed = "unaddressed"
)

// deadline is when a request lapses.
func deadline(r cosl.Row, req *coslv1.SignRequest) time.Time {
	return Deadline(r.CreatedAt, req)
}

// Deadline is when a request whose envelope was created at createdAt
// (Unix nanoseconds) lapses, committed or not.
func Deadline(createdAt int64, req *coslv1.SignRequest) time.Time {
	window := min(req.DeadlineSeconds, MaxDeadlineSeconds) // a row from a foreign writer may carry more
	return time.Unix(0, createdAt).Add(time.Duration(window) * time.Second)
}

// Stance is one Node's final word on a Sign Request: it accepted, or it
// refused with a Reason (docs/adr/0028-a-node-refuses-a-sign-request-once.md).
// Handle is the agent that took it, empty for a person.
type Stance struct {
	Node    identity.ID
	Seq     uint64
	Handle  string
	Refused bool
	Reason  string
}

// Stances are the Nodes' stances on the request at row, in COSL log order
// with the proposer first (its signature on the request is its accept). A
// Node's first sign.accept or sign.refuse within the request's deadline is
// its stance and anything later from it is ignored. A malformed row, one
// with an invalid Handle or a refusal with no reason, is no stance. The FSM
// already drops senders that are not on the Roster.
func Stances(rows []cosl.Row, row cosl.Row, req *coslv1.SignRequest) []Stance {
	out := []Stance{{Node: row.Sender, Seq: row.Seq, Handle: req.Handle}}
	end := deadline(row, req)
	seen := map[identity.ID]struct{}{row.Sender: {}}
	for _, r := range rows {
		if r.MsgType != cosl.TypeSignAccept && r.MsgType != cosl.TypeSignRefuse || r.Seq <= row.Seq || !bytes.Equal(r.ConversationID, row.ConversationID) {
			continue
		}
		if _, dup := seen[r.Sender]; dup || time.Unix(0, r.CreatedAt).After(end) {
			continue
		}
		msg, err := decodePayload(r)
		if err != nil {
			continue
		}
		st, id := Stance{Node: r.Sender, Seq: r.Seq}, []byte(nil)
		switch m := msg.(type) {
		case *coslv1.SignAccept:
			id, st.Handle = m.SignRequestId, m.Handle
		case *coslv1.SignRefuse:
			if CheckReason(m.Reason) != nil {
				continue
			}
			id, st.Handle, st.Refused, st.Reason = m.SignRequestId, m.Handle, true, m.Reason
		}
		if !bytes.Equal(id, row.MsgID[:]) || !validHandle(st.Handle) {
			continue
		}
		seen[r.Sender] = struct{}{}
		out = append(out, st)
	}
	return out
}

// Accepts is who has accepted the request at row, in COSL log order with the
// proposer first: the accepting Stances.
func Accepts(rows []cosl.Row, row cosl.Row, req *coslv1.SignRequest) []session.SignAccept {
	var out []session.SignAccept
	for _, st := range Stances(rows, row, req) {
		if !st.Refused {
			out = append(out, session.SignAccept{Node: st.Node, Seq: st.Seq})
		}
	}
	return out
}

// CheckReason is the rule for a request's or a refusal's reason: required,
// UTF-8, at most MaxReasonBytes.
func CheckReason(reason string) error {
	switch {
	case strings.TrimSpace(reason) == "":
		return invalidf("a reason is required")
	case !utf8.ValidString(reason) || len(reason) > MaxReasonBytes:
		return invalidf("reason must be UTF-8 of at most %d bytes", MaxReasonBytes)
	}
	return nil
}

// validHandle is whether h may name who acted on a sign row: empty for a
// person, else a Handle (docs/adr/0016-what-a-mention-is.md).
func validHandle(h string) bool { return h == "" || mention.ValidHandle(h) }

// CheckHandle is the writer's check of who acts on a sign row.
func CheckHandle(h string) error {
	if !validHandle(h) {
		return invalidf("handle %q is not a Handle", h)
	}
	return nil
}

// Summary is one `sign list` row.
type Summary struct {
	Seq          uint64 `json:"seq"`
	MsgID        string `json:"msg_id"`
	Conversation string `json:"conversation_id"`
	Proposer     string `json:"proposer"`
	// ProposerHandle is the agent that proposed, empty for a person.
	ProposerHandle string `json:"proposer_handle,omitempty"`
	CreatedAt      string `json:"created_at"`
	Status         string `json:"status"`
	Transactions   int    `json:"transactions"`
	Reason         string `json:"reason"`
	// Accepted is who has accepted, proposer first; Needed is the Key's t.
	// Later accepts than the first Needed do not sign.
	Accepted []string `json:"accepted"`
	Needed   int      `json:"needed"`
	// Refused is who has refused, in log order.
	Refused []string `json:"refused"`
	// Accepts and Refusals are the same stances in full: each Node's seq
	// and the Handle of the agent that took it.
	Accepts  []Actor   `json:"accepts"`
	Refusals []Refusal `json:"refusals"`
	// Stance is this Node's own: accepted, refused or unaddressed. The
	// reader fills it, knowing which Node it is.
	Stance string `json:"stance,omitempty"`
	// KeyKnown is false when the request's Key is not on this log, in which
	// case Needed is 0 and means "unknown", not zero signers.
	KeyKnown bool `json:"key_known"`
	// Session is the signing Session's id once t Nodes have accepted and one
	// has started it.
	Session string `json:"session,omitempty"`
	// Broadcasts counts the Broadcast Receipts committed for the request.
	Broadcasts int `json:"broadcasts"`
	// FullyBroadcast is true once every transaction appears in some receipt.
	FullyBroadcast bool `json:"fully_broadcast"`
}

// BroadcastedTx is one transaction a Broadcast Receipt says was submitted.
type BroadcastedTx struct {
	Index  uint32 `json:"index"` // position in the request's transactions
	Chain  string `json:"chain"` // e.g. "ethereum chain 1"
	TxHash string `json:"tx_hash"`
}

// BroadcastReceipt is one sign.broadcast, as `sign inspect` shows it.
type BroadcastReceipt struct {
	Seq          uint64          `json:"seq"`
	Broadcaster  string          `json:"broadcaster"`
	At           string          `json:"at"`
	Transactions []BroadcastedTx `json:"transactions"`
}

// TxDetail is one decoded transaction, human-readable in Lines.
type TxDetail struct {
	Chain   string   `json:"chain"` // e.g. "bitcoin testnet"; never an alias
	Type    string   `json:"type"`
	Lines   []string `json:"lines"`
	Digests []string `json:"digests,omitempty"` // hex, one per signature the transaction needs
}

// Actor is a Node that took a stance on a request, at its seq, and the
// Handle of the agent that took it, empty for a person.
type Actor struct {
	Node   string `json:"node"`
	Handle string `json:"handle,omitempty"`
	Seq    uint64 `json:"seq"`
}

// Refusal is one Node's refusal, with its reason.
type Refusal struct {
	Actor
	Reason string `json:"reason"`
}

// Detail is `sign inspect`.
type Detail struct {
	Summary
	DeadlineSeconds uint64     `json:"deadline_seconds"`
	Details         []TxDetail `json:"details"`
	// BroadcastReceipts are the request's sign.broadcast rows, in log order.
	BroadcastReceipts []BroadcastReceipt `json:"broadcast_receipts"`
}

// status derives a request's status. sessions are the log's Sessions in start
// order, of which the first for the request is the one that counts.
func status(r cosl.Row, req *coslv1.SignRequest, sessions []session.Summary, accepted, refused int, k threshold, now time.Time) string {
	id := hex.EncodeToString(r.MsgID[:])
	for _, s := range sessions {
		if s.SignRequest != id {
			continue
		}
		switch s.Status {
		case session.Complete:
			return Done
		case session.Aborted:
			return Aborted
		}
		return Signing
	}
	switch {
	case k.t > 0 && refused > k.n-k.t:
		return Refused
	case now.After(deadline(r, req)):
		return Expired
	case k.t > 0 && accepted >= k.t:
		return Waiting
	}
	return Open
}

// decodePayload is the row's payload message, per its registry type.
func decodePayload(r cosl.Row) (proto.Message, error) {
	env, err := r.Decode()
	if err != nil {
		return nil, err
	}
	msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
	if err != nil {
		return nil, fmt.Errorf("sign: seq %d: %w", r.Seq, err)
	}
	return msg, nil
}

// decodeRow is the sign.request at r. One whose Handle is malformed is
// ignored as malformed: it is ErrUnknown, as if it were not on the log.
func decodeRow(r cosl.Row) (*coslv1.SignRequest, error) {
	msg, err := decodePayload(r)
	if err != nil {
		return nil, err
	}
	req := msg.(*coslv1.SignRequest)
	if !validHandle(req.Handle) {
		return nil, fmt.Errorf("%w: seq %d has a malformed handle", ErrUnknown, r.Seq)
	}
	return req, nil
}

func summarise(rows []cosl.Row, sessions []session.Summary, keys map[string]threshold, r cosl.Row, req *coslv1.SignRequest, now time.Time) Summary {
	sum := Summary{
		Seq: r.Seq, MsgID: hex.EncodeToString(r.MsgID[:]), Conversation: hex.EncodeToString(r.ConversationID),
		Proposer: r.Sender.String(), ProposerHandle: req.Handle, CreatedAt: time.Unix(0, r.CreatedAt).UTC().Format(time.RFC3339),
		Transactions: len(req.Transactions), Reason: req.Reason,
		Accepted: []string{}, Refused: []string{}, Accepts: []Actor{}, Refusals: []Refusal{},
	}
	for _, st := range Stances(rows, r, req) {
		a := Actor{Node: st.Node.String(), Handle: st.Handle, Seq: st.Seq}
		if st.Refused {
			sum.Refused = append(sum.Refused, a.Node)
			sum.Refusals = append(sum.Refusals, Refusal{Actor: a, Reason: st.Reason})
		} else {
			sum.Accepted = append(sum.Accepted, a.Node)
			sum.Accepts = append(sum.Accepts, a)
		}
	}
	receipts := broadcasts(rows, r, req)
	sum.Broadcasts = len(receipts)
	covered := map[uint32]bool{}
	for _, b := range receipts {
		for _, tx := range b.msg.Transactions {
			covered[tx.TxIndex] = true
		}
	}
	sum.FullyBroadcast = len(covered) == len(req.Transactions)
	k, known := keys[string(r.ConversationID)]
	sum.Needed, sum.KeyKnown = k.t, known
	sum.Status = status(r, req, sessions, len(sum.Accepted), len(sum.Refused), k, now)
	return sum
}

// threshold is a Key's t of n: n is the Roster it was minted by.
type threshold struct{ t, n int }

// thresholds is each Key's t and n, by the Key's Conversation ID.
func thresholds(rows []cosl.Row) (map[string]threshold, error) {
	keys, err := key.List(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]threshold, len(keys))
	for _, k := range keys {
		conv := key.ConversationID(k.Curve, k.PublicKey)
		out[string(conv[:])] = threshold{k.T, k.N}
	}
	return out, nil
}

// List reads every Sign Request off rows, in log order.
func List(rows []cosl.Row, now time.Time) ([]Summary, error) {
	out := []Summary{}
	keys, err := thresholds(rows)
	if err != nil {
		return nil, err
	}
	sessions, err := session.List(rows)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.MsgType != cosl.TypeSignRequest {
			continue
		}
		req, err := decodeRow(r)
		if errors.Is(err, ErrUnknown) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, summarise(rows, sessions, keys, r, req, now))
	}
	return out, nil
}

// Inspect decodes the one Sign Request whose msg id starts with query. Each
// transaction shows the chain it names, as every Node sees it.
func Inspect(rows []cosl.Row, query string, now time.Time) (Detail, error) {
	m, err := match(rows, query)
	if err != nil {
		return Detail{}, err
	}
	req, err := decodeRow(m)
	if err != nil {
		return Detail{}, err
	}
	keys, err := thresholds(rows)
	if err != nil {
		return Detail{}, err
	}
	sessions, err := session.List(rows)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Summary: summarise(rows, sessions, keys, m, req, now), DeadlineSeconds: req.DeadlineSeconds, Details: []TxDetail{}, BroadcastReceipts: []BroadcastReceipt{}}
	for _, tx := range req.Transactions {
		c := ChainOf(tx)
		td := TxDetail{Chain: c.String(), Type: c.Type}
		if err := c.Validate(); err != nil {
			td.Lines = []string{"names no valid chain: " + err.Error() + "; intent not decoded"}
			d.Details = append(d.Details, td)
			continue
		}
		switch in := tx.Intent.(type) {
		case *coslv1.SignTransaction_Ethereum:
			td.Lines = describeEthereum(in.Ethereum)
			if _, dg, err := EthereumDigest(in.Ethereum.ChainId, in.Ethereum); err != nil {
				td.Lines = append(td.Lines, "digest unavailable: "+err.Error())
			} else {
				td.Digests = []string{hex.EncodeToString(dg[:])}
			}
		case *coslv1.SignTransaction_Bitcoin:
			td.Lines = describeBitcoin(in.Bitcoin)
			if dgs, err := BitcoinDigests(in.Bitcoin.Network, in.Bitcoin); err != nil {
				td.Lines = append(td.Lines, "digest unavailable: "+err.Error())
			} else {
				for _, dg := range dgs {
					td.Digests = append(td.Digests, hex.EncodeToString(dg[:]))
				}
			}
		case *coslv1.SignTransaction_Solana:
			td.Lines = describeSolana(in.Solana)
			if msg, err := SolanaMessage(in.Solana); err != nil {
				td.Lines = append(td.Lines, "digest unavailable: "+err.Error())
			} else {
				td.Digests = []string{hex.EncodeToString(msg)}
			}
		}
		d.Details = append(d.Details, td)
	}
	for _, b := range broadcasts(rows, m, req) {
		rc := BroadcastReceipt{
			Seq: b.row.Seq, Broadcaster: b.row.Sender.String(), At: time.Unix(0, b.row.CreatedAt).UTC().Format(time.RFC3339),
		}
		for _, tx := range b.msg.Transactions {
			c := ChainOf(req.Transactions[tx.TxIndex])
			rc.Transactions = append(rc.Transactions, BroadcastedTx{Index: tx.TxIndex, Chain: c.String(), TxHash: TxIDString(c, tx.TxHash)})
		}
		d.BroadcastReceipts = append(d.BroadcastReceipts, rc)
	}
	return d, nil
}

// TxIDString is how chain c writes a transaction id: base58 on Solana, else hex.
func TxIDString(c network.Chain, id []byte) string {
	if c.Type == network.Solana {
		return base58.Encode(id)
	}
	return hex.EncodeToString(id)
}

// wellFormed is a receipt covering a non-empty subset of the request's
// transactions, each entry once, with a transaction id (32 bytes, or a 64-byte
// Solana signature), all on one chain. Any Node may append one, so readers do
// not trust the shape.
func wellFormed(b *coslv1.SignBroadcast, req *coslv1.SignRequest) bool {
	if len(b.Transactions) == 0 {
		return false
	}
	seen := map[uint32]bool{}
	var first network.Chain
	for i, t := range b.Transactions {
		if (len(t.TxHash) != 32 && len(t.TxHash) != ed25519.SignatureSize) || int(t.TxIndex) >= len(req.Transactions) || seen[t.TxIndex] {
			return false
		}
		seen[t.TxIndex] = true
		c := ChainOf(req.Transactions[t.TxIndex])
		if i == 0 {
			first = c
		} else if c != first {
			return false
		}
	}
	return true
}

type receipt struct {
	row cosl.Row
	msg *coslv1.SignBroadcast
}

// broadcasts are the sign.broadcast rows for the request at row, in log order:
// same Key Conversation, naming the request. Every one is kept, so a second
// broadcast of the same transactions stays visible. A request has several
// receipts when it was broadcast more than once or its Networks were
// broadcast separately.
func broadcasts(rows []cosl.Row, row cosl.Row, req *coslv1.SignRequest) []receipt {
	var out []receipt
	for _, r := range rows {
		if r.MsgType != cosl.TypeSignBroadcast || r.Seq <= row.Seq || !bytes.Equal(r.ConversationID, row.ConversationID) {
			continue
		}
		msg, err := decodePayload(r)
		if err != nil {
			continue
		}
		if b := msg.(*coslv1.SignBroadcast); bytes.Equal(b.SignRequestId, row.MsgID[:]) && wellFormed(b, req) {
			out = append(out, receipt{r, b})
		}
	}
	return out
}

// match is the one well-formed sign.request row whose msg id starts with query.
func match(rows []cosl.Row, query string) (cosl.Row, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return cosl.Row{}, ErrUnknown
	}
	var found []cosl.Row
	for _, r := range rows {
		if r.MsgType != cosl.TypeSignRequest || !strings.HasPrefix(hex.EncodeToString(r.MsgID[:]), query) {
			continue
		}
		if _, err := decodeRow(r); errors.Is(err, ErrUnknown) {
			continue
		}
		found = append(found, r)
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

func describeEthereum(in *coslv1.EthereumIntent) []string {
	value := in.Value
	if value == "" {
		value = "0"
	}
	lines := []string{
		fmt.Sprintf("chain_id %d", in.ChainId),
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
