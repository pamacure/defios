package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"defios.com/internal/flock"
	"defios.com/internal/group"
	"defios.com/internal/identity"
)

// LiveryColours are the colours of a Livery, in the order a picker shows
// them; desktop/frontend/style.css defines each for every theme.
var LiveryColours = []string{"blue", "indigo", "violet", "magenta", "orange", "cyan", "slate", "brown"}

// LiveryBricks are the bricks of a Livery, in the order a picker shows
// them; each is desktop/frontend/bricks/<name>.svg. A name is how a Livery
// is written, so it never changes once released.
var LiveryBricks = []string{"stud", "twin", "plate", "tile", "slope", "arch", "round", "corner"}

// NoLivery, as the Livery SetLivery or a ProfileEdit gives, clears a
// party's Livery.
const NoLivery = "none"

// The kinds of party a Livery marks, as the prefix of its key: one of this
// Node's Agent Profiles (agent:<handle>), another Node (node:<node id>), or
// another Node's Agent (node:<node id>/<handle>).
const (
	agentParty = "agent:"
	nodeParty  = "node:"
)

// AgentParty is the party key of this Node's Agent Profile handle.
func AgentParty(handle string) string { return agentParty + handle }

// NodeParty is the party key of the Node nodeID.
func NodeParty(nodeID string) string { return nodeParty + nodeID }

// NodeAgentParty is the party key of the Node nodeID's Agent handle.
func NodeAgentParty(nodeID, handle string) string { return nodeParty + nodeID + "/" + handle }

// LiveryTakenError: a write refused because another party holds the
// Livery.
type LiveryTakenError struct {
	Livery string
	Holder string // the party key that holds it
}

func (e *LiveryTakenError) Error() string {
	return fmt.Sprintf("Livery %s is already %s's; clear it there first", e.Livery, e.Holder)
}

// allLiveries is every Livery, colour by colour.
func allLiveries() []string {
	out := make([]string, 0, len(LiveryColours)*len(LiveryBricks))
	for _, c := range LiveryColours {
		for _, b := range LiveryBricks {
			out = append(out, c+"-"+b)
		}
	}
	return out
}

// validLivery refuses what is not <colour>-<brick>.
func validLivery(l string) error {
	c, b, ok := strings.Cut(l, "-")
	if !ok || !slices.Contains(LiveryColours, c) || !slices.Contains(LiveryBricks, b) {
		return invalidf("%q is not a Livery: write <colour>-<brick>, with a colour of %s and a brick of %s, or %s",
			l, strings.Join(LiveryColours, " "), strings.Join(LiveryBricks, " "), NoLivery)
	}
	return nil
}

// validParty refuses a party key that names no party.
func validParty(party string) error {
	if handle, ok := strings.CutPrefix(party, agentParty); ok {
		return validHandle(handle)
	}
	rest, ok := strings.CutPrefix(party, nodeParty)
	if !ok {
		return invalidf("%q names no party: write agent:<handle>, node:<node id> or node:<node id>/<handle>", party)
	}
	id, handle, agent := strings.Cut(rest, "/")
	if _, err := identity.ParseID(id); err != nil {
		return invalidf("%q names no Node: %v", party, err)
	}
	if agent {
		return validHandle(handle)
	}
	return nil
}

// Liveries is every Livery this Node has given, by party key. A value
// that is no Livery reads as none; a duplicate reads as it is.
func (h *Host) Liveries(ctx context.Context) (map[string]string, error) {
	return h.readLiveries()
}

// FreeLiveries is every Livery no party holds, colour by colour.
func (h *Host) FreeLiveries(ctx context.Context) ([]string, error) {
	held, err := h.readLiveries()
	if err != nil {
		return nil, err
	}
	return freeOf(held), nil
}

// SetLivery gives party the Livery livery, or with NoLivery clears it. A
// Livery another party holds is a *LiveryTakenError; an agent: party must
// be one of this Node's Agent Profiles.
func (h *Host) SetLivery(ctx context.Context, party, livery string) error {
	if err := validParty(party); err != nil {
		return err
	}
	if livery != NoLivery {
		if err := validLivery(livery); err != nil {
			return err
		}
	}
	if handle, ok := strings.CutPrefix(party, agentParty); ok {
		if _, err := h.readProfile(handle); err != nil {
			return err
		}
	}
	return h.editLiveries(func(held map[string]string) error {
		return setLivery(held, party, livery)
	})
}

// setLivery sets party's Livery in held, refusing one another party holds.
func setLivery(held map[string]string, party, livery string) error {
	if livery == NoLivery {
		delete(held, party)
		return nil
	}
	for p, l := range held {
		if l == livery && p != party {
			return &LiveryTakenError{Livery: livery, Holder: p}
		}
	}
	held[party] = livery
	return nil
}

