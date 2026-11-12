package shell

import (
	"errors"
	"fmt"
	"strings"

	"defios.com/internal/group"
	"defios.com/internal/node"
	"defios.com/internal/vault"
)

// progressLines is how many of Create's progress lines State keeps.
const progressLines = 8

// CheckDisplayName is why name cannot be this device's name, or nil, as
// Create would refuse it; for checking as it is typed.
func CheckDisplayName(name string) *Problem {
	return problemOrNil(node.ValidateDisplayName(name))
}

// Create mints this device's Node with a display name and passphrase,
// reporting progress as it goes, then shows onboarding step 2 with the
// passphrase held.
func (s *Shell) Create(name, passphrase string) State {
	s.op.Lock()
	defer s.op.Unlock()
	if s.State().View != ViewNoNode {
		return s.State()
	}
	if p := CheckDisplayName(name); p != nil {
		s.fail(p)
		return s.State()
	}
	if passphrase == "" {
		s.fail(problemOf(vault.ErrEmptyPassphrase))
		return s.State()
	}
	s.set(func(st *State) { st.View, st.Progress, st.Problem = ViewCreating, nil, nil })
	err := s.d.Create(node.Creation{
		DisplayName: name,
		Passphrase:  passphrase,
		Progress: func(line string) {
			s.set(func(st *State) {
				st.Progress = append(st.Progress[max(0, len(st.Progress)-progressLines+1):len(st.Progress):len(st.Progress)], line)
			})
		},
	})
	s.set(func(st *State) { st.Progress = nil })
	if err != nil {
		s.set(func(st *State) { st.View, st.Problem = ViewNoNode, problemOf(err) })
		return s.State()
	}
	s.openHolding(passphrase)
	return s.State()
}

// openHolding opens the Node just made and holds passphrase if it has no
// Group yet, so step 2 needs none typed. s.op is held.
func (s *Shell) openHolding(passphrase string) {
	s.open()
	if s.State().View != ViewNoGroup {
		return
	}
	s.mu.Lock()
	s.pass = passphrase
	s.mu.Unlock()
	s.set(func(*State) {})
}

// CheckBackup shows why file cannot be restored, as far as that needs no
// passphrase to find, before one is asked for; Problem nil when none.
func (s *Shell) CheckBackup(file string) State {
	s.op.Lock()
	defer s.op.Unlock()
	if s.State().View != ViewNoNode {
		return s.State()
	}
	s.fail(backupProblem(s.d.CheckRestore(file)))
	return s.State()
}

// Restore brings back the Node the Backup file holds, sealed under
// passphrase. It then opens on the lock screen, or on step 2 with the
// passphrase held when it has no Group.
func (s *Shell) Restore(file, passphrase string) State {
	s.op.Lock()
	defer s.op.Unlock()
	if s.State().View != ViewNoNode {
		return s.State()
	}
	s.set(func(st *State) { st.View, st.Problem = ViewRestoring, nil })
	r, err := s.d.Restore(s.ctx, file, passphrase)
	if err != nil {
		s.set(func(st *State) { st.View, st.Problem = ViewNoNode, backupProblem(err) })
		return s.State()
	}
	note := &Restored{}
	if r.PluginsErr != nil {
		note.PluginsProblem = r.PluginsErr.Error()
	}
	s.set(func(st *State) { st.Restored = note })
	s.openHolding(passphrase)
	return s.State()
}

