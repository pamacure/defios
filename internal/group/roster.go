// Package group holds the Roster and Group ID (docs/spec/mvp.md §2.2, §6).
// Formation itself lands in a later ticket.
package group

import (
	"bytes"
	"crypto/sha256"
	"slices"

	"defios.com/internal/identity"
)

// groupIDDomain separates Group IDs from every other SHA-256 in the system.
const groupIDDomain = "defios-group-id-v1"

// ID is a 32-byte Group ID.
type ID = identity.ID

// GroupID derives SHA-256("defios-group-id-v1" || sorted Node IDs). Node IDs
// are sorted as raw bytes, so the same Roster is the same Group.
func GroupID(nodeIDs []identity.ID) ID {
	sorted := slices.Clone(nodeIDs)
	slices.SortFunc(sorted, func(a, b identity.ID) int { return bytes.Compare(a[:], b[:]) })
	h := sha256.New()
	h.Write([]byte(groupIDDomain))
	for _, id := range sorted {
		h.Write(id[:])
	}
	var out ID
	copy(out[:], h.Sum(nil))
	return out
}

// Member is one Roster entry: the Node's pinned keys plus its advertised
// display name and tsnet hostname.
type Member struct {
	identity.Public
	DisplayName   string
	TsnetHostname string
}

// Roster is the Group's fixed set of Nodes, sorted by Node ID.
type Roster struct {
	members []Member
}

// NewRoster sorts members by Node ID.
func NewRoster(members ...Member) Roster {
	sorted := slices.Clone(members)
	slices.SortFunc(sorted, func(a, b Member) int { return bytes.Compare(a.NodeID[:], b.NodeID[:]) })
	return Roster{members: sorted}
}

// Members returns the sorted Roster.
func (r Roster) Members() []Member { return slices.Clone(r.members) }

// NodeIDs returns the sorted Node IDs.
func (r Roster) NodeIDs() []identity.ID {
	ids := make([]identity.ID, len(r.members))
	for i, m := range r.members {
		ids[i] = m.NodeID
	}
	return ids
}

// Lookup finds the Member for a Node ID.
func (r Roster) Lookup(nodeID identity.ID) (Member, bool) {
	for _, m := range r.members {
		if m.NodeID == nodeID {
			return m, true
		}
	}
	return Member{}, false
}

// GroupID is the Group this Roster constitutes.
func (r Roster) GroupID() ID { return GroupID(r.NodeIDs()) }
