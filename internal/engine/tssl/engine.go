// Package tssl is the tss-lib keygen adapter (docs/spec/mvp.md §7.3).
// ECDSA and Ed25519 share PartyID, session nonce, and threshold mapping.
// EdDSA keygen is 3 rounds; ECDSA keygen is 4. Party state is not resumed.
package tssl

import (
	"crypto/elliptic"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"

	"github.com/bnb-chain/tss-lib/v4/crypto"
	ecdsakeygen "github.com/bnb-chain/tss-lib/v4/ecdsa/keygen"
	eddsakeygen "github.com/bnb-chain/tss-lib/v4/eddsa/keygen"
	"github.com/bnb-chain/tss-lib/v4/tss"

	"defios.com/internal/session"
)

// New returns a KeygenEngine that runs the curve named on the Session.
func New(self [32]byte) session.Engine { return newEngine(self, "") }

// NewECDSA returns a KeygenEngine locked to secp256k1. self is that Node's ID.
func NewECDSA(self [32]byte) session.Engine { return newEngine(self, "ecdsa") }

// NewEdDSA returns a KeygenEngine locked to Ed25519. self is that Node's ID.
func NewEdDSA(self [32]byte) session.Engine { return newEngine(self, "eddsa") }

type minted struct {
	point *crypto.ECPoint
	save  []byte
	err   error
}

type protocol struct {
	name   string
	curve  elliptic.Curve
	export func(*crypto.ECPoint) ([]byte, error)
	start  func(*tss.Parameters, chan<- tss.Message, []byte) (tss.Party, <-chan minted, error)
}

func protocols() map[string]protocol {
	return map[string]protocol{
		"ecdsa": {name: "ecdsa", curve: tss.S256(), export: CanonicalECDSA, start: startECDSA},
		"eddsa": {name: "eddsa", curve: tss.Edwards(), export: CanonicalEd25519, start: startEdDSA},
	}
}

func startECDSA(params *tss.Parameters, out chan<- tss.Message, pre []byte) (tss.Party, <-chan minted, error) {
	var pp ecdsakeygen.LocalPreParams
	if err := json.Unmarshal(pre, &pp); err != nil || !pp.ValidateWithProof() {
		return nil, nil, fmt.Errorf("ecdsa: preparams are missing")
	}
	end := make(chan *ecdsakeygen.LocalPartySaveData, 1)
	party := ecdsakeygen.NewLocalParty(params, out, end, pp)
	done := make(chan minted, 1)
	go func() {
		save, ok := <-end
		if !ok {
			return
		}
		if save == nil || save.ECDSAPub == nil {
			done <- minted{err: fmt.Errorf("ecdsa: keygen finished without a public key")}
			return
		}
		raw, err := json.Marshal(save)
		if err != nil {
			done <- minted{err: err}
			return
		}
		done <- minted{point: save.ECDSAPub, save: raw}
	}()
	return party, done, nil
}

func startEdDSA(params *tss.Parameters, out chan<- tss.Message, _ []byte) (tss.Party, <-chan minted, error) {
	end := make(chan *eddsakeygen.LocalPartySaveData, 1)
	party := eddsakeygen.NewLocalParty(params, out, end)
	done := make(chan minted, 1)
	go func() {
		save, ok := <-end
		if !ok {
			return
		}
		if save == nil || save.EDDSAPub == nil {
			done <- minted{err: fmt.Errorf("eddsa: keygen finished without a public key")}
			return
		}
		raw, err := json.Marshal(save)
		if err != nil {
			done <- minted{err: err}
			return
		}
		done <- minted{point: save.EDDSAPub, save: raw}
	}()
	return party, done, nil
}

type engine struct {
	self [32]byte
	only string // empty accepts either curve
	name string
	out  chan session.Outgoing
	done chan session.Result
	stop chan struct{}

	mu       sync.Mutex
	party    tss.Party
	byNode   map[[32]byte]*tss.PartyID
	export   func(*crypto.ECPoint) ([]byte, error)
	started  bool
	once     sync.Once
	stopOnce sync.Once
}

func newEngine(self [32]byte, only string) *engine {
	name := only
	if name == "" {
		name = "keygen"
	}
	return &engine{
		self: self,
		only: only,
		name: name,
		out:  make(chan session.Outgoing, 32),
		done: make(chan session.Result, 1),
		stop: make(chan struct{}),
	}
}

