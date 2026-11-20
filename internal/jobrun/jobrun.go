// Package jobrun keeps a Group's Job Runs on the Node that ran them (ADR
// 0040): one file each in a directory of the Group's data, never
// replicated and left out of a Backup, bounded to the last Keep.
package jobrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"defios.com/internal/trigger"
)

// Keep is how many Job Runs a Group keeps; the oldest go first.
const Keep = 200

// States of a Job Run.
const (
	Pending = "pending" // waits on the Jobs before it
	Running = "running"
	OK      = "ok"
	Failed  = "failed"
	// NotRun: a Job before it failed, or serve stopped first, so it never
	// ran.
	NotRun = "not_run"
)

// ErrUnknown: no Job Run is kept by that ID.
var ErrUnknown = errors.New("jobrun: no such Job Run")

// Run is one Job Run: one Job a Trigger's firing ran, or was to run, or
// one Job run by hand.
type Run struct {
	ID uint64 `json:"id"` // the Group's count of Job Runs, from 1
	// Firing is the ID of its firing's first Job Run: the Job Runs of one
	// firing share it.
	Firing  uint64 `json:"firing"`
	Trigger string `json:"trigger"` // the Hook or Schedule that fired; "" if run by hand
	// ByHand: run by hand (job run, Run now), a firing of this one Job
	// with no Trigger, on the event given, if any.
	ByHand  bool          `json:"by_hand,omitempty"`
	Event   trigger.Event `json:"event"` // what it fired on
	Fired   time.Time     `json:"fired"`
	Job     string        `json:"job"`
	Kind    string        `json:"kind"` // agent, exec or webhook
	State   string        `json:"state"`
	Started time.Time     `json:"started,omitzero"`
	Ended   time.Time     `json:"ended,omitzero"`
	Error   string        `json:"error,omitempty"`
	// Handle is an agent Job's agent. Conversation and Chain are where it
	// answers, hex ("" for Group-level talk), and the chain it runs as,
	// once it started: the agent's Transcript marks the run with it.
	Handle       string `json:"handle,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	Chain        string `json:"chain,omitempty"`
	// Output is an exec or webhook Job's, as far as it got.
	trigger.Output
	// Messages are the hex msg_ids of the talk messages an exec Job wrote
	// with the token naming this Job Run, in the order written, queued
	// ones included. An agent Job's are found through its Chain.
	Messages []string `json:"messages,omitempty"`
	// SignRequests are the hex msg_ids of the Sign Requests an exec Job
	// proposed with that token, in the order proposed, queued ones
	// included. An agent Job's are in its run's Transcript.
	SignRequests []string `json:"sign_requests,omitempty"`
}

// Token names r to the exec Job it runs, as DEFIOS_JOB_RUN: its ID and
// when it fired, so that a token outliving its Job Run, whose ID came to
// name another, names none.
func (r Run) Token() string {
	return strconv.FormatUint(r.ID, 10) + "-" + strconv.FormatInt(r.Fired.UnixNano(), 10)
}

// TokenID is the ID in token, if it holds one. The Job Run of that ID is
// the one token names only if its Token equals token.
func TokenID(token string) (uint64, bool) {
	id, _, ok := strings.Cut(token, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(id, 10, 64)
	return n, err == nil
}

// Store writes one Group's Job Runs. Only the serve serving the Group
// writes them; anyone may read them with List and Get.
type Store struct {
	dir   string
	mu    sync.Mutex
	last  uint64 // the highest ID given
	floor uint64 // the lowest ID kept: a Job Run below it was dropped
}

// Open is the Store of the Job Runs in dir, made if missing. A Job Run
// left pending or running was cut short by the serve that wrote it
// stopping, and is ended so.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("jobrun: %w", err)
	}
	s := &Store{dir: dir}
	ids, err := listIDs(dir)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		s.last = ids[len(ids)-1]
	}
	for _, id := range ids {
		r, err := Get(dir, id)
		if err != nil {
			continue // removed meanwhile, or unreadable: nothing to end
		}
		switch r.State {
		case Pending:
			r.State = NotRun
		case Running:
			r.State, r.Error = Failed, "serve stopped before it ended"
		default:
			continue
		}
		if err := s.Put(r); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Add keeps runs, one firing's Job Runs in the order they run, giving
// each its ID and Firing, and drops the oldest past Keep.
func (s *Store) Add(runs []Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range runs {
		s.last++
		runs[i].ID, runs[i].Firing = s.last, s.last-uint64(i)
	}
	for _, r := range runs {
		if err := s.write(r); err != nil {
			return err
		}
	}
	ids, err := listIDs(s.dir)
	if err != nil {
		return err
	}
	drop := ids[:max(len(ids)-Keep, 0)]
	for _, id := range drop {
		if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("jobrun: %w", err)
		}
	}
	if len(drop) > 0 {
		s.floor = drop[len(drop)-1] + 1
	}
	return nil
}

// Put keeps r, one Add gave its ID, as it now is: unless it was dropped
// meanwhile, as its Job ran on.
func (s *Store) Put(r Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID < s.floor {
		return nil
	}
	return s.write(r)
}

// write replaces r's file whole, so a reader never sees half of it.
func (s *Store) write(r Run) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("jobrun: %w", err)
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), s.path(r.ID))
	}
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("jobrun: %w", err)
	}
	return nil
}

func (s *Store) path(id uint64) string { return filepath.Join(s.dir, fileName(id)) }

func fileName(id uint64) string { return strconv.FormatUint(id, 10) + ".json" }

// List is the Job Runs kept in dir, newest first; none if dir is missing.
func List(dir string) ([]Run, error) {
	ids, err := listIDs(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(ids))
	for _, id := range slices.Backward(ids) {
		r, err := Get(dir, id)
		if errors.Is(err, ErrUnknown) {
			continue // dropped since it was listed
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Get is the Job Run kept in dir by id.
func Get(dir string, id uint64) (Run, error) {
	b, err := os.ReadFile(filepath.Join(dir, fileName(id)))
	if errors.Is(err, os.ErrNotExist) {
		return Run{}, fmt.Errorf("%w: %d", ErrUnknown, id)
	}
	if err != nil {
		return Run{}, fmt.Errorf("jobrun: %w", err)
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return Run{}, fmt.Errorf("jobrun: Job Run %d: %w", id, err)
	}
	return r, nil
}

// listIDs is the IDs of the Job Runs in dir, oldest first.
func listIDs(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jobrun: %w", err)
	}
	var ids []uint64
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if id, err := strconv.ParseUint(name, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}
