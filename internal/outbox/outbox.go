// Package outbox holds what this Node writes while no leader is known, and
// commits it, in the order it was written, once one is
// (docs/adr/0025-an-outbox-holds-what-a-node-writes-without-a-quorum.md).
// One Outbox serves one Group; it lives beside the replica, outside Raft.
package outbox

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/flock"
	"defios.com/internal/raftx"
	"defios.com/internal/signreq"
	coslv1 "defios.com/proto/cosl/v1"
)

// Config opens one Group's Outbox.
type Config struct {
	// Path is the file the Outbox persists to.
	Path string
	// Append commits one envelope through Raft, bounded as the caller sees
	// fit; raftx.ErrNoLeader means no quorum.
	Append func(context.Context, cosl.Signed) (raftx.Position, error)
	// Applied is the seq the replica has applied up to: what this Node
	// committed at or below it is no longer remembered for Unapplied. Nil
	// is 0.
	Applied func() uint64
	// Logf reports what the drain did with each envelope; nil is silent.
	Logf func(format string, args ...any)
	// Now is the clock a Sign Request's deadline is read against; nil is
	// time.Now.
	Now func() time.Time
	// Changed, if set, hears what is queued, oldest first, each time that
	// changes. It is called under the Outbox's lock, so it must not call
	// the Outbox.
	Changed func(queued []cosl.Signed)
}

// Result is what a Write did: committed at Position, or Queued.
type Result struct {
	Position raftx.Position
	Queued   bool
}

// Outbox is this Node's queued envelopes for one Group, oldest first.
type Outbox struct {
	cfg Config

	sendMu  sync.Mutex // one Write's direct append at a time, so none overtakes a queued one
	drainMu sync.Mutex // one envelope's commit by the drain, or one Drop, at a time

	mu      sync.Mutex
	entries []cosl.Signed
	sent    []Entry // committed, perhaps not yet applied by the replica, by seq
	// inFlight are queued envelopes an append of which ended with no clear
	// outcome: Raft may still commit them, so Drop refuses them until the
	// drain settles each one.
	inFlight map[[32]byte]bool

	queued chan struct{} // signalled by each push, for Run
}

// Open reads the Outbox at cfg.Path, empty if there is no file yet.
func Open(cfg Config) (*Outbox, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Applied == nil {
		cfg.Applied = func() uint64 { return 0 }
	}
	entries, err := Read(cfg.Path)
	if err != nil {
		return nil, err
	}
	return &Outbox{cfg: cfg, entries: entries, inFlight: map[[32]byte]bool{}, queued: make(chan struct{}, 1)}, nil
}

// Read is the envelopes queued in the Outbox at path, oldest first; none if
// there is no file yet. It is for a reader with no serve: a running serve's
// Outbox may change at any time.
func Read(path string) ([]cosl.Signed, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outbox: %w", err)
	}
	var file fileV1
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("outbox: %s: %w", path, err)
	}
	var out []cosl.Signed
	for _, e := range file.Entries {
		env, err := cosl.Unmarshal(e)
		if err != nil || len(env.MsgId) != 32 {
			return nil, fmt.Errorf("outbox: %s holds an envelope that does not parse", path)
		}
		out = append(out, cosl.Signed{MsgID: [32]byte(env.MsgId), Bytes: e})
	}
	return out, nil
}

// Lock takes the lock on the Outbox at path, which whoever writes that
// Outbox holds: a running serve's replica for as long as it runs, or
// `outbox drop` while it edits the file. With wait it blocks until the lock
// is free; without, a held lock is ErrBusy. unlock releases it.
func Lock(path string, wait bool) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("outbox: %w", err)
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("outbox: %w", err)
	}
	if err := flock.Lock(f, true, wait); err != nil {
		f.Close()
		if errors.Is(err, flock.ErrWouldBlock) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("outbox: locking %s: %w", path, err)
	}
	return func() { f.Close() }, nil // closing the file releases the lock
}

// fileV1 is the Outbox on disk: the signed envelopes in queue order.
type fileV1 struct {
	Entries [][]byte `json:"entries"`
}

