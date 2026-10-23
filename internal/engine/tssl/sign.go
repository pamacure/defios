package tssl

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"

	"github.com/bnb-chain/tss-lib/v4/common"
	ecdsakeygen "github.com/bnb-chain/tss-lib/v4/ecdsa/keygen"
	"github.com/bnb-chain/tss-lib/v4/ecdsa/signing"
	"github.com/bnb-chain/tss-lib/v4/tss"

	"defios.com/internal/session"
)

// NewSigner returns the signing engine (docs/adr/0010-signing-session-semantics.md).
// It runs tss-lib ecdsa/signing once per Digest, all in parallel inside one
// engine, and prefixes each message with the Digest's index so the COSL
// carries a single stream. EdDSA signing is not wired yet.
func NewSigner(self [32]byte) session.Engine {
	return &signer{
		self: self,
		out:  make(chan session.Outgoing, 64),
		done: make(chan session.Result, 1),
		stop: make(chan struct{}),
	}
}

type signer struct {
	self [32]byte
	out  chan session.Outgoing
	done chan session.Result
	stop chan struct{}

	mu      sync.Mutex
	parties []tss.Party
	byNode  map[[32]byte]*tss.PartyID
	sigs    [][]byte
	left    int
	started bool

	once     sync.Once
	stopOnce sync.Once
}

func (e *signer) Start(p session.Params) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.started:
		return fmt.Errorf("sign: engine already started")
	case p.Curve != "ecdsa":
		return fmt.Errorf("sign: curve %q is not supported", p.Curve)
	case len(p.Digests) == 0:
		return fmt.Errorf("sign: nothing to sign")
	}
	var save ecdsakeygen.LocalPartySaveData
	if err := json.Unmarshal(p.SaveData, &save); err != nil || save.ECDSAPub == nil {
		return fmt.Errorf("sign: this Node holds no share of the Key")
	}
	sorted, self, byNode, err := partyIDs("ecdsa", p.Participants, e.self)
	if err != nil {
		return err
	}
	threshold := p.T - 1
	if threshold < 1 || len(sorted) != p.T {
		return fmt.Errorf("sign: %d signers do not match t=%d", len(sorted), p.T)
	}
	e.byNode = byNode
	e.sigs = make([][]byte, len(p.Digests))
	e.left = len(p.Digests)
	e.started = true

	type run struct {
		party tss.Party
		out   chan tss.Message
		end   chan *common.SignatureData
	}
	runs := make([]run, len(p.Digests))
	for i, d := range p.Digests {
		params := tss.NewParameters(tss.S256(), tss.NewPeerContext(sorted), self, len(sorted), threshold)
		params.SetSessionNonce(digestNonce(p.Nonce, i))
		r := run{out: make(chan tss.Message, 32), end: make(chan *common.SignatureData, 1)}
		r.party = signing.NewLocalParty(new(big.Int).SetBytes(d[:]), params, save, r.out, r.end)
		runs[i] = r
		e.parties = append(e.parties, r.party)
		go e.loop(i, r.out, r.end)
	}
	for _, r := range runs {
		if err := r.party.Start(); err != nil {
			return err
		}
	}
	return nil
}

// digestNonce gives each Digest's run its own session nonce.
func digestNonce(nonce [32]byte, i int) *big.Int {
	h := sha256.New()
	h.Write(nonce[:])
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], uint32(i))
	h.Write(idx[:])
	return new(big.Int).SetBytes(h.Sum(nil))
}

func (e *signer) loop(i int, out chan tss.Message, end <-chan *common.SignatureData) {
	for {
		select {
		case <-e.stop:
			return
		case msg := <-out:
			e.forward(i, msg)
		case sig := <-end:
			e.complete(i, sig)
			return
		}
	}
}

func (e *signer) forward(i int, msg tss.Message) {
	bz, meta, err := msg.WireBytes()
	if err != nil {
		e.finish(session.Result{Err: err})
		return
	}
	framed := make([]byte, 4, 4+len(bz))
	binary.BigEndian.PutUint32(framed, uint32(i))
	framed = append(framed, bz...)
	if meta.IsBroadcast || len(meta.To) == 0 {
		e.send(session.Outgoing{Data: framed})
		return
	}
	for _, to := range meta.To {
		if id, ok := nodeID(to); ok {
			e.send(session.Outgoing{To: &id, Data: append([]byte(nil), framed...)})
		}
	}
}

func (e *signer) send(o session.Outgoing) {
	select {
	case <-e.stop:
	case e.out <- o:
	}
}

// complete records Digest i's signature as r||s||v and finishes once all are in.
func (e *signer) complete(i int, sig *common.SignatureData) {
	if sig == nil || len(sig.R) == 0 || len(sig.S) == 0 || len(sig.SignatureRecovery) == 0 {
		e.finish(session.Result{Err: fmt.Errorf("sign: digest %d finished without a signature", i)})
		return
	}
	raw := make([]byte, 65)
	new(big.Int).SetBytes(sig.R).FillBytes(raw[:32])
	new(big.Int).SetBytes(sig.S).FillBytes(raw[32:64])
	raw[64] = sig.SignatureRecovery[0]

	e.mu.Lock()
	e.sigs[i] = raw
	e.left--
	left, sigs := e.left, e.sigs
	e.mu.Unlock()
	if left == 0 {
		e.finish(session.Result{Signatures: sigs})
	}
}

func (e *signer) finish(res session.Result) {
	e.once.Do(func() {
		select {
		case <-e.stop:
		case e.done <- res:
		}
	})
}

func (e *signer) Receive(from [32]byte, isBroadcast bool, data []byte) error {
	e.mu.Lock()
	parties, byNode := e.parties, e.byNode
	e.mu.Unlock()
	if len(data) < 4 {
		return fmt.Errorf("sign: short message")
	}
	i := int(binary.BigEndian.Uint32(data))
	if i >= len(parties) {
		return fmt.Errorf("sign: message for digest %d of %d", i, len(parties))
	}
	pid := byNode[from]
	if pid == nil {
		return fmt.Errorf("sign: sender is not a signer")
	}
	if _, err := parties[i].UpdateFromBytes(data[4:], pid, isBroadcast); err != nil {
		return err
	}
	return nil
}

func (e *signer) Out() <-chan session.Outgoing { return e.out }

func (e *signer) Done() <-chan session.Result { return e.done }

func (e *signer) WaitingFor() [][32]byte {
	e.mu.Lock()
	parties := e.parties
	e.mu.Unlock()
	seen := map[[32]byte]bool{}
	var out [][32]byte
	for _, p := range parties {
		for _, id := range p.WaitingFor() {
			if n, ok := nodeID(id); ok && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

func (e *signer) Abort() { e.stopOnce.Do(func() { close(e.stop) }) }
