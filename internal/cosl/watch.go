package cosl

import (
	"context"
	"time"
)

// Changed returns a channel that is closed at the next change to the
// replica: a row the FSM commits, or a Restore that rewrites it (ADR 0014).
// Take it before reading, so a change landing mid-read is not missed. Any
// number of readers may wait on it at no cost. Pruning does not announce:
// it only destroys payloads, which no reader reacts to.
func (s *Store) Changed() <-chan struct{} {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	return s.changed
}

// announce wakes everyone waiting on Changed.
func (s *Store) announce() {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	close(s.changed)
	s.changed = make(chan struct{})
}

// FallbackTick is how often a reader waiting on Changed checks the replica
// anyway, in case a wake is ever missed (ADR 0014). With nothing new, the
// check costs one cosl_meta read.
const FallbackTick = time.Minute

// Mark is how far the replica has moved: its last applied seq and how many
// restores have rewritten it since Open. While neither moves, nothing a
// reader folds can have changed.
type Mark struct {
	LastAppliedSeq uint64
	Restores       uint64
}

// Mark reads the replica's Mark: one cosl_meta row.
func (s *Store) Mark() (Mark, error) {
	// Restores first: a restore landing between the two reads then shows
	// as a seq that moved, and the next Mark sees the restore too.
	restores := s.restores.Load()
	var seq []byte
	if err := scanMeta(s.db, metaLastAppliedSeq, &seq); err != nil {
		return Mark{}, err
	}
	return Mark{LastAppliedSeq: beUint64(seq), Restores: restores}, nil
}

// Follower paces one long-running reader of the replica: it waits for a
// change or its fallback tick, and hands the reader a Wake only when the
// replica moved since the reader last acted. Not safe for concurrent use.
type Follower struct {
	store    *Store
	fallback time.Duration
	changed  <-chan struct{} // taken before the last Poll's read
	acted    Mark            // as of the last Done
	hasActed bool
	pending  Mark // the last Wake handed out
}

// Wake is what a reader has to catch up on.
type Wake struct {
	// Mark is the replica's as this Wake was handed out.
	Mark Mark
	// Rescan means read everything: this is the reader's first Wake, or a
	// restore has rewritten the replica since it last acted.
	Rescan bool
	// Since is the seq the reader last acted on; unless Rescan, only rows
	// after it (Query.AfterSeq) are new.
	Since uint64
}

// Follow paces a reader of s, checking at least every fallback.
func (s *Store) Follow(fallback time.Duration) *Follower {
	return &Follower{store: s, fallback: fallback}
}

// Poll reads the replica's Mark and reports a Wake if it moved since the
// reader last called Done, or if the reader never has. It does not block.
func (f *Follower) Poll() (Wake, bool, error) {
	f.changed = f.store.Changed()
	m, err := f.store.Mark()
	if err != nil {
		return Wake{}, false, err
	}
	if f.hasActed && m == f.acted {
		return Wake{}, false, nil
	}
	f.pending = m
	return Wake{
		Mark:   m,
		Rescan: !f.hasActed || m.Restores != f.acted.Restores,
		Since:  f.acted.LastAppliedSeq,
	}, true, nil
}

// Done records that the reader has acted on the last Wake. A Wake it does
// not finish, say after an error, is handed out again by the next Poll.
func (f *Follower) Done() { f.acted, f.hasActed = f.pending, true }

// Changes is closed at the first change since the last Poll. A reader that
// waits on other things too (nudges, deadlines) selects on it and on its
// own fallback timer in place of Wait.
func (f *Follower) Changes() <-chan struct{} {
	if f.changed == nil {
		f.changed = f.store.Changed()
	}
	return f.changed
}

// Wait blocks until the replica changes after the last Poll or the
// fallback tick falls due. It reports false once ctx ends.
func (f *Follower) Wait(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	t := time.NewTimer(f.fallback)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-f.Changes():
		return true
	case <-t.C:
		return true
	}
}
