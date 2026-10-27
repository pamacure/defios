package cosl

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite" // pure-Go sqlite driver

	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// schema is docs/spec/mvp.md §5.5 plus envelope_hash (ADR 0012): the chain
// folds over the hash, so a NULL envelope (a pruned row) still verifies.
// PRAGMAs ride on the DSN so every connection gets them.
const schema = `
CREATE TABLE IF NOT EXISTS cosl (
  seq              INTEGER PRIMARY KEY,
  term             INTEGER NOT NULL,
  envelope         BLOB,
  envelope_hash    BLOB    NOT NULL,
  sender           BLOB    NOT NULL,
  msg_type         INTEGER NOT NULL,
  msg_id           BLOB    NOT NULL,
  conversation_id  BLOB,
  created_at       INTEGER NOT NULL,
  prev_hash        BLOB    NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_cosl_msg_id ON cosl(msg_id);
CREATE INDEX IF NOT EXISTS idx_cosl_conversation ON cosl(conversation_id, seq);
CREATE INDEX IF NOT EXISTS idx_cosl_type ON cosl(msg_type, seq);

CREATE TABLE IF NOT EXISTS cosl_meta (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
);
`

// triggers keep the replica append-only, by name. The one UPDATE they
// admit is Pruning: an app.freeform's (msg_type 6) envelope set to NULL,
// every other column unchanged. Open and Restore reinstall them when they
// differ, so a replica's guard is always this binary's, never an older
// one's or a snapshot's.
var triggers = map[string]string{
	"cosl_no_update": `CREATE TRIGGER cosl_no_update BEFORE UPDATE ON cosl
WHEN NOT (OLD.msg_type = 6 AND NEW.envelope IS NULL
  AND NEW.seq IS OLD.seq AND NEW.term IS OLD.term
  AND NEW.envelope_hash IS OLD.envelope_hash AND NEW.sender IS OLD.sender
  AND NEW.msg_type IS OLD.msg_type AND NEW.msg_id IS OLD.msg_id
  AND NEW.conversation_id IS OLD.conversation_id
  AND NEW.created_at IS OLD.created_at AND NEW.prev_hash IS OLD.prev_hash)
BEGIN SELECT RAISE(ABORT, 'cosl is append-only'); END`,
	"cosl_no_delete": `CREATE TRIGGER cosl_no_delete BEFORE DELETE ON cosl
BEGIN SELECT RAISE(ABORT, 'cosl is append-only'); END`,
}

// cosl_meta keys.
const (
	metaLastAppliedSeq  = "last_applied_seq"
	metaLastAppliedTerm = "last_applied_term"
	metaChainHead       = "chain_head"
)

var (
	// ErrNotFound is returned when no row matches.
	ErrNotFound = errors.New("cosl: no such row")
	// ErrAmbiguous is returned when a prefix matches more than one row.
	ErrAmbiguous = errors.New("cosl: ambiguous msg_id prefix")
)

// Store is one Node's COSL replica. The FSM is its only appender and Prune
// its only other writer; CLI reads go through WAL. It announces each
// committed row and each restore to whoever waits on Changed.
type Store struct {
	db       *sql.DB
	restores atomic.Uint64

	changeMu sync.Mutex
	changed  chan struct{} // closed and replaced on each change
}

