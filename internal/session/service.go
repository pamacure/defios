package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

const (
	// DefaultWaitingFor is how long a live Session may sit before any Node
	// aborts it (§7.4). Tests inject a shorter wait.
	DefaultWaitingFor = 2 * time.Minute

	reasonPartyLost     = "party state lost"
	reasonWaitingAccept = "waiting for accept"
	reasonTimedOut      = "timed out waiting for parties"
	reasonDisagree      = "completes disagree"
	reasonExpired       = "sign request expired"
	reasonDeclined      = "declined"
)

// ShareStore is where a minted share is written before session.complete
// and deleted again if the Session aborts or the completes disagree.
type ShareStore interface {
	Put(curve string, publicKey, saveData []byte) error
	Delete(curve string, publicKey []byte) error
	// Get is the save data of a held share, for a signing Session.
	Get(curve string, publicKey []byte) ([]byte, bool)
}

// SignInfo is what a signing Session needs from its sign.request: the Key,
// the Digests every Node derives from it, and when it lapses. The caller
// reads it off the COSL; this package does not know transactions.
type SignInfo struct {
	Curve     string
	PublicKey []byte
	T         int // the Key's t
	Digests   [][32]byte
	Deadline  time.Time
	// Conversation is the Key's Conversation ID, which a sign.accept is scoped to.
	Conversation []byte
	// Accepted is who has accepted the request, in log order, the proposer
	// first. The first T of them are the signers.
	Accepted []SignAccept
}

// SignAccept is one Node's accept of a sign.request, at its COSL position.
type SignAccept struct {
	Node identity.ID
	Seq  uint64
}

// signers are the first t Nodes to have accepted before seq (0 = no bound),
// or fewer while t have not.
func (i SignInfo) signers(before uint64) []identity.ID {
	var out []identity.ID
	for _, a := range i.Accepted {
		if len(out) == i.T {
			break
		}
		if before == 0 || a.Seq < before {
			out = append(out, a.Node)
		}
	}
	return out
}

// Config is one Node's Session layer.
type Config struct {
	Node   *identity.Node
	Roster group.Roster
	Store  *cosl.Store
	Append func(context.Context, cosl.Signed) error
	// NewEngine is called once per Session, when this Node's engine starts.
	NewEngine func() Engine
	// NewSigner is called once per signing Session. Nil refuses to sign.
	NewSigner func() Engine
	// SignInfo resolves a sign.request by msg_id. Nil refuses to sign.
	SignInfo func(requestID [32]byte) (SignInfo, error)
	// Preparams are this Node's ECDSA preparams, already unlocked. They are
	// passed through on an ecdsa Start and are not a COSL field.
	Preparams []byte
	// WaitingFor overrides DefaultWaitingFor when non-zero.
	WaitingFor time.Duration
	// Now is the clock. Nil means time.Now.
	Now  func() time.Time
	Logf func(string, ...any)
	// AcceptPath is the local-accept file. A restart finds Sessions this
	// Node already accepted and aborts them: the in-memory party is gone
	// and cannot be resumed. Empty keeps accepts in memory only.
	AcceptPath string
	// Shares is the unlocked Vault. Nil skips minting (tests that do not
	// finish a Key). PendingPath remembers a share that is in the Vault
	// but not yet a minted Key, so a restart can delete it on abort.
	Shares      ShareStore
	PendingPath string
	// Tick is how often the replica is polled. Zero means 50ms.
	Tick time.Duration
}

// Service is one Node's Session layer. Run watches the replica. Start,
// Accept, and Abort are the verbs.
type Service struct {
	cfg      Config
	mu       sync.Mutex
	pending  map[ID]struct{} // local accept in this process
	onDisk   map[ID]struct{} // local accept durable across restarts
	inflight map[ID]struct{} // abort already submitted
	failed   map[ID]struct{} // do not start another engine
	aborting map[ID]string   // abort not yet durable; retry this reason
	held     map[ID]shareRef // share in the Vault, Key not minted yet
	parties  map[ID]*party
	waitLog  map[ID]struct{}
	signs    map[ID]SignInfo       // resolved sign.requests, by Session
	starting map[[32]byte]struct{} // sign.requests this Node has appended a start for
}

type shareRef struct {
	Curve     string
	PublicKey []byte
}

type party struct {
	engine     Engine
	cancel     context.CancelFunc
	head       uint64
	fed        uint64
	replayed   bool
	replayDone chan struct{}
	once       sync.Once
	waitSince  time.Time
	minted     bool

	mu      sync.Mutex
	result  Result
	doneSet bool
}

