package cosl

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"

	"modernc.org/sqlite"
)

// ErrBadChain means a snapshot's prev_hash chain does not verify, an
// envelope it still holds does not match its envelope_hash, or it does not
// extend the chain this replica already holds.
var ErrBadChain = errors.New("cosl: snapshot chain does not verify")

// Restore brings this replica up to a leader's snapshot (§4.4): the
// snapshot's chain is verified from genesis to its recorded head, this
// replica's own rows must be its prefix, and then the sqlite Online Backup
// API copies it over the live file: the one legitimate rewrite, below the
// append-only triggers, which are then reinstalled since the copy brought
// the snapshot's. A snapshot no further along than this replica changes
// nothing. On any error before the copy the live replica is untouched.
func (s *Store) Restore(snapshotPath string) error {
	uri := "file:" + snapshotPath + "?mode=ro"
	snap, err := sql.Open("sqlite", uri)
	if err != nil {
		return fmt.Errorf("cosl: restore: %w", err)
	}
	defer snap.Close()

	theirs, err := verifyChain(snap)
	if err != nil {
		return err
	}
	ours, err := readMeta(s.db)
	if err != nil {
		return err
	}
	if theirs.LastAppliedSeq <= ours.LastAppliedSeq {
		return nil
	}
	if err := s.isPrefixOf(snap); err != nil {
		return err
	}

	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("cosl: restore: %w", err)
	}
	defer conn.Close()
	err = conn.Raw(func(dc any) error {
		c, ok := dc.(interface {
			NewRestore(srcURI string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("driver %T has no online backup", dc)
		}
		b, err := c.NewRestore(uri)
		if err != nil {
			return err
		}
		if _, err := b.Step(-1); err != nil {
			b.Finish()
			return err
		}
		return b.Finish()
	})
	if err != nil {
		return fmt.Errorf("cosl: restore: %w", err)
	}
	s.restores.Add(1)
	defer s.announce()
	return installTriggers(s.db)
}

// verifyChain recomputes the prev_hash chain of a replica file from
// genesis and checks it against every row and the recorded head. Each
// envelope still present must hash to its envelope_hash.
func verifyChain(db *sql.DB) (Meta, error) {
	rows, err := db.Query("SELECT seq, term, envelope, envelope_hash, prev_hash FROM cosl ORDER BY seq")
	if err != nil {
		return Meta{}, fmt.Errorf("cosl: restore: %w", err)
	}
	defer rows.Close()
	var prev [32]byte
	var lastSeq uint64
	for rows.Next() {
		var seq, term uint64
		var envelope, envHash, stored []byte
		if err := rows.Scan(&seq, &term, &envelope, &envHash, &stored); err != nil {
			return Meta{}, fmt.Errorf("cosl: restore: %w", err)
		}
		var eh [32]byte
		copy(eh[:], envHash)
		// A pruned row has no envelope left to check; its hash still chains.
		if envelope != nil && sha256.Sum256(envelope) != eh {
			return Meta{}, fmt.Errorf("%w: row %d envelope does not match its hash", ErrBadChain, seq)
		}
		h := chainHash(prev, seq, term, eh)
		if string(h[:]) != string(stored) {
			return Meta{}, fmt.Errorf("%w: row %d", ErrBadChain, seq)
		}
		prev, lastSeq = h, seq
	}
	if err := rows.Err(); err != nil {
		return Meta{}, fmt.Errorf("cosl: restore: %w", err)
	}
	meta, err := readMeta(db)
	if err != nil {
		return Meta{}, err
	}
	if meta.ChainHead != prev {
		return Meta{}, fmt.Errorf("%w: recorded head is not the chain's", ErrBadChain)
	}
	if meta.LastAppliedSeq < lastSeq {
		return Meta{}, fmt.Errorf("%w: last_applied %d is behind row %d", ErrBadChain, meta.LastAppliedSeq, lastSeq)
	}
	return meta, nil
}

// isPrefixOf checks that this replica's last row sits in snap with the
// same prev_hash; identical hashes there mean identical history before.
func (s *Store) isPrefixOf(snap *sql.DB) error {
	var seq uint64
	var head []byte
	err := s.db.QueryRow("SELECT seq, prev_hash FROM cosl ORDER BY seq DESC LIMIT 1").Scan(&seq, &head)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cosl: restore: %w", err)
	}
	var theirs []byte
	err = snap.QueryRow("SELECT prev_hash FROM cosl WHERE seq = ?", seq).Scan(&theirs)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && string(theirs) != string(head)) {
		return fmt.Errorf("%w: this replica's row %d is not in the snapshot's history", ErrBadChain, seq)
	}
	if err != nil {
		return fmt.Errorf("cosl: restore: %w", err)
	}
	return nil
}
