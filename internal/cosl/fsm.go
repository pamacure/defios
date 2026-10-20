package cosl

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
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
	res := f.verify(data)
	tx, err := f.store.db.Begin()
	if err != nil {
		return Result{}, fmt.Errorf("cosl: %w", err)
	}
	defer tx.Rollback()

	if res.env != nil {
		inserted, err := insertRow(tx, index, term, data, res.env)
		if err != nil {
			return Result{}, err
		}
		if inserted {
			res.result = Result{Inserted: true}
		} else {
			res.result = skip("duplicate msg_id")
		}
	}
	if err := setLastApplied(tx, index, term); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("cosl: %w", err)
	}
	return res.result, nil
}

type verified struct {
	env    *coslv1.Envelope // nil when the entry must not be inserted
	result Result
}

// verify is §5.4 steps 1–5.
func (f *FSM) verify(data []byte) verified {
	var env coslv1.Envelope
	if err := proto.Unmarshal(data, &env); err != nil {
		return verified{result: skip("unmarshal: %v", err)}
	}
	if env.Version != 1 {
		return verified{result: skip("unknown version %d", env.Version)}
	}
	if !bytes.Equal(env.GroupId, f.groupID[:]) {
		return verified{result: skip("group_id mismatch")}
	}
	var sender identity.ID
	if len(env.Sender) != len(sender) {
		return verified{result: skip("sender is not a Node ID; not on roster")}
	}
	copy(sender[:], env.Sender)
	member, ok := f.roster.Lookup(sender)
	if !ok {
		return verified{result: skip("sender %s not on roster", sender)}
	}
	if err := Verify(&env, member.AuthPublicKey); err != nil {
		return verified{result: skip("signature does not verify for sender %s", sender)}
	}
	return verified{env: &env}
}

// insertRow is §5.4 steps 6–7: INSERT OR IGNORE on msg_id, then extend
// the prev_hash chain. Reports false on a duplicate.
func insertRow(tx *sql.Tx, index, term uint64, data []byte, env *coslv1.Envelope) (bool, error) {
	meta, err := readMeta(tx)
	if err != nil {
		return false, err
	}
	prevHash := chainHash(meta.ChainHead, index, term, data)

	var conversationID any
	if env.ConversationId != nil {
		conversationID = env.ConversationId
	}
	r, err := tx.Exec(`INSERT OR IGNORE INTO cosl
		(seq, term, envelope, sender, msg_type, msg_id, conversation_id, created_at, prev_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		index, term, data, env.Sender, env.MsgType, env.MsgId, conversationID, env.CreatedAt, prevHash[:])
	if err != nil {
		return false, fmt.Errorf("cosl: insert: %w", err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cosl: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if err := setMeta(tx, metaChainHead, prevHash[:]); err != nil {
		return false, err
	}
	return true, nil
}

// chainHash is SHA-256(prev || seq_be64 || term_be64 || envelope_bytes).
func chainHash(prev [32]byte, seq, term uint64, envelope []byte) [32]byte {
	h := sha256.New()
	h.Write(prev[:])
	h.Write(binary.BigEndian.AppendUint64(nil, seq))
	h.Write(binary.BigEndian.AppendUint64(nil, term))
	h.Write(envelope)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func setLastApplied(tx *sql.Tx, index, term uint64) error {
	if err := setMeta(tx, metaLastAppliedSeq, binary.BigEndian.AppendUint64(nil, index)); err != nil {
		return err
	}
	return setMeta(tx, metaLastAppliedTerm, binary.BigEndian.AppendUint64(nil, term))
}

func setMeta(tx *sql.Tx, key string, value []byte) error {
	if _, err := tx.Exec(`INSERT INTO cosl_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("cosl: meta %s: %w", key, err)
	}
	return nil
}

func beUint64(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}