func (p *party) finishReplay() { p.once.Do(func() { close(p.replayDone) }) }

func (p *party) setDone(res Result) {
	p.mu.Lock()
	p.result, p.doneSet = res, true
	p.mu.Unlock()
}

func (p *party) takeDone() (Result, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result, p.doneSet
}

// New prepares a Service. It does not watch the replica until Run.
func New(cfg Config) (*Service, error) {
	if cfg.Node == nil || cfg.Store == nil || cfg.Append == nil || cfg.NewEngine == nil {
		return nil, errors.New("session: incomplete config")
	}
	if cfg.WaitingFor <= 0 {
		cfg.WaitingFor = DefaultWaitingFor
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 50 * time.Millisecond
	}
	onDisk := map[ID]struct{}{}
	if cfg.AcceptPath != "" {
		ids, err := loadAccepts(cfg.AcceptPath)
		if err != nil {
			return nil, err
		}
		onDisk = ids
	}
	held, err := loadPending(cfg.PendingPath)
	if err != nil {
		return nil, err
	}
	return &Service{
		cfg:      cfg,
		pending:  map[ID]struct{}{},
		onDisk:   onDisk,
		inflight: map[ID]struct{}{},
		failed:   map[ID]struct{}{},
		aborting: map[ID]string{},
		held:     held,
		parties:  map[ID]*party{},
		waitLog:  map[ID]struct{}{},
		signs:    map[ID]SignInfo{},
		starting: map[[32]byte]struct{}{},
	}, nil
}

// Run watches the replica until ctx is done. Cancelling ctx drops
// in-memory parties and does not itself append session.abort; a later Run
// sees the lost party and aborts.
func (s *Service) Run(ctx context.Context) {
	defer s.shutdown()
	s.poll(ctx)
	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.poll(ctx)
		}
	}
}

// Start appends session.start. n is the Roster. The plaintext nonce is
// sealed to each participant and is not a COSL field. This call is the
// initiator's local accept.
func (s *Service) Start(ctx context.Context, curve string, t int) (Summary, error) {
	if curve != "ecdsa" && curve != "eddsa" {
		return Summary{}, ErrBadCurve
	}
	if t < 0 {
		return Summary{}, fmt.Errorf("session: t must be >= 0")
	}
	var id ID
	var nonce [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Summary{}, fmt.Errorf("session: %w", err)
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		return Summary{}, fmt.Errorf("session: %w", err)
	}
	nodes := s.cfg.Roster.Nodes()
	boxes := make([]*coslv1.SealedBox, 0, len(nodes))
	parts := make([][]byte, 0, len(nodes))
	for _, node := range nodes {
		eph, n, ct, err := seal(nonce[:], node.EncryptionPublicKey)
		if err != nil {
			return Summary{}, err
		}
		boxes = append(boxes, &coslv1.SealedBox{
			To:                 append([]byte(nil), node.NodeID[:]...),
			EphemeralPublicKey: eph,
			Nonce:              n,
			Ciphertext:         ct,
		})
		parts = append(parts, append([]byte(nil), node.NodeID[:]...))
	}
	payload := &coslv1.SessionStart{
		SessionId:    append([]byte(nil), id[:]...),
		Curve:        curve,
		T:            uint32(t),
		N:            uint32(len(nodes)),
		Participants: parts,
		NonceBoxes:   boxes,
	}
	if err := s.remember(id); err != nil {
		return Summary{}, err
	}
	if err := s.signAndAppend(ctx, cosl.TypeSessionStart, payload); err != nil {
		s.forget(id)
		return Summary{}, err
	}
	return Summary{SessionID: id.String(), Status: Open, Curve: curve, T: uint32(t), N: uint32(len(nodes))}, nil
}

// Accept is this Node's local engine gate for a Session it did not start.
// Nothing is written to the COSL.
func (s *Service) Accept(_ context.Context, id ID) error {
	recs, err := s.records()
	if err != nil {
		return err
	}
	r := find(recs, id)
	if r == nil {
		return ErrUnknownSession
	}
	if r.status != Open {
		return ErrClosedSession
	}
	self := s.cfg.Node.NodeID
	for _, p := range r.participants {
		if p == self {
			return s.remember(id)
		}
	}
	return ErrNotParticipant
}

// Abort appends session.abort. A second abort for an already-closed
// Session is a no-op.
func (s *Service) Abort(ctx context.Context, id ID, reason string) error {
	recs, err := s.records()
	if err != nil {
		return err
	}
	r := find(recs, id)
	if r == nil {
		return ErrUnknownSession
	}
	if r.status != Open {
		return nil
	}
	if err := s.abort(ctx, id, reason); err != nil {
		return err
	}
	s.dropShare(id)
	return nil
}

