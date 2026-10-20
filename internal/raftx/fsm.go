package raftx

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/hashicorp/raft"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
)

// fsm adapts cosl.FSM to raft.FSM: only LogCommand entries reach the
// COSL, with the raft index and term as seq and term (§4.3 step 5).
type fsm struct {
	inner *cosl.FSM
	store *cosl.Store
	dir   string // group/<gid>/, where snapshot staging files live
}

func newFSM(store *cosl.Store, roster group.Roster, dir string) *fsm {
	return &fsm{inner: cosl.NewFSM(store, roster), store: store, dir: dir}
}

// applied is Apply's response to the leader's ApplyFuture.
type applied struct {
	index, term uint64
	result      cosl.Result
}

func (f *fsm) Apply(l *raft.Log) any {
	if l.Type != raft.LogCommand {
		return nil
	}
	res, err := f.inner.Apply(l.Index, l.Term, l.Data)
	if err != nil {
		return err
	}
	return applied{index: l.Index, term: l.Term, result: res}
}

// Snapshot is a consistent copy of the replica file, staged beside it.
// Raft only asks once SnapshotThreshold entries have accumulated (§4.2).
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	path, err := f.staging()
	if err != nil {
		return nil, err
	}
	if err := f.store.VacuumInto(path); err != nil {
		os.Remove(path)
		return nil, err
	}
	return &fileSnapshot{path: path}, nil
}

// ErrRestoreUnsupported: installing another Node's snapshot over this
// replica is the catch-up ticket's (§4.4); until then a Node that has
// fallen behind the retained log must keep its replica.
var ErrRestoreUnsupported = errors.New("raftx: snapshot restore is not implemented yet (catch-up ticket)")

// Restore is called by NewRaft for the latest local snapshot and by
// InstallSnapshot from the leader. The replica file is durable on its
// own, so a snapshot this Node has already applied past is a no-op;
// anything further ahead is the unsupported case.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	path, err := f.staging()
	if err != nil {
		return err
	}
	defer os.Remove(path)
	if err := writeFile(path, rc); err != nil {
		return fmt.Errorf("raftx: restore: %w", err)
	}
	snap, err := cosl.Open(path)
	if err != nil {
		return fmt.Errorf("raftx: restore: %w", err)
	}
	defer snap.Close()
	theirs, err := snap.Meta()
	if err != nil {
		return fmt.Errorf("raftx: restore: %w", err)
	}
	ours, err := f.store.Meta()
	if err != nil {
		return fmt.Errorf("raftx: restore: %w", err)
	}
	if ours.LastAppliedSeq >= theirs.LastAppliedSeq {
		return nil
	}
	return fmt.Errorf("%w: snapshot at seq %d, replica at seq %d", ErrRestoreUnsupported, theirs.LastAppliedSeq, ours.LastAppliedSeq)
}

// staging reserves a fresh file name under dir for a snapshot copy. The
// file is removed because VACUUM INTO refuses to overwrite.
func (f *fsm) staging() (string, error) {
	tmp, err := os.CreateTemp(f.dir, "cosl-snap-*.sqlite")
	if err != nil {
		return "", fmt.Errorf("raftx: snapshot: %w", err)
	}
	tmp.Close()
	os.Remove(tmp.Name())
	return tmp.Name(), nil
}

func writeFile(path string, r io.Reader) error {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// fileSnapshot streams a VACUUM INTO copy to the snapshot sink.
type fileSnapshot struct {
	path string
}

func (s *fileSnapshot) Persist(sink raft.SnapshotSink) error {
	f, err := os.Open(s.path)
	if err != nil {
		sink.Cancel()
		return fmt.Errorf("raftx: snapshot: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(sink, f); err != nil {
		sink.Cancel()
		return fmt.Errorf("raftx: snapshot: %w", err)
	}
	return sink.Close()
}

func (s *fileSnapshot) Release() { os.Remove(s.path) }
