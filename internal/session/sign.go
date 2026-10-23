package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	coslv1 "defios.com/proto/cosl/v1"
)

// StartSign appends the session.start that signs a sign.request. The t
// signers are this Node plus the first t-1 other Roster Nodes, and appending
// is the proposer's local accept: proposing is a single step
// (docs/adr/0010-signing-session-semantics.md).
func (s *Service) StartSign(ctx context.Context, requestID [32]byte) (Summary, error) {
	if s.cfg.SignInfo == nil {
		return Summary{}, ErrNoEngine
	}
	info, err := s.cfg.SignInfo(requestID)
	if err != nil {
		return Summary{}, err
	}
	nodes := s.cfg.Roster.Nodes()
	self := s.cfg.Node.NodeID
	signers := []group.RosterNode{}
	for _, n := range nodes {
		if n.NodeID == self {
			signers = append(signers, n)
		}
	}
	if len(signers) == 0 {
		return Summary{}, ErrNotParticipant
	}
	for _, n := range nodes {
		if len(signers) < info.T && n.NodeID != self {
			signers = append(signers, n)
		}
	}
	if len(signers) < info.T {
		return Summary{}, fmt.Errorf("session: the Roster has %d Nodes, the Key needs %d signers", len(nodes), info.T)
	}
	slices.SortFunc(signers, func(a, b group.RosterNode) int { return bytes.Compare(a.NodeID[:], b.NodeID[:]) })

	var id ID
	var nonce [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Summary{}, fmt.Errorf("session: %w", err)
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		return Summary{}, fmt.Errorf("session: %w", err)
	}
	boxes := make([]*coslv1.SealedBox, 0, len(signers))
	parts := make([][]byte, 0, len(signers))
	for _, n := range signers {
		eph, nn, ct, err := seal(nonce[:], n.EncryptionPublicKey)
		if err != nil {
			return Summary{}, err
		}
		boxes = append(boxes, &coslv1.SealedBox{To: append([]byte(nil), n.NodeID[:]...), EphemeralPublicKey: eph, Nonce: nn, Ciphertext: ct})
		parts = append(parts, append([]byte(nil), n.NodeID[:]...))
	}
	payload := &coslv1.SessionStart{
		SessionId:     append([]byte(nil), id[:]...),
		Curve:         info.Curve,
		T:             uint32(info.T),
		N:             uint32(len(nodes)),
		Participants:  parts,
		NonceBoxes:    boxes,
		SignRequestId: append([]byte(nil), requestID[:]...),
	}
	if err := s.remember(id); err != nil {
		return Summary{}, err
	}
	if err := s.signAndAppend(ctx, cosl.TypeSessionStart, payload); err != nil {
		s.forget(id)
		return Summary{}, err
	}
	return Summary{SessionID: id.String(), Status: Open, Curve: info.Curve, T: uint32(info.T), N: uint32(len(nodes))}, nil
}

// AcceptSign is this Node's local accept of the open signing Session for a
// sign.request. Nothing is written to the COSL.
func (s *Service) AcceptSign(ctx context.Context, requestID [32]byte) error {
	id, err := s.openSignSession(requestID)
	if err != nil {
		return err
	}
	return s.Accept(ctx, id)
}

// DeclineSign aborts the open signing Session for a sign.request.
func (s *Service) DeclineSign(ctx context.Context, requestID [32]byte) error {
	id, err := s.openSignSession(requestID)
	if err != nil {
		return err
	}
	return s.Abort(ctx, id, reasonDeclined)
}

func (s *Service) openSignSession(requestID [32]byte) (ID, error) {
	recs, err := s.records()
	if err != nil {
		return ID{}, err
	}
	for _, r := range recs {
		if r.isSign && r.signReq == requestID && r.status == Open {
			return r.id, nil
		}
	}
	return ID{}, ErrUnknownSession
}

// validateAll applies Key policy to every Session. It reports false when a
// sign.request could not be read this time, so the caller waits for the next
// poll instead of aborting on a transient failure.
func (s *Service) validateAll(recs []*rec) bool {
	for _, r := range recs {
		if !r.isSign {
			r.validate(s.cfg.Roster)
			continue
		}
		info, err := s.signInfo(r)
		switch {
		case err == nil:
			r.validateSign(s.cfg.Roster, info)
		case errors.Is(err, ErrBadSignRequest) || errors.Is(err, ErrUnsupported):
			r.invalid = err.Error()
		default:
			s.cfg.Logf("session: reading sign request for %s: %v", r.id, err)
			return false
		}
	}
	return true
}

// signInfo resolves a signing Session's request, once. A lookup that fails
// is retried on the next poll.
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

// validateSign is Key policy for a signing start: it signs with the Key's
// own t signers, all on the Roster, one of them the proposer. An invalid
// start still occupies the one-open slot until it is aborted.
func (r *rec) validateSign(roster group.Roster, info SignInfo) {
	ids := roster.NodeIDs()
	switch {
	case info.Curve != "ecdsa":
		r.invalid = fmt.Sprintf("signing with a %s Key is not supported", info.Curve)
	case r.curve != info.Curve || int(r.t) != info.T || int(r.n) != len(ids):
		r.invalid = "start does not match the Key"
	case len(r.participants) != info.T:
		r.invalid = fmt.Sprintf("%d signers, the Key needs %d", len(r.participants), info.T)
	case !slices.Contains(r.participants, r.sender):
		r.invalid = "the proposer is not a signer"
	default:
		for i, id := range r.participants {
			if !slices.Contains(ids, id) || (i > 0 && bytes.Compare(r.participants[i-1][:], id[:]) >= 0) {
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
// signature verifies against the Key and its Digest.
func (s *Service) verifySigned(r *rec, c seen) bool {
	info, err := s.signInfo(r)
	return err == nil && info.verifies(c.signatures)
}

// verifies reports whether sigs are exactly one valid signature per Digest.
func (i SignInfo) verifies(sigs [][]byte) bool {
	if len(sigs) != len(i.Digests) {
		return false
	}
	for n, sig := range sigs {
		if !VerifyECDSA(i.PublicKey, i.Digests[n], sig) {
			return false
		}
	}
	return true
}

// finishSign appends session.complete once the engine's signatures verify. A
// Node that cannot verify its own aborts.
func (s *Service) finishSign(ctx context.Context, r *rec, p *party, res Result) {
	info, err := s.signInfo(r)
	if err != nil {
		s.cfg.Logf("session: reading sign request for %s: %v", r.id, err)
		return // retried on the next poll
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
		s.cfg.Logf("session: complete %s: %v", r.id, err)
		return
	}
	p.minted = true
}