func (s *Service) poll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	rows, err := s.cfg.Store.Log(cosl.Query{})
	if err != nil {
		s.cfg.Logf("session: reading replica: %v", err)
		return
	}
	recs, err := parse(rows, s.verifySigned)
	if err != nil {
		s.cfg.Logf("session: %v", err)
		return
	}
	if !s.validateAll(recs) {
		return
	}
	s.startAccepted(ctx, recs)
	runnable, need := plan(recs)
	var head uint64
	if len(rows) > 0 {
		head = rows[len(rows)-1].Seq
	}
	for _, r := range recs {
		if r.status != Open {
			s.stop(r.id)
			s.forget(r.gate())
			delete(s.aborting, r.id)
			if r.status == Aborted {
				s.dropShare(r.id)
			} else {
				s.forgetShare(r.id)
			}
			continue
		}
		if r.conflict {
			s.giveUp(ctx, r.id, reasonDisagree)
			continue
		}
		if _, ok := s.aborting[r.id]; ok {
			s.giveUp(ctx, r.id, s.aborting[r.id])
			continue
		}
		if reason, ok := need[r.id]; ok {
			s.giveUp(ctx, r.id, reason)
			continue
		}
		if info, err := s.signInfo(r); err == nil && s.cfg.Now().After(info.Deadline) {
			s.giveUp(ctx, r.id, reasonExpired)
			continue
		}
		if s.lost(r) {
			s.giveUp(ctx, r.id, reasonPartyLost)
			continue
		}
		if runnable == nil || *runnable != r.id {
			continue
		}
		if !s.pendingNow(r.gate()) {
			s.logWaiting(r)
			if s.cfg.Now().Sub(time.Unix(0, r.createdAt)) > s.cfg.WaitingFor {
				s.giveUp(ctx, r.id, reasonWaitingAccept)
			}
			continue
		}
		s.ensureParty(ctx, r, head)
		s.feed(ctx, r)
		s.checkWait(ctx, r)
	}
}

func (s *Service) logWaiting(r *rec) {
	if _, ok := s.waitLog[r.id]; ok {
		return
	}
	s.waitLog[r.id] = struct{}{}
	s.cfg.Logf("session %s waiting for accept", r.id)
}

func (s *Service) ensureParty(ctx context.Context, r *rec, head uint64) {
	if _, ok := s.parties[r.id]; ok {
		return
	}
	if _, ok := s.failed[r.id]; ok {
		return
	}
	if !s.pendingNow(r.gate()) {
		return
	}
	eng, params, err := s.engineFor(r)
	if err != nil {
		s.giveUp(ctx, r.id, err.Error())
		return
	}
	pctx, cancel := context.WithCancel(ctx)
	p := &party{engine: eng, cancel: cancel, head: head, replayDone: make(chan struct{})}
	s.parties[r.id] = p
	go s.pump(pctx, r.id, p)
	if err := eng.Start(params); err != nil {
		s.giveUp(ctx, r.id, err.Error())
	}
}

// engineFor builds the engine and Params for a Session: keygen, or a signing
// Session that loads this Node's share and the request's Digests.
func (s *Service) engineFor(r *rec) (Engine, Params, error) {
	var eng Engine
	if r.isSign {
		if s.cfg.NewSigner != nil {
			eng = s.cfg.NewSigner()
		}
	} else {
		eng = s.cfg.NewEngine()
	}
	if eng == nil {
		return nil, Params{}, ErrNoEngine
	}
	nonce, err := unsealNonce(r.boxes, s.cfg.Node.NodeID, s.cfg.Node.EncryptionPrivateKey)
	if err != nil {
		return nil, Params{}, err
	}
	params := Params{
		SessionID:    [32]byte(r.id),
		Curve:        r.curve,
		T:            int(r.t),
		N:            int(r.n),
		Participants: participants(r),
		Nonce:        nonce,
	}
	if r.isSign {
		info, err := s.signInfo(r)
		if err != nil {
			return nil, Params{}, err
		}
		if s.cfg.Shares == nil {
			return nil, Params{}, errors.New("session: no vault")
		}
		save, ok := s.cfg.Shares.Get(info.Curve, info.PublicKey)
		if !ok {
			return nil, Params{}, errors.New("session: this Node holds no share of the Key")
		}
		params.Digests, params.SaveData = info.Digests, save
	} else if r.curve == "ecdsa" && len(s.cfg.Preparams) > 0 {
		params.Preparams = append([]byte(nil), s.cfg.Preparams...)
	}
	return eng, params, nil
}