// Write commits s, or queues it when no leader is known. While the Outbox
// holds anything, s joins its back, leader or not.
func (o *Outbox) Write(ctx context.Context, s cosl.Signed) (Result, error) {
	if !Queueable(s) {
		pos, err := o.cfg.Append(ctx, s)
		return Result{Position: pos}, err
	}
	o.sendMu.Lock()
	defer o.sendMu.Unlock()
	if len(o.Entries()) > 0 {
		if err := o.push(s); err != nil {
			return Result{}, err
		}
		return Result{Queued: true}, nil
	}
	pos, err := o.cfg.Append(ctx, s)
	if errors.Is(err, raftx.ErrNoLeader) {
		if err := o.push(s); err != nil {
			return Result{}, err
		}
		return Result{Queued: true}, nil
	}
	if err != nil {
		return Result{}, err
	}
	o.mu.Lock()
	o.remember(s, pos.Index)
	o.mu.Unlock()
	return Result{Position: pos}, nil
}

// Run drains the Outbox now, again whenever a leader becomes known or a
// message is queued, and on the fallback tick (ADR 0014) in case a wake is
// missed, until ctx ends.
func (o *Outbox) Run(ctx context.Context, leaderKnown <-chan struct{}) {
	for {
		o.Drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-leaderKnown:
		case <-o.queued:
		case <-time.After(cosl.FallbackTick):
		}
	}
}

// Queueable reports whether s may wait in the Outbox: talk and the Sign
// Request lifecycle may, Session traffic may not, whatever chains a Sign
// Request's transactions are on.
func Queueable(s cosl.Signed) bool {
	env, err := cosl.Unmarshal(s.Bytes)
	if err != nil {
		return false
	}
	switch env.MsgType {
	case cosl.TypeAppFreeform, cosl.TypeSignRequest, cosl.TypeSignAccept, cosl.TypeSignRefuse, cosl.TypeSignBroadcast:
		return true
	}
	return false
}

// signRequest is env's payload as a sign.request.
func signRequest(env *coslv1.Envelope) (*coslv1.SignRequest, error) {
	msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
	if err != nil {
		return nil, err
	}
	return msg.(*coslv1.SignRequest), nil
}

// Drain commits the queued envelopes, oldest first, until the Outbox is
// empty or one cannot be committed now. An envelope the FSM rejects, and a
// sign.request past its deadline, are logged and dropped, and the drain
// goes on.
func (o *Outbox) Drain(ctx context.Context) {
	for o.drainHead(ctx) {
	}
}

// drainHead commits, or drops, the oldest queued envelope, and reports
// whether the drain should go on. A Drop waits for it.
func (o *Outbox) drainHead(ctx context.Context) bool {
	o.drainMu.Lock()
	defer o.drainMu.Unlock()
	s, ok := o.head()
	if !ok {
		return false
	}
	if d, ok := o.deadline(s); ok && o.cfg.Now().After(d) {
		o.cfg.Logf("outbox: sign.request %x expired unsent: its deadline passed at %s", s.MsgID[:8], d.Format(time.RFC3339))
		if err := o.remove(s, 0); err != nil {
			o.cfg.Logf("outbox: %v", err)
			return false
		}
		return true
	}
	pos, err := o.cfg.Append(ctx, s)
	var seq uint64 // what it committed at, 0 if it did not
	switch {
	case err == nil:
		seq = pos.Index
		o.cfg.Logf("outbox: %x committed at seq %d", s.MsgID[:8], pos.Index)
	case errors.Is(err, raftx.ErrNotInserted):
		o.cfg.Logf("outbox: %x rejected by the log, dropped: %v", s.MsgID[:8], err)
	case errors.Is(err, raftx.ErrNoLeader) && !errors.Is(err, raftx.ErrMaybeCommitted):
		return false
	case errors.Is(err, raftx.ErrNoLeader), errors.Is(err, raftx.ErrClosed), ctx.Err() != nil:
		o.setInFlight(s.MsgID)
		return false
	default:
		o.setInFlight(s.MsgID)
		o.cfg.Logf("outbox: %x not committed, will retry: %v", s.MsgID[:8], err)
		return false
	}
	if err := o.remove(s, seq); err != nil {
		o.cfg.Logf("outbox: %v", err)
		return false
	}
	return true
}

// setInFlight marks the queued envelope with msgID as perhaps committing.
func (o *Outbox) setInFlight(msgID [32]byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.inFlight[msgID] = true
}