// freeOf is every Livery held does not hold.
func freeOf(held map[string]string) []string {
	taken := map[string]bool{}
	for _, l := range held {
		taken[l] = true
	}
	var out []string
	for _, l := range allLiveries() {
		if !taken[l] {
			out = append(out, l)
		}
	}
	return out
}

// assign gives party a random free Livery unless it has one; with none
// free it stays without. It reports whether held changed.
func assign(held map[string]string, party string) bool {
	if _, ok := held[party]; ok {
		return false
	}
	free := freeOf(held)
	if len(free) == 0 {
		return false
	}
	held[party] = free[rand.IntN(len(free))]
	return true
}

// readLiveries reads liveries.toml; an absent one holds none.
func (h *Host) readLiveries() (map[string]string, error) {
	held := map[string]string{}
	raw, err := os.ReadFile(h.p.LiveriesFile())
	if errors.Is(err, os.ErrNotExist) {
		return held, nil
	} else if err != nil {
		return nil, err
	}
	var file map[string]any
	if err := toml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", h.p.LiveriesFile(), err)
	}
	for party, v := range file {
		if l, ok := v.(string); ok && validParty(party) == nil && validLivery(l) == nil {
			held[party] = l
		}
	}
	return held, nil
}

// editLiveries applies edit to liveries.toml under its lock, which every
// process editing it holds, and writes it back if edit changed it. A
// refused edit writes nothing. edit is tried first without the lock, so
// one that would change nothing, as most sightings of a party are, takes
// no lock and writes nothing.
func (h *Host) editLiveries(edit func(held map[string]string) error) error {
	held, err := h.readLiveries()
	if err != nil {
		return err
	}
	tried := maps.Clone(held)
	if err := edit(tried); err != nil {
		return err
	}
	if maps.Equal(tried, held) {
		return nil
	}
	path := h.p.LiveriesFile()
	if err := mkdirPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() // closing the file releases the lock
	if err := flock.Lock(lock, true, true); err != nil {
		return fmt.Errorf("locking %s: %w", path, err)
	}
	if held, err = h.readLiveries(); err != nil {
		return err
	}
	before := maps.Clone(held)
	if err := edit(held); err != nil {
		return err
	}
	if maps.Equal(held, before) {
		return nil
	}
	var b strings.Builder
	b.WriteString("# The Livery this Node marks each party of talk with: <colour>-<brick>. See ADR 0037.\n")
	if err := toml.NewEncoder(&b).Encode(held); err != nil {
		return err
	}
	return writePrivate(path, []byte(b.String()))
}

// seeRosters gives each Node in a Roster this Node holds, but itself, a
// Livery the first time it is seen, and frees the Liveries of Nodes, and
// their Agents, that are in no Roster any more. The Node has no verb that
// removes a Group, so this is how removing one frees the Liveries of
// devices in no Group left: at the next sighting, whatever front end
// hosts the Node. A Record that does not load keeps every Livery, since
// its Roster cannot be told. Liveries only mark talk, so a failure here
// is logged, never returned.
func (h *Host) seeRosters() {
	aliases, err := h.aliases()
	if err != nil {
		h.o.Logf("node: Liveries: %v", err)
		return
	}
	in, prune := map[string]bool{}, true
	for _, a := range aliases {
		rec, err := group.LoadAlias(h.p.GroupsDir(), a)
		if errors.Is(err, group.ErrNoGroup) {
			continue
		} else if err != nil {
			prune = false
			continue
		}
		for _, n := range rec.Roster().Nodes() {
			if n.NodeID != h.pub.NodeID {
				in[n.NodeID.String()] = true
			}
		}
	}
	err = h.editLiveries(func(held map[string]string) error {
		if prune {
			for party := range held {
				if rest, ok := strings.CutPrefix(party, nodeParty); ok {
					if id, _, _ := strings.Cut(rest, "/"); !in[id] {
						delete(held, party)
					}
				}
			}
		}
		for _, id := range slices.Sorted(maps.Keys(in)) {
			assign(held, NodeParty(id))
		}
		return nil
	})
	if err != nil {
		h.o.Logf("node: Liveries: %v", err)
	}
}

// seeAgents gives each other Node's Agent among parties, node:<id>/<handle>
// keys, a Livery the first time it is seen, logging a failure.
func (h *Host) seeAgents(parties []string) {
	if len(parties) == 0 {
		return
	}
	err := h.editLiveries(func(held map[string]string) error {
		for _, p := range parties {
			assign(held, p)
		}
		return nil
	})
	if err != nil {
		h.o.Logf("node: Liveries: %v", err)
	}
}