func (s *Service) feed(ctx context.Context, r *rec) {
	p := s.parties[r.id]
	if p == nil {
		return
	}
	for _, round := range r.rounds {
		if round.seq <= p.fed {
			continue
		}
		if !p.replayed && round.seq > p.head {
			break
		}
		if err := s.deliver(p, round); err != nil {
			s.giveUp(ctx, r.id, err.Error())
			return
		}
		p.fed = round.seq
	}
	if !p.replayed {
		p.replayed = true
		p.finishReplay()
	}
	if res, ok := p.takeDone(); ok {
		if res.Err != nil {
			s.giveUp(ctx, r.id, res.Err.Error())
			return
		}
		switch {
		case p.minted:
		case r.isSign:
			s.finishSign(ctx, r, p, res)
		default:
			s.mint(ctx, r, p, res)
		}
	}
}

// deliver feeds one committed tss.session into the engine. Own rows are
// skipped. A routing tag is who the engine addressed, not where the row
// was delivered: every replica already has it.
func (s *Service) deliver(p *party, round round) error {
	if round.sender == s.cfg.Node.NodeID {
		return nil
	}
	tag := round.msg.RoutingTag
	var data []byte
	broadcast := len(tag) == 0
	if broadcast {
		data = append([]byte(nil), round.msg.Data...)
	} else {
		if len(tag) != 32 || !bytes.Equal(tag, s.cfg.Node.NodeID[:]) {
			return nil
		}
		plain, ok := open(round.msg.Data, round.msg.Nonce, round.msg.EphemeralPublicKey, s.cfg.Node.EncryptionPrivateKey)
		if !ok {
			return fmt.Errorf("session: cannot unseal tss.session")
		}
		data = plain
	}
	return p.engine.Receive([32]byte(round.sender), broadcast, data)
}

func (s *Service) checkWait(ctx context.Context, r *rec) {
	p := s.parties[r.id]
	if p == nil || !p.replayed || p.minted {
		return
	}
	if res, ok := p.takeDone(); ok && res.Err == nil {
		return
	}
	if len(p.engine.WaitingFor()) == 0 {
		p.waitSince = time.Time{}
		return
	}
	if p.waitSince.IsZero() {
		p.waitSince = s.cfg.Now()
		return
	}
	if s.cfg.Now().Sub(p.waitSince) > s.cfg.WaitingFor {
		s.giveUp(ctx, r.id, reasonTimedOut)
	}
}

// giveUp stops the engine and appends session.abort. A failed append is
// remembered and retried; the engine is not started again.
func (s *Service) giveUp(ctx context.Context, id ID, reason string) {
	s.stop(id)
	s.dropShare(id)
	s.fail(id)
	if _, ok := s.aborting[id]; !ok {
		s.aborting[id] = reason
	}
	if err := s.abort(ctx, id, s.aborting[id]); err != nil {
		s.cfg.Logf("session: abort %s: %v", id, err)
		return
	}
	delete(s.aborting, id)
}

func (s *Service) pump(ctx context.Context, id ID, p *party) {
	var held []Outgoing
	replaying := true
	replay := p.replayDone
	out := p.engine.Out()
	done := p.engine.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-replay:
			replay = nil
			replaying = false
			for _, o := range held {
				s.emit(ctx, id, o)
			}
			held = nil
		case o, ok := <-out:
			if !ok {
				return
			}
			o = cloneOut(o)
			if replaying {
				held = append(held, o)
				continue
			}
			s.emit(ctx, id, o)
		case res, ok := <-done:
			if !ok {
				return
			}
			p.setDone(res)
			return
		}
	}
}

func (s *Service) emit(ctx context.Context, id ID, o Outgoing) {
	if ctx.Err() != nil {
		return
	}
	msg := &coslv1.TssSession{SessionId: append([]byte(nil), id[:]...), Data: append([]byte(nil), o.Data...)}
	if o.To != nil {
		msg.RoutingTag = append([]byte(nil), o.To[:]...)
		recipient, ok := s.cfg.Roster.Lookup(identity.ID(*o.To))
		if !ok {
			s.cfg.Logf("session: recipient %x is not on the roster", o.To[:])
			return
		}
		eph, nonce, ct, err := seal(o.Data, recipient.EncryptionPublicKey)
		if err != nil {
			s.cfg.Logf("session: seal: %v", err)
			return
		}
		msg.EphemeralPublicKey = eph
		msg.Nonce = nonce
		msg.Data = ct
	}
	if err := s.signAndAppend(ctx, cosl.TypeTssSession, msg); err != nil && ctx.Err() == nil {
		s.cfg.Logf("session: tss.session: %v", err)
	}
}