var (
	// ErrBadMsgID means a msg_id prefix is not 1 to 64 hex digits.
	ErrBadMsgID = errors.New("outbox: a msg_id prefix is 1 to 64 hex digits")
	// ErrNotQueued means no queued message's msg_id starts with the prefix.
	ErrNotQueued = errors.New("outbox: no queued message has that msg_id")
	// ErrAmbiguous means more than one queued message's msg_id starts with
	// the prefix.
	ErrAmbiguous = errors.New("outbox: that msg_id prefix names more than one queued message")
	// ErrCommitted means the message is on the log: it can no longer be
	// dropped.
	ErrCommitted = errors.New("outbox: that message has committed")
	// ErrInFlight means an attempt to commit the message ended with no
	// clear outcome, so it may yet commit: it cannot be dropped until the
	// drain's next attempt settles it.
	ErrInFlight = errors.New("outbox: that message may be committing; the drain's next attempt settles it, try again after")
	// ErrBusy means another process holds the Outbox's lock: a running
	// serve's replica, which owns the Outbox.
	ErrBusy = errors.New("outbox: another process holds the Outbox")
)

// CommittedError is ErrCommitted for the message with MsgID, committed at
// Seq.
type CommittedError struct {
	MsgID [32]byte
	Seq   uint64
}

func (e *CommittedError) Error() string {
	return fmt.Sprintf("outbox: message %x committed at seq %d; it is on the log and cannot be dropped", e.MsgID[:8], e.Seq)
}

func (e *CommittedError) Unwrap() error { return ErrCommitted }

// MsgIDPrefix is prefix, a hex msg_id prefix, in lower case, or
// ErrBadMsgID.
func MsgIDPrefix(prefix string) (string, error) {
	p := strings.ToLower(prefix)
	if p == "" || len(p) > 64 || strings.Trim(p, "0123456789abcdef") != "" {
		return "", fmt.Errorf("%w: %q", ErrBadMsgID, prefix)
	}
	return p, nil
}

// Drop erases the queued envelope whose hex msg_id starts with prefix, and
// nothing else (ADR 0025). It was never on the log, so this is a real
// delete. If the drain is committing it, Drop waits, and then reports it
// committed: a message is never both dropped and committed. One this
// Outbox committed and the replica has not applied yet is a
// CommittedError; one the replica has applied is ErrNotQueued here, and the
// caller looks for it on the log.
func (o *Outbox) Drop(prefix string) (cosl.Signed, error) {
	p, err := MsgIDPrefix(prefix)
	if err != nil {
		return cosl.Signed{}, err
	}
	return o.drop(p)
}

// drop is Drop of p, a MsgIDPrefix.
func (o *Outbox) drop(p string) (cosl.Signed, error) {
	o.drainMu.Lock()
	defer o.drainMu.Unlock()
	o.mu.Lock()
	var matches []cosl.Signed
	for _, s := range o.entries {
		if strings.HasPrefix(hex.EncodeToString(s.MsgID[:]), p) {
			matches = append(matches, s)
		}
	}
	o.forget()
	sent := slices.Clone(o.sent)
	inFlight := len(matches) == 1 && o.inFlight[matches[0].MsgID]
	o.mu.Unlock()
	switch len(matches) {
	case 0:
		for _, e := range sent {
			if strings.HasPrefix(hex.EncodeToString(e.MsgID[:]), p) {
				return cosl.Signed{}, &CommittedError{MsgID: e.MsgID, Seq: e.Seq}
			}
		}
		return cosl.Signed{}, fmt.Errorf("%w: none starts with %s", ErrNotQueued, p)
	case 1:
		if inFlight {
			return cosl.Signed{}, fmt.Errorf("%w: %x", ErrInFlight, matches[0].MsgID[:8])
		}
		if err := o.remove(matches[0], 0); err != nil {
			return cosl.Signed{}, err
		}
		return matches[0], nil
	}
	return cosl.Signed{}, fmt.Errorf("%w: %s matches %d", ErrAmbiguous, p, len(matches))
}

// Withdraw is `outbox drop`: o.Drop, and when nothing queued or remembered
// matches, a CommittedError if the replica store holds the message.
func Withdraw(o *Outbox, store *cosl.Store, prefix string) (cosl.Signed, error) {
	p, err := MsgIDPrefix(prefix)
	if err != nil {
		return cosl.Signed{}, err
	}
	s, err := o.drop(p)
	if !errors.Is(err, ErrNotQueued) {
		return s, err
	}
	if lerr := onLog(store, p); lerr != nil {
		return cosl.Signed{}, lerr
	}
	return cosl.Signed{}, err
}

