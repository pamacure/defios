package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// AcceptSign is this Node's accept of a sign.request. It appends a sign.accept
// and opens the local gate its engine waits on: the engine runs only for a
// request this Node accepted. The Node is a signer if it is among the first t
// to accept; if t others got there first the accept is ignored and nothing is
// appended. The proposer's accept is implied by proposing (AcceptProposed).
func (s *Service) AcceptSign(ctx context.Context, requestID [32]byte) error {
	if s.cfg.SignInfo == nil {
		return ErrNoEngine
	}
	info, err := s.cfg.SignInfo(requestID)
	if err != nil {
		return err
	}
	if r, err := s.signSession(requestID); err != nil {
		return err
	} else if r != nil {
		return s.acceptStarted(r, requestID)
	}
	if s.now().After(info.Deadline) {
		return ErrClosedSession
	}
	if info.hasAccepted(s.cfg.Node.NodeID) {
		return s.remember(ID(requestID))
	}
	if len(info.signers(0)) >= info.T {
		return ErrSignerSetFull
	}
	return s.appendAccept(ctx, requestID, info)
}

// acceptStarted is the local accept of a request whose Session already
// started: only one of its participants can open the gate, and only while
// open. The participants are the first t accepts, so any other Node was late.
func (s *Service) acceptStarted(r *rec, requestID [32]byte) error {
	if !slices.Contains(r.participants, s.cfg.Node.NodeID) {
		return ErrSignerSetFull
	}
	if r.status != Open {
		return ErrClosedSession
	}
	return s.remember(ID(requestID))
}

// appendAccept opens the local gate and appends this Node's sign.accept,
// closing the gate again if the append fails.
func (s *Service) appendAccept(ctx context.Context, requestID [32]byte, info SignInfo) error {
	if err := s.remember(ID(requestID)); err != nil {
		return err
	}
	payload := &coslv1.SignAccept{SignRequestId: append([]byte(nil), requestID[:]...)}
	signed, err := s.author().Sign(cosl.TypeSignAccept, info.Conversation, payload)
	if err == nil {
		err = s.cfg.Append(ctx, signed)
	}
	if err != nil {
		s.forget(ID(requestID))
	}
	return err
}

// AcceptProposed is the proposer's local accept, made by proposing: its
// signature on the sign.request is the accept the log sees, so nothing is
// appended here.
func (s *Service) AcceptProposed(requestID [32]byte) error {
	return s.remember(ID(requestID))
}

// DeclineResult says what `sign decline` did, because declining before a
// Session exists changes nothing on the log.
type DeclineResult string

const (
	// DeclineAborted: the open signing Session was aborted.
	DeclineAborted DeclineResult = "aborted"
	// DeclineNoSession: no Session has started and this Node has not accepted,
	// so there was nothing to decline.
	DeclineNoSession DeclineResult = "no-session"
	// DeclineAcceptStands: no Session has started, but this Node already
	// accepted; its accept is on the log and it can still be named a signer.
	DeclineAcceptStands DeclineResult = "accept-stands"
	// DeclineClosed: the request's Session had already finished or aborted.
	DeclineClosed DeclineResult = "closed"
)

// DeclineSign aborts the open signing Session for a sign.request. Before one
// exists there is nothing to abort: declining is simply not accepting, and an
// accept this Node already made stays, which the result says.
func (s *Service) DeclineSign(ctx context.Context, requestID [32]byte) (DeclineResult, error) {
	r, err := s.signSession(requestID)
	if err != nil {
		return "", err
	}
	switch {
	case r != nil && r.status != Open:
		return DeclineClosed, nil
	case r != nil:
		return DeclineAborted, s.Abort(ctx, r.id, reasonDeclined)
	case s.acceptedLocally(ID(requestID)):
		return DeclineAcceptStands, nil
	default:
		return DeclineNoSession, nil
	}
}

// acceptedLocally is whether this Node has accepted id (on disk or live).
func (s *Service) acceptedLocally(id ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, p := s.pending[id]
	_, d := s.onDisk[id]
	return p || d
}

// signSession is the first Session started for a sign.request, or nil.
func (s *Service) signSession(requestID [32]byte) (*rec, error) {
	recs, err := s.records()
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if r.isSign && r.signReq == requestID {
			return r, nil
		}
	}
	return nil, nil
}

