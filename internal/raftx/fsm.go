package raftx

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/hashicorp/raft"

	"defios.com/internal/cosl"
)

// fsm adapts cosl.FSM to raft.FSM: only LogCommand entries reach the
// COSL, with the raft index and term as seq and term (§4.3 step 5).
type fsm struct {
	inner *cosl.FSM
	store *cosl.Store
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

// Snapshot is a consistent copy of the replica file. Raft only asks once
// SnapshotThreshold entries have accumulated (§4.2).
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	tmp, err := os.CreateTemp("", "defios-cosl-snap-*.sqlite")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	os.Remove(tmp.Name()) // VACUUM INTO refuses to overwrite
	if err := f.store.VacuumInto(tmp.Name()); err != nil {
		return nil, err
	}
	return &fileSnapshot{path: tmp.Name()}, nil
}

// ErrRestoreUnsupported: InstallSnapshot restore is the catch-up ticket's
// (§4.4); until then a Node that has fallen behind the retained log must
// keep its replica.
var ErrRestoreUnsupported = errors.New("raftx: snapshot restore is not implemented yet (catch-up ticket)")

func (f *fsm) Restore(rc io.ReadCloser) error {
	rc.Close()
	return ErrRestoreUnsupported
}

// fileSnapshot streams a VACUUM INTO copy to the snapshot sink.
type fileSnapshot struct {
	path string
}

func (s *fileSnapshot) Persist(sink raft.SnapshotSink) error {
	f, err := os.Open(s.path)
	if err != nil {
		sink.Cancel()
		return err
	}
	defer f.Close()
	if _, err := io.Copy(sink, f); err != nil {
		sink.Cancel()
		return fmt.Errorf("raftx: snapshot: %w", err)
	}
	return sink.Close()
}

func (s *fileSnapshot) Release() { os.Remove(s.path) }
