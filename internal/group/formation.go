package group

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

// Formation message rejections (§6.2 step 3, §11.3). Each wraps the reason.
var (
	ErrBadProposal = errors.New("group: proposal does not verify")
	ErrBadAccept   = errors.New("group: accept does not verify")
	ErrBadDecline  = errors.New("group: decline does not verify")
)

// Proposal is a CreateProposal that has verified: every Identity on it
// checks, the Group ID is derived from the Roster, and the initiator's
// signature is under the auth key the Roster pins for it.
type Proposal struct {
	GroupID   ID
	Roster    Roster
	Initiator identity.ID
	CreatedAt int64                   // unix seconds
	raw       *groupv1.CreateProposal // the signed wire form, kept for the Record
}

// SignProposal builds the initiator's Create proposal over the given signed
// Identities (§6.2 step 2). The initiator must be one of them.
func SignProposal(initiator *identity.Node, roster []*groupv1.Identity, now time.Time) (*groupv1.CreateProposal, error) {
	r, err := rosterFromIdentities(roster)
	if err != nil {
		return nil, err
	}
	if _, ok := r.Lookup(initiator.NodeID); !ok {
		return nil, fmt.Errorf("%w: initiator %s is not on the roster", ErrBadProposal, initiator.NodeID)
	}
	gid := r.GroupID()
	p := &groupv1.CreateProposal{
		GroupId:   gid[:],
		Roster:    sortedIdentities(roster),
		Initiator: initiator.NodeID[:],
		CreatedAt: now.Unix(),
	}
	p.Sig, err = sign(initiator, p)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// VerifyProposal checks a received proposal: Identities, Group ID hash,
// initiator on the Roster, and the initiator's signature.
func VerifyProposal(p *groupv1.CreateProposal) (Proposal, error) {
	r, err := rosterFromIdentities(p.Roster)
	if err != nil {
		return Proposal{}, err
	}
	for i := 1; i < len(p.Roster); i++ {
		if bytes.Compare(p.Roster[i-1].NodeId, p.Roster[i].NodeId) >= 0 {
			return Proposal{}, fmt.Errorf("%w: roster is not sorted by node_id", ErrBadProposal)
		}
	}
	gid, ok := idFromBytes(p.GroupId)
	if !ok || gid != r.GroupID() {
		return Proposal{}, fmt.Errorf("%w: group_id is not derived from the roster", ErrBadProposal)
	}
	initiator, ok := nodeIDFromBytes(p.Initiator)
	if !ok {
		return Proposal{}, fmt.Errorf("%w: initiator is not 32 bytes", ErrBadProposal)
	}
	signer, ok := r.Lookup(initiator)
	if !ok {
		return Proposal{}, fmt.Errorf("%w: initiator %s is not on the roster", ErrBadProposal, initiator)
	}
	if err := verify(signer.AuthPublicKey, p, p.Sig); err != nil {
		return Proposal{}, fmt.Errorf("%w: %w", ErrBadProposal, err)
	}
	return Proposal{GroupID: gid, Roster: r, Initiator: initiator, CreatedAt: p.CreatedAt, raw: p}, nil
}

// SignAccept is this Node's Accept of the Group (§6.2 step 4).
func SignAccept(n *identity.Node, gid ID) (*groupv1.CreateAccept, error) {
	a := &groupv1.CreateAccept{GroupId: gid[:], NodeId: n.NodeID[:]}
	var err error
	a.Sig, err = sign(n, a)
	return a, err
}

// VerifyAccept checks an Accept against the Proposal it answers and
// returns the accepting Node.
func VerifyAccept(a *groupv1.CreateAccept, p Proposal) (identity.ID, error) {
	who, err := verifyResponse(a.GroupId, a.NodeId, a, a.Sig, p)
	if err != nil {
		return identity.ID{}, fmt.Errorf("%w: %w", ErrBadAccept, err)
	}
	return who, nil
}

// SignDecline is this Node's Decline of the Group (§6.2 step 4).
func SignDecline(n *identity.Node, gid ID, reason string) (*groupv1.CreateDecline, error) {
	d := &groupv1.CreateDecline{GroupId: gid[:], NodeId: n.NodeID[:], Reason: reason}
	var err error
	d.Sig, err = sign(n, d)
	return d, err
}

// VerifyDecline checks a Decline against the Proposal it answers and
// returns the declining Node and its reason.
func VerifyDecline(d *groupv1.CreateDecline, p Proposal) (identity.ID, string, error) {
	who, err := verifyResponse(d.GroupId, d.NodeId, d, d.Sig, p)
	if err != nil {
		return identity.ID{}, "", fmt.Errorf("%w: %w", ErrBadDecline, err)
	}
	return who, d.Reason, nil
}

// verifyResponse is the shared Accept/Decline check: same Group, sender on
// the Roster, signature under the Roster's pinned auth key.
func verifyResponse(groupID, nodeID []byte, msg proto.Message, sig []byte, p Proposal) (identity.ID, error) {
	gid, ok := idFromBytes(groupID)
	if !ok || gid != p.GroupID {
		return identity.ID{}, errors.New("group_id is not this proposal's")
	}
	who, ok := nodeIDFromBytes(nodeID)
	if !ok {
		return identity.ID{}, errors.New("node_id is not 32 bytes")
	}
	signer, ok := p.Roster.Lookup(who)
	if !ok {
		return identity.ID{}, fmt.Errorf("node %s is not on the roster", who)
	}
	if err := verify(signer.AuthPublicKey, msg, sig); err != nil {
		return identity.ID{}, err
	}
	return who, nil
}

// rosterFromIdentities verifies each Identity and builds the Roster,
// rejecting duplicates.
func rosterFromIdentities(ids []*groupv1.Identity) (Roster, error) {
	if len(ids) < 2 {
		return Roster{}, fmt.Errorf("%w: a roster needs at least two Nodes", ErrBadProposal)
	}
	nodes := make([]RosterNode, 0, len(ids))
	seen := map[identity.ID]bool{}
	for _, id := range ids {
		n, err := VerifyIdentity(id)
		if err != nil {
			return Roster{}, fmt.Errorf("%w: %w", ErrBadProposal, err)
		}
		if seen[n.NodeID] {
			return Roster{}, fmt.Errorf("%w: node %s appears twice", ErrBadProposal, n.NodeID)
		}
		seen[n.NodeID] = true
		nodes = append(nodes, n)
	}
	return NewRoster(nodes...), nil
}

// sortedIdentities clones ids (the proposal owns its Roster) sorted by
// node_id as raw bytes.
func sortedIdentities(ids []*groupv1.Identity) []*groupv1.Identity {
	sorted := make([]*groupv1.Identity, len(ids))
	for i, id := range ids {
		sorted[i] = proto.Clone(id).(*groupv1.Identity)
	}
	slices.SortFunc(sorted, func(a, b *groupv1.Identity) int { return bytes.Compare(a.NodeId, b.NodeId) })
	return sorted
}

// unsigned returns msg's deterministic bytes with sig cleared: every
// formation message is signed over everything but its sig field (§2.6).
func unsigned(msg proto.Message) ([]byte, error) {
	clone := proto.Clone(msg)
	switch m := clone.(type) {
	case *groupv1.Identity:
		m.Sig = nil
	case *groupv1.CreateProposal:
		m.Sig = nil
	case *groupv1.CreateAccept:
		m.Sig = nil
	case *groupv1.CreateDecline:
		m.Sig = nil
	default:
		return nil, fmt.Errorf("group: cannot sign %T", msg)
	}
	return canonical.Marshal(clone)
}

// sign produces n's auth signature over msg.
func sign(n *identity.Node, msg proto.Message) ([]byte, error) {
	b, err := unsigned(msg)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(ed25519.NewKeyFromSeed(n.AuthSeed), b), nil
}

// verify checks sig over msg under pub.
func verify(pub ed25519.PublicKey, msg proto.Message, sig []byte) error {
	if len(sig) != ed25519.SignatureSize {
		return errors.New("malformed signature")
	}
	b, err := unsigned(msg)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, b, sig) {
		return errors.New("bad signature")
	}
	return nil
}

// idFromBytes reads a wire group_id.
func idFromBytes(b []byte) (ID, bool) {
	var id ID
	if len(b) != len(id) {
		return id, false
	}
	copy(id[:], b)
	return id, true
}

// nodeIDFromBytes reads a wire node_id.
func nodeIDFromBytes(b []byte) (identity.ID, bool) {
	var id identity.ID
	if len(b) != len(id) {
		return id, false
	}
	copy(id[:], b)
	return id, true
}