// startAccepted appends the session.start for every request this Node has
// accepted that now has t signers, this Node among them, and no Session yet.
// Any signer may do it; if two race, the later start is a duplicate and every
// Node aborts it. The Session slot must be free, or the start would only be
// aborted.
func (s *Service) startAccepted(ctx context.Context, recs []*rec) {
	if s.cfg.SignInfo == nil || s.cfg.NewSigner == nil {
		return
	}
	started := map[ID]struct{}{}
	sessions := map[ID]struct{}{}
	busy := false
	for _, r := range recs {
		sessions[r.id] = struct{}{}
		if r.isSign {
			started[ID(r.signReq)] = struct{}{}
		}
		busy = busy || r.status == Open
	}
	for _, id := range s.acceptedRequests(sessions) {
		if _, ok := started[id]; ok {
			continue
		}
		if _, ok := s.starting[id]; ok {
			continue
		}
		s.startRequest(ctx, id, busy)
	}
}

// startRequest starts the Session for an accepted request if t signers exist,
// this Node is one, and the slot is free.
func (s *Service) startRequest(ctx context.Context, id ID, busy bool) {
	info, err := s.cfg.SignInfo(id)
	if err != nil {
		return // not readable yet, or no longer a sign.request
	}
	if s.now().After(info.Deadline) {
		s.forget(id)
		return
	}
	// The accept lapses at the deadline: wake then to forget it.
	s.until(info.Deadline.Add(time.Nanosecond))
	s.promote(id) // accepted before a restart, and no Session has been lost
	signers := info.signers(0)
	if busy || len(signers) < info.T || !slices.Contains(signers, s.cfg.Node.NodeID) || !SupportsSigning(info.Curve) {
		return
	}
	s.starting[id] = struct{}{}
	if err := s.appendSignStart(ctx, id, info, signers); err != nil {
		delete(s.starting, id)
		s.note(fmt.Errorf("starting %x: %w", id[:4], err))
	}
}

// appendSignStart mints the Session's nonce and seals it to the signers,
// which are in sorted order (SignInfo.signers).
func (s *Service) appendSignStart(ctx context.Context, requestID ID, info SignInfo, signers []identity.ID) error {
	nodes := s.cfg.Roster.Nodes()
	var id ID
	var nonce [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	boxes := make([]*coslv1.SealedBox, 0, len(signers))
	parts := make([][]byte, 0, len(signers))
	for _, sg := range signers {
		i := slices.IndexFunc(nodes, func(n group.RosterNode) bool { return n.NodeID == sg })
		if i < 0 {
			return fmt.Errorf("session: signer %s is not on the Roster", sg)
		}
		eph, nn, ct, err := seal(nonce[:], nodes[i].EncryptionPublicKey)
		if err != nil {
			return err
		}
		boxes = append(boxes, &coslv1.SealedBox{To: append([]byte(nil), sg[:]...), EphemeralPublicKey: eph, Nonce: nn, Ciphertext: ct})
		parts = append(parts, append([]byte(nil), sg[:]...))
	}
	return s.signAndAppend(ctx, cosl.TypeSessionStart, &coslv1.SessionStart{
		SessionId:     append([]byte(nil), id[:]...),
		Curve:         info.Curve,
		T:             uint32(info.T),
		N:             uint32(len(nodes)),
		Participants:  parts,
		NonceBoxes:    boxes,
		SignRequestId: append([]byte(nil), requestID[:]...),
	})
}

// acceptedRequests are the ids this Node has locally accepted that are not
// in skip (the Sessions the log shows): sign.requests that may not have a
// Session yet.
func (s *Service) acceptedRequests(skip map[ID]struct{}) []ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []ID
	add := func(id ID) {
		if _, ok := skip[id]; !ok {
			ids = append(ids, id)
		}
	}
	for id := range s.onDisk {
		add(id)
	}
	for id := range s.pending {
		if _, ok := s.onDisk[id]; !ok {
			add(id)
		}
	}
	return ids
}

// promote makes a durable accept live in this process, for a request with no
// Session: nothing was running, so nothing is lost by the restart.
func (s *Service) promote(id ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.onDisk[id]; ok {
		s.pending[id] = struct{}{}
	}
}

// validateAll applies Key policy to every Session. It reports false when a
// sign.request could not be read this time, so the caller waits for the next
// wake instead of aborting on a transient failure.
func (s *Service) validateAll(recs []*rec) bool {
	started := map[[32]byte]struct{}{}
	for _, r := range recs {
		if !r.isSign {
			r.validate(s.cfg.Roster)
			continue
		}
		if _, ok := started[r.signReq]; ok {
			r.dup, r.invalid = true, "the request already has a session"
			continue
		}
		started[r.signReq] = struct{}{}
		info, err := s.signInfo(r)
		switch {
		case err == nil:
			r.validateSign(s.cfg.Roster, info)
		case errors.Is(err, ErrBadSignRequest) || errors.Is(err, ErrUnsupported):
			r.invalid = err.Error()
		default:
			s.note(fmt.Errorf("reading sign request for %s: %w", r.id, err))
			return false
		}
	}
	return true
}