func cloneOut(o Outgoing) Outgoing {
	o.Data = append([]byte(nil), o.Data...)
	if o.To != nil {
		c := *o.To
		o.To = &c
	}
	return o
}

func (s *Service) abort(ctx context.Context, id ID, reason string) error {
	s.mu.Lock()
	if _, ok := s.inflight[id]; ok {
		s.mu.Unlock()
		return nil
	}
	s.inflight[id] = struct{}{}
	s.mu.Unlock()
	err := s.signAndAppend(ctx, cosl.TypeSessionAbort, &coslv1.SessionAbort{
		SessionId: append([]byte(nil), id[:]...),
		Reason:    reason,
	})
	if err != nil {
		s.mu.Lock()
		delete(s.inflight, id)
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *Service) signAndAppend(ctx context.Context, typ uint32, payload proto.Message) error {
	signed, err := s.author().Sign(typ, nil, payload)
	if err != nil {
		return err
	}
	return s.cfg.Append(ctx, signed)
}

func (s *Service) author() cosl.Author {
	return cosl.Author{Node: s.cfg.Node, GroupID: s.cfg.Roster.GroupID()}
}

func (s *Service) records() ([]*rec, error) {
	rows, err := s.cfg.Store.Log(cosl.Query{})
	if err != nil {
		return nil, err
	}
	recs, err := parse(rows, s.verifySigned)
	if err != nil {
		return nil, err
	}
	s.validateAll(recs) // a sign.request that cannot be read leaves its Session unvalidated
	return recs, nil
}

func (s *Service) pendingNow(id ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pending[id]
	return ok
}

// lost reports a Session this Node accepted whose party is not in memory.
// The initiator's start row counts as that accept even if the accept file
// was not written. A fresh accept in this process is pending and is not lost.
func (s *Service) lost(r *rec) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[r.gate()]; ok {
		return false
	}
	if _, ok := s.parties[r.id]; ok {
		return false
	}
	if r.sender == s.cfg.Node.NodeID {
		return true
	}
	_, ok := s.onDisk[r.gate()]
	return ok
}

func (s *Service) remember(id ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[id] = struct{}{}
	s.onDisk[id] = struct{}{}
	return s.saveLocked()
}

func (s *Service) forget(id ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, p := s.pending[id]
	_, d := s.onDisk[id]
	if !p && !d {
		return
	}
	delete(s.pending, id)
	delete(s.onDisk, id)
	if err := s.saveLocked(); err != nil {
		s.cfg.Logf("session: %v", err)
	}
}

func (s *Service) fail(id ID) { s.failed[id] = struct{}{} }

func (s *Service) stop(id ID) {
	p, ok := s.parties[id]
	if !ok {
		return
	}
	delete(s.parties, id)
	p.cancel()
	p.engine.Abort()
}

func (s *Service) shutdown() {
	for id := range s.parties {
		s.stop(id)
	}
}

func participants(r *rec) [][32]byte {
	out := make([][32]byte, len(r.participants))
	for i, id := range r.participants {
		out[i] = [32]byte(id)
	}
	return out
}

func unsealNonce(boxes []*coslv1.SealedBox, self identity.ID, priv []byte) ([32]byte, error) {
	var zero [32]byte
	for _, b := range boxes {
		if !bytes.Equal(b.To, self[:]) {
			continue
		}
		plain, ok := open(b.Ciphertext, b.Nonce, b.EphemeralPublicKey, priv)
		if !ok || len(plain) != 32 {
			return zero, fmt.Errorf("session: nonce box for %s did not open", self)
		}
		var n [32]byte
		copy(n[:], plain)
		return n, nil
	}
	return zero, fmt.Errorf("session: no nonce box for %s", self)
}

type acceptFile struct {
	SessionIDs []string `json:"session_ids"`
}

func loadAccepts(path string) (map[ID]struct{}, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[ID]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	var file acceptFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("session: local accept file: %w", err)
	}
	out := map[ID]struct{}{}
	for _, s := range file.SessionIDs {
		id, err := ParseID(s)
		if err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, nil
}

func (s *Service) saveLocked() error {
	if s.cfg.AcceptPath == "" {
		return nil
	}
	file := acceptFile{}
	for id := range s.onDisk {
		file.SessionIDs = append(file.SessionIDs, id.String())
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := os.WriteFile(s.cfg.AcceptPath, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}