// onLog is a CommittedError if the one row of store whose hex msg_id
// starts with prefix, a MsgIDPrefix, is on the log; nil if none is, or
// more than one may be.
func onLog(store *cosl.Store, prefix string) error {
	// The store matches whole bytes: look up the prefix's whole bytes, then
	// check an odd last hex digit against what that finds.
	whole, _ := hex.DecodeString(prefix[:len(prefix)&^1])
	r, err := store.ByMsgIDPrefix(whole)
	if errors.Is(err, cosl.ErrNotFound) || errors.Is(err, cosl.ErrAmbiguous) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	if !strings.HasPrefix(hex.EncodeToString(r.MsgID[:]), prefix) {
		return nil
	}
	return &CommittedError{MsgID: r.MsgID, Seq: r.Seq}
}

// deadline is when s lapses, if it is a sign.request.
func (o *Outbox) deadline(s cosl.Signed) (time.Time, bool) {
	env, err := cosl.Unmarshal(s.Bytes)
	if err != nil || env.MsgType != cosl.TypeSignRequest {
		return time.Time{}, false
	}
	req, err := signRequest(env)
	if err != nil {
		return time.Time{}, false
	}
	return signreq.Deadline(env.CreatedAt, req), true
}

// Entries are the queued envelopes, oldest first.
func (o *Outbox) Entries() []cosl.Signed {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]cosl.Signed(nil), o.entries...)
}

// head is the oldest queued envelope, if any.
func (o *Outbox) head() (cosl.Signed, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.entries) == 0 {
		return cosl.Signed{}, false
	}
	return o.entries[0], true
}

// push persists s at the back, then queues it in memory and wakes Run.
func (o *Outbox) push(s cosl.Signed) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	next := append(append([]cosl.Signed(nil), o.entries...), s)
	if err := o.save(next); err != nil {
		return err
	}
	o.entries = next
	o.changed()
	select {
	case o.queued <- struct{}{}:
	default:
	}
	return nil
}

// remove persists the Outbox without s, then drops it in memory, and
// remembers it as committed at seq unless seq is 0.
func (o *Outbox) remove(s cosl.Signed, seq uint64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	var next []cosl.Signed
	for _, e := range o.entries {
		if e.MsgID != s.MsgID {
			next = append(next, e)
		}
	}
	if err := o.save(next); err != nil {
		return err
	}
	o.entries = next
	o.changed()
	delete(o.inFlight, s.MsgID)
	if seq != 0 {
		o.remember(s, seq)
	}
	return nil
}

// changed tells Config.Changed what is queued now. o.mu is held.
func (o *Outbox) changed() {
	if o.cfg.Changed != nil {
		o.cfg.Changed(slices.Clone(o.entries))
	}
}

// Entry is one of this Node's messages its replica may not hold yet:
// committed at Seq, or queued, with Seq 0.
type Entry struct {
	cosl.Signed
	Seq uint64
}

// Unapplied is what this Node wrote that its replica may not show yet: what
// it committed past the seq the replica has applied, by seq, then what is
// queued, oldest first. A follower's commit is applied by the leader before
// it is here, so a reader that reads this before the replica, and drops
// what both hold, sees every message of this Node's (ADR 0025).
func (o *Outbox) Unapplied() []Entry {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.forget()
	out := append([]Entry(nil), o.sent...)
	for _, s := range o.entries {
		out = append(out, Entry{Signed: s})
	}
	return out
}

// remember keeps s, committed at seq, until the replica applies it. o.mu is
// held.
func (o *Outbox) remember(s cosl.Signed, seq uint64) {
	o.sent = append(o.sent, Entry{Signed: s, Seq: seq})
	slices.SortFunc(o.sent, func(a, b Entry) int { return cmp.Compare(a.Seq, b.Seq) })
	o.forget()
}

// forget drops what the replica has applied. o.mu is held.
func (o *Outbox) forget() {
	applied := o.cfg.Applied()
	o.sent = slices.DeleteFunc(o.sent, func(e Entry) bool { return e.Seq <= applied })
}

// save replaces the file with entries, atomically.
func (o *Outbox) save(entries []cosl.Signed) error {
	file := fileV1{Entries: make([][]byte, len(entries))}
	for i, e := range entries {
		file.Entries[i] = e.Bytes
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	dir := filepath.Dir(o.cfg.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".outbox.*")
	if err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	_, err = tmp.Write(append(raw, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), o.cfg.Path)
	}
	if err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync() // best effort: the rename is durable once the directory is
		d.Close()
	}
	return nil
}
