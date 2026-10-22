package group

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

// Status of this Node's Group (§6.2 step 5).
type Status int

const (
	// Pending: the proposal is held but not every Roster Node has accepted.
	Pending Status = iota + 1
	// Valid: the proposal plus an Accept from every Roster Node is held.
	Valid
)

func (s Status) String() string {
	switch s {
	case Pending:
		return "pending"
	case Valid:
		return "valid"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// MarshalJSON writes the lowercase name.
func (s Status) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// ErrNoGroup means this Node holds no Group, pending or valid.
var ErrNoGroup = errors.New("group: this Node has no Group")

// Record is this Node's Group as held on disk: the verified proposal plus
// every Accept received so far. The initiator's create is its Accept and
// is not stored separately.
type Record struct {
	proposal Proposal
	accepts  map[identity.ID]*groupv1.CreateAccept
}

// GroupID is the Group this Record is for.
func (r *Record) GroupID() ID { return r.proposal.GroupID }

// Roster is the fixed set of Nodes the proposal named.
func (r *Record) Roster() Roster { return r.proposal.Roster }

// Initiator is the Node that proposed the Group.
func (r *Record) Initiator() identity.ID { return r.proposal.Initiator }

// Accepted reports whether node's Accept is held. The initiator always
// has.
func (r *Record) Accepted(node identity.ID) bool {
	if node == r.proposal.Initiator {
		return true
	}
	_, ok := r.accepts[node]
	return ok
}

// holds reports whether a signed Accept from node is stored (the
// initiator's implicit one is not).
func (r *Record) holds(node identity.ID) bool {
	_, ok := r.accepts[node]
	return ok
}

// Status is Valid once every Roster Node has accepted.
func (r *Record) Status() Status {
	for _, id := range r.proposal.Roster.NodeIDs() {
		if !r.Accepted(id) {
			return Pending
		}
	}
	return Valid
}

// ImplicitAlias is the Group alias the test harness and single-Group tests
// use. Storage is alias-keyed (ADR 0007); a Node may now hold several Groups
// at once (#27), each under its own alias.
const ImplicitAlias = "default"

// aliasPattern is the validation for a Group alias (ADR 0007): lowercase,
// filesystem-safe, 1–64 chars, starting with a letter or digit. Aliases are
// local and never appear on the wire.
var aliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidateAlias reports whether alias is a well-formed Group alias.
func ValidateAlias(alias string) error {
	if !aliasPattern.MatchString(alias) {
		return fmt.Errorf("group: %q is not a valid alias; use 1–64 chars matching [a-z0-9][a-z0-9_-]*", alias)
	}
	return nil
}

// recordFileName is the on-disk Record, group/<alias>/roster.json (ADR 0007),
// which binds the alias to its Group ID: the signed wire messages, re-verified
// on load.
const recordFileName = "roster.json"

type recordFileV1 struct {
	Version  int      `json:"version"`
	Proposal []byte   `json:"proposal"` // CreateProposal bytes
	Accepts  [][]byte `json:"accepts"`  // CreateAccept bytes
}

// LoadAlias reads the Record in the alias subtree under groupsDir. ErrNoGroup
// if that Group is scaffolded but not yet formed (no roster.json). This is the
// per-Group read now that a Node may hold several Groups at once (#27).
func LoadAlias(groupsDir, alias string) (*Record, error) {
	path := filepath.Join(groupsDir, alias, recordFileName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoGroup
	} else if err != nil {
		return nil, err
	}
	return loadRecord(path)
}

// Load reads the one Record under groupsDir, whatever alias subtree it lives
// in, for callers that still assume a single Group (some tests, the formation
// helpers). ErrNoGroup if there is none; an error if more than one is found.
// Per-Group callers use LoadAlias instead (#27).
func Load(groupsDir string) (*Record, error) {
	matches, err := filepath.Glob(filepath.Join(groupsDir, "*", recordFileName))
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, ErrNoGroup
	case 1:
	default:
		return nil, fmt.Errorf("group: found %d Groups under %s; Load expects a single Group, use LoadAlias", len(matches), groupsDir)
	}
	return loadRecord(matches[0])
}

func loadRecord(path string) (*Record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f recordFileV1
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("group: %s: %w", path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("group: %s: unsupported version %d", path, f.Version)
	}
	var p groupv1.CreateProposal
	if err := proto.Unmarshal(f.Proposal, &p); err != nil {
		return nil, fmt.Errorf("group: %s: %w", path, err)
	}
	prop, err := VerifyProposal(&p)
	if err != nil {
		return nil, fmt.Errorf("group: %s: %w", path, err)
	}
	r := &Record{proposal: prop, accepts: map[identity.ID]*groupv1.CreateAccept{}}
	for _, b := range f.Accepts {
		var a groupv1.CreateAccept
		if err := proto.Unmarshal(b, &a); err != nil {
			return nil, fmt.Errorf("group: %s: %w", path, err)
		}
		who, err := VerifyAccept(&a, prop)
		if err != nil {
			return nil, fmt.Errorf("group: %s: %w", path, err)
		}
		r.accepts[who] = &a
	}
	return r, nil
}

// save writes r under groupDir (the Group's alias subtree) atomically,
// 0700/0600.
func (r *Record) save(groupDir string) error {
	if err := os.MkdirAll(groupDir, 0o700); err != nil {
		return err
	}
	f := recordFileV1{Version: 1}
	var err error
	if f.Proposal, err = proto.Marshal(r.proposal.raw); err != nil {
		return err
	}
	for _, id := range r.proposal.Roster.NodeIDs() { // deterministic order
		if a, ok := r.accepts[id]; ok {
			b, err := proto.Marshal(a)
			if err != nil {
				return err
			}
			f.Accepts = append(f.Accepts, b)
		}
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(groupDir, recordFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// remove deletes the Record file under groupDir, freeing the Slot. Only the
// roster.json is removed: the alias subtree also holds the Group's tsnet
// state, which outlives a cancelled proposal (a Group is never dropped once
// valid, so no replica state is present). A missing file is success.
func (r *Record) remove(groupDir string) error {
	err := os.Remove(filepath.Join(groupDir, recordFileName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
