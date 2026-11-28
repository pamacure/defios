package node

import (
	"fmt"
	"slices"
	"strings"

	"defios.com/internal/config"
)

// A profile's tier, from the Tools it grants, fixes where it may be
// included: a Key-tier one only in a Key's Conversation, a Group-tier one
// only in Group-level talk, an untiered one anywhere (ADR 0046). So an
// include, or a change to a profile's Tools, that would put a tiered
// profile in the wrong place is refused, naming every place it breaks.

// Misplaced is a tiered Agent Profile included, or to be, where its tier
// may not be.
type Misplaced struct {
	Handle string
	Tier   config.Tier
	In     Place
}

// MisplacedError: an include, or a change to a profile's Tools, refused
// because it would leave the Agents included where their tiers may not be.
type MisplacedError struct {
	Agents []Misplaced
}

func (e *MisplacedError) Error() string {
	parts := make([]string, len(e.Agents))
	for i, m := range e.Agents {
		parts[i] = fmt.Sprintf("@%s grants %s Tools, so may be included %s, not in %s", m.Handle, m.Tier, m.Tier.Where(), placeName(m.In))
	}
	return "node: " + strings.Join(parts, "; ")
}

// placeName names pl in a refusal.
func placeName(pl Place) string {
	if pl.Conversation == GroupTalk {
		return "Group-level talk of " + pl.Group
	}
	return fmt.Sprintf("Conversation %s of %s", pl.Workspace, pl.Group)
}

// checkPlaced refuses changing the list of agents at pl from before to
// after where after adds a profile whose tier may not be included there.
// One already there, by a hand edit, is left for its run to refuse.
func (h *Host) checkPlaced(pl Place, before, after []string) error {
	var out []Misplaced
	for _, handle := range after {
		if slices.Contains(before, handle) {
			continue
		}
		p, err := h.readProfile(handle)
		if err != nil {
			return err
		}
		tier, err := p.Tier()
		if err != nil {
			return &InvalidError{Err: err}
		}
		if !tier.Fits(pl.Conversation == GroupTalk) {
			out = append(out, Misplaced{Handle: handle, Tier: tier, In: pl})
		}
	}
	if len(out) > 0 {
		return &MisplacedError{Agents: out}
	}
	return nil
}

// checkStaysPlaced refuses p as handle's profile where its tier may not be
// included in a place that includes it.
func (h *Host) checkStaysPlaced(handle string, p config.Profile) error {
	tier, err := p.Tier()
	if err != nil {
		return &InvalidError{Err: err}
	}
	if tier == config.TierNone {
		return nil
	}
	in, err := h.includers()
	if err != nil {
		return err
	}
	var out []Misplaced
	for _, pl := range in[handle] {
		if !tier.Fits(pl.Conversation == GroupTalk) {
			out = append(out, Misplaced{Handle: handle, Tier: tier, In: pl})
		}
	}
	if len(out) > 0 {
		return &MisplacedError{Agents: out}
	}
	return nil
}