// Open opens or creates the replica at path.
func Open(path string) (*Store, error) {
	// Every transaction here writes, so each takes the write lock as it
	// begins and waits out busy_timeout for it. A deferred one would read
	// first and then fail at once, with no wait, if a Prune committed in
	// between, losing the FSM's row.
	dsn := "file:" + path + "?" + url.Values{
		"_pragma": {"journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(5000)"},
		"_txlock": {"immediate"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cosl: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("cosl: schema: %w", err)
	}
	if err := installTriggers(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, changed: make(chan struct{})}, nil
}

// installTriggers makes the replica's triggers exactly this binary's. When
// they already are it only reads, so opening a replica for a CLI read never
// contends with serve's FSM for the write lock; otherwise it replaces them
// in one transaction, so no writer sees the replica unguarded in between.
func installTriggers(db *sql.DB) error {
	rows, err := db.Query("SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND tbl_name = 'cosl'")
	if err != nil {
		return fmt.Errorf("cosl: triggers: %w", err)
	}
	have := map[string]string{}
	for rows.Next() {
		var name, stmt string
		if err := rows.Scan(&name, &stmt); err != nil {
			rows.Close()
			return fmt.Errorf("cosl: triggers: %w", err)
		}
		have[name] = stmt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("cosl: triggers: %w", err)
	}
	if maps.Equal(have, triggers) {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("cosl: triggers: %w", err)
	}
	defer tx.Rollback()
	for name := range have {
		// The name may come from a snapshot, so quote it as an identifier.
		if _, err := tx.Exec(`DROP TRIGGER IF EXISTS "` + strings.ReplaceAll(name, `"`, `""`) + `"`); err != nil {
			return fmt.Errorf("cosl: triggers: %w", err)
		}
	}
	for _, stmt := range triggers {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("cosl: triggers: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cosl: triggers: %w", err)
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Row is one COSL row; Envelope is the source of truth while it is held,
// the rest is denormalized for queries. Envelope is nil once the row is
// pruned; EnvelopeHash, SHA-256 of it, is what the chain folds over.
type Row struct {
	Seq            uint64
	Term           uint64
	Envelope       []byte
	EnvelopeHash   [32]byte
	Sender         identity.ID
	MsgType        uint32
	MsgID          [32]byte
	ConversationID []byte // nil = Group-level
	CreatedAt      int64
	PrevHash       [32]byte
}

// Meta is the catch-up bookkeeping in cosl_meta.
type Meta struct {
	LastAppliedSeq  uint64
	LastAppliedTerm uint64
	ChainHead       [32]byte // genesis: 32 zero bytes
}

// Query filters Log. MsgTypes, when set, keeps rows of those message types
// only, so a reader decodes only what it folds (ADR 0014). AfterSeq keeps
// rows with a seq above it, so a reader reads only what it has not seen.
// GroupLevel keeps only rows whose conversation id is unset (§8).
// ConversationID, when set, keeps rows where conversation_id equals it.
// Limit > 0 returns the latest Limit matching rows, in seq order.
type Query struct {
	MsgTypes       []uint32
	AfterSeq       uint64
	GroupLevel     bool
	ConversationID []byte
	Limit          int
}

const rowColumns = "seq, term, envelope, envelope_hash, sender, msg_type, msg_id, conversation_id, created_at, prev_hash"

// Log returns rows in seq order.
func (s *Store) Log(q Query) ([]Row, error) {
	var conds []string
	args := []any{}
	if len(q.MsgTypes) > 0 {
		conds = append(conds, "msg_type IN (?"+strings.Repeat(", ?", len(q.MsgTypes)-1)+")")
		for _, t := range q.MsgTypes {
			args = append(args, t)
		}
	}
	if q.AfterSeq > 0 {
		conds = append(conds, "seq > ?")
		args = append(args, q.AfterSeq)
	}
	if q.GroupLevel {
		conds = append(conds, "conversation_id IS NULL")
	}
	if q.ConversationID != nil {
		conds = append(conds, "conversation_id = ?")
		args = append(args, q.ConversationID)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	stmt := fmt.Sprintf("SELECT %s FROM cosl %s ORDER BY seq", rowColumns, where)
	if q.Limit > 0 {
		stmt = fmt.Sprintf("SELECT %s FROM (SELECT %s FROM cosl %s ORDER BY seq DESC LIMIT ?) ORDER BY seq", rowColumns, rowColumns, where)
		args = append(args, q.Limit)
	}
	rows, err := s.db.Query(stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("cosl: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ByMsgIDPrefix finds the single row whose msg_id starts with prefix.
func (s *Store) ByMsgIDPrefix(prefix []byte) (Row, error) {
	if prefix == nil {
		prefix = []byte{} // a nil slice would bind as NULL and match nothing
	}
	stmt := fmt.Sprintf("SELECT %s FROM cosl WHERE substr(msg_id, 1, ?) = ? LIMIT 2", rowColumns)
	rows, err := s.db.Query(stmt, len(prefix), prefix)
	if err != nil {
		return Row{}, fmt.Errorf("cosl: %w", err)
	}
	defer rows.Close()
	var matches []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return Row{}, err
		}
		matches = append(matches, r)
	}
	if err := rows.Err(); err != nil {
		return Row{}, fmt.Errorf("cosl: %w", err)
	}
	switch len(matches) {
	case 0:
		return Row{}, ErrNotFound
	case 1:
		return matches[0], nil
	default:
		return Row{}, ErrAmbiguous
	}
}

// VacuumInto writes a consistent copy of the replica to path, which must
// not exist. This is the snapshot source; the live file is never rewritten.
func (s *Store) VacuumInto(path string) error {
	if _, err := s.db.Exec("VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("cosl: vacuum into: %w", err)
	}
	return nil
}

// Prune destroys the payload of one Conversation's app.freeform rows at or
// below seq through (nil conversationID is Group-level talk), keeping each
// row, its envelope_hash and so its place in the chain (ADR 0012). It is
// Node-local and appends nothing; a row already pruned is skipped. It
// reports how many payloads it destroyed. Which seq is safe to prune
// through is the caller's: the live Recap's base_seq.
func (s *Store) Prune(conversationID []byte, through uint64) (int, error) {
	conv := "conversation_id IS NULL"
	args := []any{TypeAppFreeform, through}
	if conversationID != nil {
		conv = "conversation_id = ?"
		args = append(args, conversationID)
	}
	res, err := s.db.Exec(`UPDATE cosl SET envelope = NULL
		WHERE msg_type = ? AND seq <= ? AND envelope IS NOT NULL AND `+conv, args...)
	if err != nil {
		return 0, fmt.Errorf("cosl: prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cosl: prune: %w", err)
	}
	return int(n), nil
}

// Restores is how many times Restore has rewritten this replica since Open.
// Each one may bring back payloads this Node pruned.
func (s *Store) Restores() uint64 { return s.restores.Load() }

// Meta reads cosl_meta; a fresh replica reports zeroes and a genesis head.
func (s *Store) Meta() (Meta, error) {
	return readMeta(s.db)
}

// Pruned reports whether this Node has destroyed the row's payload, with the
// envelope that carried it; only its metadata and envelope_hash remain.
func (r Row) Pruned() bool { return r.Envelope == nil }

// Decode parses the stored envelope bytes.
func (r Row) Decode() (*coslv1.Envelope, error) {
	var env coslv1.Envelope
	if err := proto.Unmarshal(r.Envelope, &env); err != nil {
		return nil, fmt.Errorf("cosl: seq %d: stored envelope is unreadable: %w", r.Seq, err)
	}
	return &env, nil
}

// append is §5.4 steps 6–7 for an already-verified envelope: INSERT OR
// IGNORE on msg_id, extend the prev_hash chain, record last_applied. One
// transaction, so a crash leaves either all or none. Reports false on a
// duplicate msg_id.
func (s *Store) append(index, term uint64, data []byte, env *coslv1.Envelope) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("cosl: %w", err)
	}
	defer tx.Rollback()

	meta, err := readMeta(tx)
	if err != nil {
		return false, err
	}
	envHash := sha256.Sum256(data)
	head := chainHash(meta.ChainHead, index, term, envHash)

	var conversationID any
	if env.ConversationId != nil {
		conversationID = env.ConversationId
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO cosl
		(seq, term, envelope, envelope_hash, sender, msg_type, msg_id, conversation_id, created_at, prev_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		index, term, data, envHash[:], env.Sender, env.MsgType, env.MsgId, conversationID, env.CreatedAt, head[:])
	if err != nil {
		return false, fmt.Errorf("cosl: insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cosl: %w", err)
	}
	if n == 1 {
		if err := setMeta(tx, metaChainHead, head[:]); err != nil {
			return false, err
		}
	}
	if err := setLastApplied(tx, index, term); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("cosl: %w", err)
	}
	if n == 1 {
		s.announce()
	}
	return n == 1, nil
}

// markApplied records last_applied for an entry that produced no row.
func (s *Store) markApplied(index, term uint64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("cosl: %w", err)
	}
	defer tx.Rollback()
	if err := setLastApplied(tx, index, term); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cosl: %w", err)
	}
	return nil
}

// chainHash is SHA-256(prev || seq_be64 || term_be64 || envelope_hash),
// where envelope_hash is SHA-256(envelope_bytes).
func chainHash(prev [32]byte, seq, term uint64, envelopeHash [32]byte) [32]byte {
	h := sha256.New()
	h.Write(prev[:])
	h.Write(binary.BigEndian.AppendUint64(nil, seq))
	h.Write(binary.BigEndian.AppendUint64(nil, term))
	h.Write(envelopeHash[:])
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

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func readMeta(q querier) (Meta, error) {
	var m Meta
	var seq, term, head []byte
	if err := scanMeta(q, metaLastAppliedSeq, &seq); err != nil {
		return Meta{}, err
	}
	if err := scanMeta(q, metaLastAppliedTerm, &term); err != nil {
		return Meta{}, err
	}
	if err := scanMeta(q, metaChainHead, &head); err != nil {
		return Meta{}, err
	}
	if seq != nil {
		m.LastAppliedSeq = beUint64(seq)
	}
	if term != nil {
		m.LastAppliedTerm = beUint64(term)
	}
	copy(m.ChainHead[:], head)
	return m, nil
}

func scanMeta(q querier, key string, dst *[]byte) error {
	err := q.QueryRow("SELECT value FROM cosl_meta WHERE key = ?", key).Scan(dst)
	if errors.Is(err, sql.ErrNoRows) {
		*dst = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("cosl: meta %s: %w", key, err)
	}
	return nil
}

func scanRow(rows *sql.Rows) (Row, error) {
	var r Row
	var envHash, sender, msgID, prevHash []byte
	if err := rows.Scan(&r.Seq, &r.Term, &r.Envelope, &envHash, &sender, &r.MsgType, &msgID, &r.ConversationID, &r.CreatedAt, &prevHash); err != nil {
		return Row{}, fmt.Errorf("cosl: %w", err)
	}
	copy(r.EnvelopeHash[:], envHash)
	copy(r.Sender[:], sender)
	copy(r.MsgID[:], msgID)
	copy(r.PrevHash[:], prevHash)
	return r, nil
}

func beUint64(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}