func (e *engine) Start(p session.Params) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return fmt.Errorf("%s: engine already started", e.name)
	}
	if e.only != "" && p.Curve != e.only {
		return fmt.Errorf("%s: curve %q is not supported", e.only, p.Curve)
	}
	proto, ok := protocols()[p.Curve]
	if !ok {
		return fmt.Errorf("%s: curve %q is not supported", e.name, p.Curve)
	}
	sorted, self, byNode, err := partyIDs(proto.name, p.Participants, e.self)
	if err != nil {
		return err
	}
	threshold := p.T - 1
	if threshold < 1 || threshold >= len(sorted) {
		return fmt.Errorf("%s: t=%d is not valid for n=%d", proto.name, p.T, len(sorted))
	}
	nonce := new(big.Int).SetBytes(p.Nonce[:])
	if nonce.Sign() <= 0 {
		return fmt.Errorf("%s: session nonce must be positive", proto.name)
	}
	params := tss.NewParameters(proto.curve, tss.NewPeerContext(sorted), self, len(sorted), threshold)
	params.SetSessionNonce(nonce)

	partyOut := make(chan tss.Message, 32)
	party, finished, err := proto.start(params, partyOut, p.Preparams)
	if err != nil {
		return err
	}
	e.party = party
	e.byNode = byNode
	e.export = proto.export
	e.name = proto.name
	e.started = true
	go e.loop(partyOut, finished)
	if err := party.Start(); err != nil {
		return err
	}
	return nil
}

func (e *engine) loop(partyOut chan tss.Message, end <-chan minted) {
	for {
		select {
		case <-e.stop:
			return
		case msg, ok := <-partyOut:
			if !ok {
				return
			}
			e.forward(msg)
		case save, ok := <-end:
			if !ok {
				return
			}
			e.succeed(save)
			return
		}
	}
}

func (e *engine) forward(msg tss.Message) {
	bz, meta, err := msg.WireBytes()
	if err != nil {
		e.fail(err)
		return
	}
	if meta.IsBroadcast || len(meta.To) == 0 {
		e.send(session.Outgoing{Data: append([]byte(nil), bz...)})
		return
	}
	for _, to := range meta.To {
		id, ok := nodeID(to)
		if !ok {
			continue
		}
		e.send(session.Outgoing{To: &id, Data: append([]byte(nil), bz...)})
	}
}

func (e *engine) send(o session.Outgoing) {
	select {
	case <-e.stop:
	case e.out <- o:
	}
}

func (e *engine) succeed(save minted) {
	if save.err != nil {
		e.fail(save.err)
		return
	}
	pk, err := e.export(save.point)
	if err != nil {
		e.fail(err)
		return
	}
	e.finish(session.Result{PublicKey: pk, SaveData: save.save})
}

func (e *engine) fail(err error) {
	e.finish(session.Result{Err: err})
}

func (e *engine) finish(res session.Result) {
	e.once.Do(func() {
		select {
		case <-e.stop:
		case e.done <- res:
		}
	})
}

func (e *engine) Receive(from [32]byte, isBroadcast bool, data []byte) error {
	e.mu.Lock()
	party, byNode := e.party, e.byNode
	name := e.name
	e.mu.Unlock()
	if party == nil {
		return fmt.Errorf("%s: engine has not started", name)
	}
	pid := byNode[from]
	if pid == nil {
		return fmt.Errorf("%s: sender is not a participant", name)
	}
	if _, err := party.UpdateFromBytes(data, pid, isBroadcast); err != nil {
		return err
	}
	return nil
}

func (e *engine) Out() <-chan session.Outgoing { return e.out }

func (e *engine) Done() <-chan session.Result { return e.done }

func (e *engine) WaitingFor() [][32]byte {
	e.mu.Lock()
	party := e.party
	e.mu.Unlock()
	if party == nil {
		return nil
	}
	waiting := party.WaitingFor()
	out := make([][32]byte, 0, len(waiting))
	for _, id := range waiting {
		n, ok := nodeID(id)
		if ok {
			out = append(out, n)
		}
	}
	return out
}

func (e *engine) Abort() {
	e.stopOnce.Do(func() { close(e.stop) })
}

// partyIDs builds tss-lib identities. PartyID.Key is the Node ID as a
// big-endian integer; PartyID.Id is the hex Node ID; indices come from
// SortPartyIDs.
func partyIDs(curve string, participants [][32]byte, self [32]byte) (tss.SortedPartyIDs, *tss.PartyID, map[[32]byte]*tss.PartyID, error) {
	if len(participants) == 0 {
		return nil, nil, nil, fmt.Errorf("%s: no participants", curve)
	}
	unsorted := make(tss.UnSortedPartyIDs, len(participants))
	for i, raw := range participants {
		unsorted[i] = tss.NewPartyID(hex.EncodeToString(raw[:]), "", new(big.Int).SetBytes(raw[:]))
	}
	sorted := tss.SortPartyIDs(unsorted)
	byNode := make(map[[32]byte]*tss.PartyID, len(sorted))
	var mine *tss.PartyID
	for _, id := range sorted {
		n, ok := nodeID(id)
		if !ok {
			return nil, nil, nil, fmt.Errorf("%s: party id has no key", curve)
		}
		byNode[n] = id
		if n == self {
			mine = id
		}
	}
	if mine == nil {
		return nil, nil, nil, fmt.Errorf("%s: this Node is not a participant", curve)
	}
	return sorted, mine, byNode, nil
}

func nodeID(id *tss.PartyID) ([32]byte, bool) {
	var out [32]byte
	if id == nil || id.MessageWrapper_PartyID == nil {
		return out, false
	}
	b := id.KeyInt().Bytes()
	if len(b) > 32 {
		return out, false
	}
	copy(out[32-len(b):], b)
	return out, true
}
