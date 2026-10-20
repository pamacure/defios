package cosl

import (
	"bytes"
	"fmt"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// FSM verifies committed log entries into the replica (docs/spec/mvp.md
// §5.4). It is deliberately free of hashicorp/raft: the Raft layer adapts
// raft.Log to Apply and only passes LogCommand entries.
type FSM struct {
	store   *Store
	roster  group.Roster
	groupID group.ID
}

// NewFSM binds a replica to the Roster whose keys it verifies against.
func NewFSM(store *Store, roster group.Roster) *FSM {
	return &FSM{store: store, roster: roster, groupID: roster.GroupID()}
}

// Result is what Apply reports to the Raft layer. Not inserting is not an
// error: the entry still occupies its raft index on every replica.
type Result struct {
	Inserted bool
	Reason   string // why nothing was inserted; empty when Inserted
}

func skip(format string, args ...any) Result {
	return Result{Reason: fmt.Sprintf(format, args...)}
}

// Apply runs the §5.4 rules in order for the entry at (index, term) and
// records last_applied in cosl_meta whether or not a row was inserted.
// The returned error is a storage failure only.
func (f *FSM) Apply(index, term uint64, data []byte) (Result, error) {
	env, rejected := f.verify(data)
	if env == nil {
		if err := f.store.markApplied(index, term); err != nil {
			return Result{}, err
		}
		return rejected, nil
	}
	inserted, err := f.store.append(index, term, data, env)
	if err != nil {
		return Result{}, err
	}
	if !inserted {
		return skip("duplicate msg_id"), nil
	}
	return Result{Inserted: true}, nil
}

// verify is §5.4 steps 1–5. A nil envelope means the entry is rejected
// for the Result's Reason.
func (f *FSM) verify(data []byte) (*coslv1.Envelope, Result) {
	var env coslv1.Envelope
	if err := proto.Unmarshal(data, &env); err != nil {
		return nil, skip("unmarshal: %v", err)
	}
	if env.Version != 1 {
		return nil, skip("unknown version %d", env.Version)
	}
	if !bytes.Equal(env.GroupId, f.groupID[:]) {
		return nil, skip("group_id mismatch")
	}
	var sender identity.ID
	if len(env.Sender) != len(sender) {
		return nil, skip("sender is not a Node ID; not on roster")
	}
	copy(sender[:], env.Sender)
	node, ok := f.roster.Lookup(sender)
	if !ok {
		return nil, skip("sender %s not on roster", sender)
	}
	if err := Verify(&env, node.AuthPublicKey); err != nil {
		return nil, skip("signature does not verify for sender %s", sender)
	}
	return &env, Result{}
}