// AddGroup is onboarding step 2: it makes a Group under alias on the
// tailnet headscaleURL controls, keeps authKey as the Secret its config
// names, and serves it for step 3. passphrase is needed only when the app
// holds none. Serving it stops the Group served now, if any.
func (s *Shell) AddGroup(alias, headscaleURL, authKey, passphrase string) State {
	s.op.Lock()
	defer s.op.Unlock()
	n := s.node()
	if v := s.State().View; n == nil || (v != ViewNoGroup && v != ViewServed) {
		return s.State()
	}
	s.mu.Lock()
	pass, from := s.pass, s.st.Served
	s.mu.Unlock()
	if pass == "" {
		if passphrase == "" {
			s.fail(problemOf(node.ErrNoPassphrase))
			return s.State()
		}
		if err := n.CheckPassphrase(passphrase); err != nil {
			s.fail(problemOf(err))
			return s.State()
		}
		pass = passphrase
	}
	if authKey == "" {
		s.fail(&Problem{"no-auth-key", "Enter the tailnet auth key from your headscale server."})
		return s.State()
	}
	if _, err := n.NewGroup(node.GroupSpec{Alias: alias, HeadscaleURL: headscaleURL}); err != nil {
		s.fail(problemOf(err))
		return s.State()
	}
	held := func() (string, bool, error) { return pass, true, nil }
	if err := n.SetSecret(s.ctx, alias, node.TailnetAuthKeySecret, authKey, held); err != nil {
		// The Group is made, but cannot join its tailnet: say so.
		p := &Problem{"auth-key-not-saved", fmt.Sprintf("%s was added to your Groups, but its tailnet auth key was not saved: %s", alias, problemOf(err).Message)}
		if from == "" {
			s.forget() // the lock screen asks for it again
			s.route(p)
			return s.State()
		}
		groups, _ := s.groups(n)
		s.set(func(st *State) { st.Groups, st.Problem = groups, p })
		return s.State()
	}
	s.set(func(st *State) { st.View, st.Selected, st.Problem = ViewUnlocking, alias, nil })
	var err error
	if from != "" {
		err = n.Switch(alias)
	} else {
		err = n.Unlock(pass, alias)
	}
	if err != nil {
		s.forget()
		s.route(problemOf(err))
		return s.State()
	}
	s.mu.Lock()
	s.pass, s.back = pass, from
	s.mu.Unlock()
	s.apply(n.Status())
	return s.State()
}

// Forming is a served Group's formation, onboarding step 3.
type Forming struct {
	Group string
	// Back is the Group to serve again instead of forming this one now; ""
	// when there is none, and Lock is the way out.
	Back string
	// Discovered are the other devices on the Group's tailnet, as the last
	// Discover found them, and DiscoverProblem why it failed, if it did.
	Discovered      []NodeInfo
	DiscoverProblem *Problem
	// Proposal is the Group's proposal, pending; nil while unformed.
	Proposal *Proposal
	// Proposed are the devices the last Propose named, and NotReached
	// the Node IDs of those it did not reach: it was withdrawn, so
	// proposing again retries it.
	Proposed   []NodeInfo
	NotReached []string
	// Undelivered are the Node IDs the last Accept did not reach;
	// accepting again retries them.
	Undelivered []string
}

// Proposal is a Group proposal this device holds.
type Proposal struct {
	GroupID string
	// Initiator is the Node ID of the device that proposed it; Mine when
	// that is this one.
	Initiator string
	Mine      bool
	// Accepted: this device has accepted it.
	Accepted bool
	Roster   []ProposedNode
}

// ProposedNode is one Node a proposal's Roster names.
type ProposedNode struct {
	ID       string
	Name     string
	Self     bool
	Accepted bool
}

// formingOf is alias's formation while it has no valid Record, or while
// an Accept has yet to reach every device; nil once it is formed.
func (s *Shell) formingOf(alias string, groups []Group) *Forming {
	s.mu.Lock()
	prev, back := s.st.Forming, s.back
	s.mu.Unlock()
	f := Forming{Group: alias}
	if prev != nil && prev.Group == alias {
		f = *prev
	}
	f.Back = ""
	if back != alias && hasGroup(groups, back) {
		f.Back = back
	}
	switch statusOf(groups, alias) {
	case node.StatusUnformed:
		f.Proposal, f.Undelivered = nil, nil
	case group.Pending.String():
		f.Proposal, f.NotReached = s.proposal(alias), nil
	default:
		if len(f.Undelivered) == 0 {
			return nil
		}
		f.Proposal = s.proposal(alias)
	}
	return &f
}

func statusOf(groups []Group, alias string) string {
	for _, g := range groups {
		if g.Alias == alias {
			return g.Status
		}
	}
	return ""
}

// proposal is group's proposal as this device holds it; nil if none.
func (s *Shell) proposal(group string) *Proposal {
	d, err := s.node().InspectGroup(s.ctx, group)
	if err != nil {
		return nil
	}
	self := s.State().Node.ID
	p := &Proposal{GroupID: d.GroupID, Initiator: d.Initiator, Mine: d.Initiator == self}
	for _, m := range d.Roster {
		p.Roster = append(p.Roster, ProposedNode{ID: m.NodeID, Name: m.DisplayName, Self: m.NodeID == self, Accepted: m.Accepted})
		if m.NodeID == self {
			p.Accepted = m.Accepted
		}
	}
	return p
}