// signInfo resolves a signing Session's request, once. A lookup that fails
// is retried on the next wake.
func (s *Service) signInfo(r *rec) (SignInfo, error) {
	if !r.isSign || s.cfg.SignInfo == nil {
		return SignInfo{}, ErrNoEngine
	}
	s.mu.Lock()
	info, ok := s.signs[r.id]
	s.mu.Unlock()
	if ok {
		return info, nil
	}
	info, err := s.cfg.SignInfo(r.signReq)
	if err != nil {
		return SignInfo{}, err
	}
	s.mu.Lock()
	s.signs[r.id] = info
	s.mu.Unlock()
	return info, nil
}

// validateSign is Key policy for a signing start: its participants are
// exactly the first t Nodes to have accepted the request before the start, in
// sorted order, and the sender is one of them. An invalid start still occupies
// the one-open slot until it is aborted.
func (r *rec) validateSign(roster group.Roster, info SignInfo) {
	ids := roster.NodeIDs()
	signers := info.signers(r.startSeq)
	switch {
	case !SupportsSigning(info.Curve):
		r.invalid = fmt.Sprintf("signing with a %s Key is not supported", info.Curve)
	case r.curve != info.Curve || int(r.t) != info.T || int(r.n) != len(ids):
		r.invalid = "start does not match the Key"
	case len(signers) < info.T:
		r.invalid = fmt.Sprintf("%d of the Key's %d signers have accepted", len(signers), info.T)
	case !slices.Equal(r.participants, signers):
		r.invalid = "participants are not the first t Nodes to accept"
	case !slices.Contains(r.participants, r.sender):
		r.invalid = "the sender is not a signer"
	default:
		for _, id := range r.participants {
			if !slices.Contains(ids, id) {
				r.invalid = "signers are not distinct Roster Nodes in order"
				return
			}
		}
		if !boxesMatch(r.boxes, r.participants) {
			r.invalid = "nonce boxes do not match the signers"
			return
		}
		r.valid = true
	}
}

// verifySigned is the parse hook: a session.complete only counts when every
// signature verifies against the Key and its Digest. The answer cannot change
// once the row is committed, so it is worked out once per row, keyed by the
// row's envelope hash (ADR 0014). A sign.request that cannot be read yet is
// not an answer, so it is asked again on the next wake.
func (s *Service) verifySigned(r *rec, c seen) bool {
	s.mu.Lock()
	ok, known := s.verified[c.hash]
	s.mu.Unlock()
	if known {
		return ok
	}
	info, err := s.signInfo(r)
	if err != nil {
		return false
	}
	ok = info.verifies(c.signatures)
	s.mu.Lock()
	s.verified[c.hash] = ok
	s.mu.Unlock()
	return ok
}

// verifies reports whether sigs are exactly one valid signature per Digest.
func (i SignInfo) verifies(sigs [][]byte) bool {
	if len(sigs) != len(i.Digests) {
		return false
	}
	for n, sig := range sigs {
		if !i.verifyOne(i.Digests[n], sig) {
			return false
		}
	}
	return true
}

// verifyOne checks one signature of msg against the Key by its curve.
func (i SignInfo) verifyOne(msg, sig []byte) bool {
	switch i.Curve {
	case "ecdsa":
		return len(msg) == 32 && VerifyECDSA(i.PublicKey, [32]byte(msg), sig)
	case "eddsa":
		return VerifyEd25519(i.PublicKey, msg, sig)
	}
	return false
}

// finishSign appends session.complete once the engine's signatures verify. A
// Node that cannot verify its own aborts.
func (s *Service) finishSign(ctx context.Context, r *rec, p *party, res Result) {
	info, err := s.signInfo(r)
	if err != nil {
		s.note(fmt.Errorf("reading sign request for %s: %w", r.id, err))
		return // retried on the next wake
	}
	if !info.verifies(res.Signatures) {
		s.giveUp(ctx, r.id, "session: a signature does not verify against the Key")
		return
	}
	err = s.signAndAppend(ctx, cosl.TypeSessionComplete, &coslv1.SessionComplete{
		SessionId:  append([]byte(nil), r.id[:]...),
		Curve:      r.curve,
		PublicKey:  append([]byte(nil), info.PublicKey...),
		Signatures: cloneAll(res.Signatures),
	})
	if err != nil {
		s.note(fmt.Errorf("complete %s: %w", r.id, err))
		return
	}
	p.minted = true
}
