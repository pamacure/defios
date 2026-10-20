// Package group holds the Roster and Group ID (docs/spec/mvp.md §2.2, §6).
// Formation itself lands in a later ticket.
package group

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"defios.com/internal/identity"
)

// groupIDDomain separates Group IDs from every other SHA-256 in the system.
const groupIDDomain = "defios-group-id-v1"

// ID is a 32-byte Group ID. A distinct type from identity.ID so the two
// cannot be passed for each other.
type ID [32]byte

// String is the lowercase hex display form.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

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

// RosterNode is one Roster entry: the Node's pinned keys plus its
// advertised display name and tsnet hostname.
type RosterNode struct {
	identity.Public
	DisplayName   string
	TsnetHostname string
}

// Roster is the Group's fixed set of Nodes, sorted by Node ID.
type Roster struct {
	nodes []RosterNode
}

// NewRoster sorts nodes by Node ID.
func NewRoster(nodes ...RosterNode) Roster {
	sorted := slices.Clone(nodes)
	slices.SortFunc(sorted, func(a, b RosterNode) int { return bytes.Compare(a.NodeID[:], b.NodeID[:]) })
	return Roster{nodes: sorted}
}

// Nodes returns the sorted Roster.
func (r Roster) Nodes() []RosterNode { return slices.Clone(r.nodes) }

// NodeIDs returns the sorted Node IDs.
func (r Roster) NodeIDs() []identity.ID {
	ids := make([]identity.ID, len(r.nodes))
	for i, n := range r.nodes {
		ids[i] = n.NodeID
	}
	return ids
}

// Lookup finds the Roster entry for a Node ID.
func (r Roster) Lookup(nodeID identity.ID) (RosterNode, bool) {
	for _, n := range r.nodes {
		if n.NodeID == nodeID {
			return n, true
		}
	}
	return RosterNode{}, false
}

// GroupID is the Group this Roster constitutes.
func (r Roster) GroupID() ID { return GroupID(r.NodeIDs()) }