// forming is the Group step 3 forms now, and the node; "" when not on
// step 3.
func (s *Shell) forming() (Node, *Forming) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.View != ViewForming || s.st.Forming == nil {
		return nil, nil
	}
	return s.n, s.st.Forming
}

// changeForming changes the formation shown, if it is still group's, on
// a copy: a State already reported shares the one shown.
func (s *Shell) changeForming(group string, p *Problem, change func(*Forming)) {
	s.set(func(st *State) {
		if st.View != ViewForming || st.Forming == nil || st.Forming.Group != group {
			return
		}
		f := *st.Forming
		change(&f)
		st.Forming, st.Problem = &f, p
	})
}

// Discover looks again for the other devices on the Group's tailnet.
func (s *Shell) Discover() State {
	n, f := s.forming()
	if f == nil {
		return s.State()
	}
	found, err := n.Discover(s.ctx, f.Group)
	self := s.State().Node.ID
	nodes := []NodeInfo{}
	for _, d := range found {
		if d.NodeID != self {
			nodes = append(nodes, NodeInfo{ID: d.NodeID, Name: d.DisplayName})
		}
	}
	s.mu.Lock()
	p := s.st.Problem
	s.mu.Unlock()
	s.changeForming(f.Group, p, func(f *Forming) { f.Discovered, f.DiscoverProblem = nodes, problemOrNil(err) })
	return s.State()
}

// Propose proposes the Group to this device and the devices nodeIDs
// names. Those it did not reach are in Forming.NotReached; proposing them
// again retries.
func (s *Shell) Propose(nodeIDs []string) State {
	s.op.Lock()
	defer s.op.Unlock()
	n, f := s.forming()
	if f == nil || f.Proposal != nil {
		return s.State()
	}
	ids := append([]string{s.State().Node.ID}, nodeIDs...)
	proposed := make([]NodeInfo, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		d := NodeInfo{ID: id}
		for _, found := range append(f.Discovered, f.Proposed...) {
			if found.ID == id {
				d = found
				break
			}
		}
		proposed = append(proposed, d)
	}
	_, err := n.Propose(s.ctx, f.Group, ids)
	var missed *node.NotReachedError
	if errors.As(err, &missed) {
		s.changeForming(f.Group, problemOf(err), func(f *Forming) { f.Proposed, f.NotReached = proposed, missed.NodeIDs })
		return s.State()
	} else if err != nil {
		s.fail(problemOf(err))
		return s.State()
	}
	s.changeForming(f.Group, nil, func(f *Forming) { f.Proposed, f.NotReached = proposed, nil })
	s.apply(n.Status())
	return s.State()
}

// Accept accepts the Group's proposal, or accepts again for the devices
// the last Accept did not reach.
func (s *Shell) Accept() State {
	s.op.Lock()
	defer s.op.Unlock()
	n, f := s.forming()
	if f == nil || f.Proposal == nil {
		return s.State()
	}
	res, err := n.Accept(s.ctx, f.Group, f.Proposal.GroupID)
	if err != nil {
		s.fail(problemOf(err))
		return s.State()
	}
	s.changeForming(f.Group, nil, func(f *Forming) { f.Undelivered = res.Undelivered })
	s.apply(n.Status())
	return s.State()
}

// Decline declines the Group's proposal for reason, which the other
// devices log, freeing the Group for another.
func (s *Shell) Decline(reason string) State {
	s.op.Lock()
	defer s.op.Unlock()
	n, f := s.forming()
	if f == nil || f.Proposal == nil {
		return s.State()
	}
	res, err := n.Decline(s.ctx, f.Group, f.Proposal.GroupID, reason)
	if err != nil {
		s.fail(problemOf(err))
		return s.State()
	}
	var p *Problem
	if len(res.Undelivered) > 0 {
		p = &Problem{"undelivered", fmt.Sprintf("Declined, but %s did not hear of it yet.", f.Proposal.names(res.Undelivered))}
	}
	s.changeForming(f.Group, p, func(f *Forming) { f.Undelivered = nil })
	s.apply(n.Status())
	return s.State()
}

// names are the display names of the Nodes ids, as p names them.
func (p *Proposal) names(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		name := id
		for _, m := range p.Roster {
			if m.ID == id && m.Name != "" {
				name = m.Name
			}
		}
		out = append(out, name)
	}
	return strings.Join(out, ", ")
}
