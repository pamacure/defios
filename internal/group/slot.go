package group

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

// Slot rejections (§1.2, §6.2).
var (
	ErrOneGroup     = errors.New("group: this Node already has a pending or valid Group")
	ErrUnknownGroup = errors.New("group: that is not this Node's Group")
	ErrAlreadyValid = errors.New("group: the Group is already valid")
)

// Slot is this Node's one Group (§1.2): empty, pending, or valid. It signs
// this Node's own formation messages, verifies the ones it receives, and
// persists the Record under groupsDir so a restarted serve resumes.
type Slot struct {
	mu   sync.Mutex
	dir  string
	self *identity.Node
	rec  *Record // nil when empty
}

// OpenSlot loads whatever Record groupsDir already holds.
func OpenSlot(groupsDir string, self *identity.Node) (*Slot, error) {
	rec, err := Load(groupsDir)
	if err != nil && !errors.Is(err, ErrNoGroup) {
		return nil, err
	}
	return &Slot{dir: groupsDir, self: self, rec: rec}, nil
}

// Current is the held Record, if any.
func (s *Slot) Current() (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec, s.rec != nil
}

// Propose signs a Create proposal over roster and holds it as pending;
// this Node's create is its Accept (§6.2 steps 1–2).
func (s *Slot) Propose(roster []*groupv1.Identity, now time.Time) (*groupv1.CreateProposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec != nil {
		return nil, ErrOneGroup
	}
	p, err := SignProposal(s.self, roster, now)
	if err != nil {
		return nil, err
	}
	prop, err := VerifyProposal(p)
	if err != nil {
		return nil, err
	}
	if err := s.holdProposal(prop); err != nil {
		return nil, err
	}
	return p, nil
}

// ReceiveProposal verifies a proposal that names this Node and holds it as
// pending (§6.2 step 3). Re-delivery of the held proposal is a no-op; any
// other proposal while one is held is ErrOneGroup.
func (s *Slot) ReceiveProposal(p *groupv1.CreateProposal) (Proposal, error) {
	prop, err := VerifyProposal(p)
	if err != nil {
		return Proposal{}, err
	}
	if _, ok := prop.Roster.Lookup(s.self.NodeID); !ok {
		return Proposal{}, fmt.Errorf("%w: this Node is not on the roster", ErrBadProposal)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec != nil {
		if s.rec.GroupID() == prop.GroupID {
			return prop, nil
		}
		return Proposal{}, ErrOneGroup
	}
	if err := s.holdProposal(prop); err != nil {
		return Proposal{}, err
	}
	return prop, nil
}

// Accept signs this Node's Accept of the held Group (§6.2 step 4). It does
// not hold it: the caller delivers it to every other Roster Node and then
// passes it to ReceiveAccept, so this Node turns Valid only once the
// others can too.
func (s *Slot) Accept(gid ID) (*groupv1.CreateAccept, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(gid); err != nil {
		return nil, err
	}
	if a, ok := s.rec.accepts[s.self.NodeID]; ok {
		return a, nil
	}
	return SignAccept(s.self, gid)
}

// ReceiveAccept verifies and holds an Accept (another Node's, or this
// Node's own once delivered), returning who accepted. The Record turns
// Valid once every Roster Node has.
func (s *Slot) ReceiveAccept(a *groupv1.CreateAccept) (identity.ID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gid, ok := idFromBytes(a.GroupId)
	if !ok {
		return identity.ID{}, fmt.Errorf("%w: group_id is not 32 bytes", ErrBadAccept)
	}
	if err := s.check(gid); err != nil {
		return identity.ID{}, err
	}
	who, err := VerifyAccept(a, s.rec.proposal)
	if err != nil {
		return identity.ID{}, err
	}
	if who == s.rec.Initiator() || s.rec.holds(who) {
		return who, nil // the initiator's create already counts; re-delivery is a no-op
	}
	return who, s.hold(who, a)
}

// Decline signs this Node's Decline and cancels the held Group, freeing the
// Slot; the caller delivers it to every other Roster Node.
func (s *Slot) Decline(gid ID, reason string) (*groupv1.CreateDecline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(gid); err != nil {
		return nil, err
	}
	if s.rec.Status() == Valid {
		return nil, ErrAlreadyValid
	}
	d, err := SignDecline(s.self, gid, reason)
	if err != nil {
		return nil, err
	}
	return d, s.drop()
}

// ReceiveDecline verifies another Node's Decline and cancels the held
// Group, returning who declined. A valid Group is not cancelled.
func (s *Slot) ReceiveDecline(d *groupv1.CreateDecline) (identity.ID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gid, ok := idFromBytes(d.GroupId)
	if !ok {
		return identity.ID{}, fmt.Errorf("%w: group_id is not 32 bytes", ErrBadDecline)
	}
	if err := s.check(gid); err != nil {
		return identity.ID{}, err
	}
	who, _, err := VerifyDecline(d, s.rec.proposal)
	if err != nil {
		return identity.ID{}, err
	}
	if s.rec.Status() == Valid {
		return who, ErrAlreadyValid
	}
	return who, s.drop()
}

// Cancel drops a pending Group this Node proposed but could not deliver.
func (s *Slot) Cancel(gid ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(gid); err != nil {
		return err
	}
	if s.rec.Status() == Valid {
		return ErrAlreadyValid
	}
	return s.drop()
}

// check requires a held Group with this ID. Caller holds mu.
func (s *Slot) check(gid ID) error {
	if s.rec == nil {
		return ErrNoGroup
	}
	if s.rec.GroupID() != gid {
		return fmt.Errorf("%w: %s", ErrUnknownGroup, gid)
	}
	return nil
}

// holdProposal persists prop as this Node's pending Group. Caller holds mu.
func (s *Slot) holdProposal(prop Proposal) error {
	rec := &Record{proposal: prop, accepts: map[identity.ID]*groupv1.CreateAccept{}}
	if err := rec.save(s.dir); err != nil {
		return err
	}
	s.rec = rec
	return nil
}

// hold persists one more Accept. Caller holds mu.
func (s *Slot) hold(who identity.ID, a *groupv1.CreateAccept) error {
	s.rec.accepts[who] = a
	if err := s.rec.save(s.dir); err != nil {
		delete(s.rec.accepts, who)
		return err
	}
	return nil
}

// drop removes the held Group from disk and memory. Caller holds mu.
func (s *Slot) drop() error {
	if err := s.rec.remove(s.dir); err != nil {
		return err
	}
	s.rec = nil
	return nil
}
