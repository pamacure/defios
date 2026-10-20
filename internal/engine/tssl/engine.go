// Package tssl is the tss-lib keygen adapter (docs/spec/mvp.md §7.3).
// ECDSA only; Ed25519 is a later ticket. Party state is not resumed.
package tssl

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"

	"github.com/bnb-chain/tss-lib/v4/ecdsa/keygen"
	"github.com/bnb-chain/tss-lib/v4/tss"

	"defios.com/internal/session"
)

// NewECDSA returns a KeygenEngine for one Node. self is that Node's ID.
func NewECDSA(self [32]byte) session.Engine {
	return &engine{
		self: self,
		out:  make(chan session.Outgoing, 32),
		done: make(chan session.Result, 1),
		stop: make(chan struct{}),
	}
}

type engine struct {
	self [32]byte
	out  chan session.Outgoing
	done chan session.Result
	stop chan struct{}

	mu       sync.Mutex
	party    tss.Party
	byNode   map[[32]byte]*tss.PartyID
	started  bool
	once     sync.Once
	stopOnce sync.Once
}

func (e *engine) Start(p session.Params) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return fmt.Errorf("ecdsa: engine already started")
	}
	if p.Curve != "ecdsa" {
		return fmt.Errorf("ecdsa: curve %q is not supported", p.Curve)
	}
	var pre keygen.LocalPreParams
	if err := json.Unmarshal(p.Preparams, &pre); err != nil || !pre.ValidateWithProof() {
		return fmt.Errorf("ecdsa: preparams are missing")
	}
	sorted, self, byNode, err := partyIDs(p.Participants, e.self)
	if err != nil {
		return err
	}
	threshold := p.T - 1
	if threshold < 1 || threshold >= len(sorted) {
		return fmt.Errorf("ecdsa: t=%d is not valid for n=%d", p.T, len(sorted))
	}
	nonce := new(big.Int).SetBytes(p.Nonce[:])
	if nonce.Sign() <= 0 {
		return fmt.Errorf("ecdsa: session nonce must be positive")
	}
	params := tss.NewParameters(tss.S256(), tss.NewPeerContext(sorted), self, len(sorted), threshold)
	params.SetSessionNonce(nonce)

	partyOut := make(chan tss.Message, 32)
	end := make(chan *keygen.LocalPartySaveData, 1)
	party := keygen.NewLocalParty(params, partyOut, end, pre)
	e.party = party
	e.byNode = byNode
	e.started = true
	go e.loop(partyOut, end)
	if err := party.Start(); err != nil {
		return err
	}
	return nil
}

func (e *engine) loop(partyOut chan tss.Message, end chan *keygen.LocalPartySaveData) {
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

func (e *engine) succeed(save *keygen.LocalPartySaveData) {
	if save == nil || save.ECDSAPub == nil {
		e.fail(fmt.Errorf("ecdsa: keygen finished without a public key"))
		return
	}
	pk, err := CanonicalECDSA(save.ECDSAPub)
	if err != nil {
		e.fail(err)
		return
	}
	raw, err := json.Marshal(save)
	if err != nil {
		e.fail(err)
		return
	}
	e.finish(session.Result{PublicKey: pk, SaveData: raw})
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
	e.mu.Unlock()
	if party == nil {
		return fmt.Errorf("ecdsa: engine has not started")
	}
	pid := byNode[from]
	if pid == nil {
		return fmt.Errorf("ecdsa: sender is not a participant")
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
func partyIDs(participants [][32]byte, self [32]byte) (tss.SortedPartyIDs, *tss.PartyID, map[[32]byte]*tss.PartyID, error) {
	if len(participants) == 0 {
		return nil, nil, nil, fmt.Errorf("ecdsa: no participants")
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
			return nil, nil, nil, fmt.Errorf("ecdsa: party id has no key")
		}
		byNode[n] = id
		if n == self {
			mine = id
		}
	}
	if mine == nil {
		return nil, nil, nil, fmt.Errorf("ecdsa: this Node is not a participant")
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
