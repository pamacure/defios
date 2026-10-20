package cosl

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"

	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite" // pure-Go sqlite driver

	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// schema is docs/spec/mvp.md §5.5 verbatim. PRAGMAs ride on the DSN so every
// connection gets them.
const schema = `
CREATE TABLE IF NOT EXISTS cosl (
  seq              INTEGER PRIMARY KEY,
  term             INTEGER NOT NULL,
  envelope         BLOB    NOT NULL,
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

CREATE TRIGGER IF NOT EXISTS cosl_no_update BEFORE UPDATE ON cosl
BEGIN SELECT RAISE(ABORT, 'cosl is append-only'); END;
CREATE TRIGGER IF NOT EXISTS cosl_no_delete BEFORE DELETE ON cosl
BEGIN SELECT RAISE(ABORT, 'cosl is append-only'); END;
`

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

// Store is one Node's COSL replica. The FSM is its only writer; CLI reads
// go through WAL.
type Store struct {
	db *sql.DB
}

// Open opens or creates the replica at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?" + url.Values{
		"_pragma": {"journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(5000)"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cosl: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("cosl: schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Row is one COSL row; Envelope is the source of truth, the rest is
// denormalized for queries.
type Row struct {
	Seq            uint64
	Term           uint64
	Envelope       []byte
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

// Query filters Log. Limit > 0 returns the latest Limit rows, in seq order.
type Query struct {
	MsgType *uint32
	Limit   int
}

const rowColumns = "seq, term, envelope, sender, msg_type, msg_id, conversation_id, created_at, prev_hash"

// Log returns rows in seq order.
func (s *Store) Log(q Query) ([]Row, error) {
	where, args := "", []any{}
	if q.MsgType != nil {
		where = "WHERE msg_type = ?"
		args = append(args, *q.MsgType)
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

// Meta reads cosl_meta; a fresh replica reports zeroes and a genesis head.
func (s *Store) Meta() (Meta, error) {
	return readMeta(s.db)
}

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
	head := chainHash(meta.ChainHead, index, term, data)

	var conversationID any
	if env.ConversationId != nil {
		conversationID = env.ConversationId
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO cosl
		(seq, term, envelope, sender, msg_type, msg_id, conversation_id, created_at, prev_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		index, term, data, env.Sender, env.MsgType, env.MsgId, conversationID, env.CreatedAt, head[:])
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
	var sender, msgID, prevHash []byte
	if err := rows.Scan(&r.Seq, &r.Term, &r.Envelope, &sender, &r.MsgType, &msgID, &r.ConversationID, &r.CreatedAt, &prevHash); err != nil {
		return Row{}, fmt.Errorf("cosl: %w", err)
	}
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
