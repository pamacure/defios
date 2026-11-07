package serve

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/errlog"
	"defios.com/internal/key"
	"defios.com/internal/raftx"
	"defios.com/internal/secret"
	"defios.com/internal/signreq"
	"defios.com/internal/trigger"
	coslv1 "defios.com/proto/cosl/v1"
)

// keepHooks fires this Node's Hooks (ADR 0032) on what the replica applies
// live, until ctx ends. Live is after the startup catch-up: it waits until
// the replica has caught up with the leader, takes the log and the Sign
// Requests' statuses as they then are as its baseline, and fires on every
// row applied after it and every status change it sees, including a
// request's deadline passing. A snapshot restore is a new baseline and
// fires nothing. config.toml's Jobs and Hooks are reread whenever the file
// changes, as `prune` is.
func (s *server) keepHooks(ctx context.Context, replica *raftx.Replica, store *cosl.Store) {
	h := s.newHookLoop(ctx, store) // notes config.toml's mtime now, as prune does
	defer h.queues.wait()
	caughtUp, ok := s.caughtUp(ctx, replica)
	if !ok {
		return
	}
	h.errs.Report(h.baseline(caughtUp))
	s.opts.Logf("serve: hooks: live after seq %d, the startup catch-up", caughtUp)
	f := store.Follow(cosl.FallbackTick)
	tick := time.NewTicker(cosl.FallbackTick)
	defer tick.Stop()
	expiry := time.NewTimer(time.Hour)
	expiry.Stop() // armed once the baseline knows the deadlines
	defer expiry.Stop()
	for {
		h.reread()
		h.errs.Report(f.Act(h.wake))
		h.armExpiry(expiry)
		select {
		case <-ctx.Done():
			return
		case <-f.Changes():
		case <-expiry.C:
			if h.baselined {
				h.errs.Report(h.expire())
			}
		case <-tick.C:
		}
	}
}

// caughtUp waits until replica has applied what the leader had when asked,
// retrying while no leader is known, and returns the seq it caught up to;
// false if ctx ends first.
func (s *server) caughtUp(ctx context.Context, replica *raftx.Replica) (uint64, bool) {
	for {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		seq, err := replica.CaughtUp(attempt)
		cancel()
		switch {
		case err == nil:
			return seq, true
		case ctx.Err() != nil, errors.Is(err, raftx.ErrClosed):
			return 0, false
		}
		select {
		case <-ctx.Done():
			return 0, false
		case <-time.After(joinRetry):
		}
	}
}

// hookLoop is keepHooks' state between wakes.
type hookLoop struct {
	*triggers
	store     *cosl.Store
	baselined bool
	restores  uint64            // the Store's restore count as of the baseline
	seen      uint64            // the last seq handled: statuses are read as of it
	statuses  map[string]string // each Sign Request's, by hex msg_id, as of seen
	deadline  time.Time         // the next open request's deadline; zero for none
	errs      *errlog.Reporter
}

// newHookLoop starts from the config serve read as it started.
func (s *server) newHookLoop(ctx context.Context, store *cosl.Store) *hookLoop {
	return &hookLoop{
		triggers: s.newTriggers(ctx, store, "hook"),
		store:    store,
		errs:     errlog.New(s.opts.Logf, "serve: hooks"),
	}
}

// triggers is what a loop of Triggers (Hooks or Schedules) keeps: the
// Group config in force, and how the Jobs its firings run are run.
type triggers struct {
	s          *server
	ctx        context.Context
	kind       string        // "hook" or "schedule", as the log names one
	cfg        config.Config // the Jobs, Triggers and max_depth in force
	mtime      time.Time     // config.toml's as last read; zero while it is missing
	queues     *firingQueues
	runner     trigger.Runner
	configErrs *errlog.Reporter
}

// newTriggers starts from the config serve read as it started.
func (s *server) newTriggers(ctx context.Context, store *cosl.Store, kind string) *triggers {
	o := s.opts
	t := &triggers{
		s: s, ctx: ctx, kind: kind, cfg: o.Config,
		queues:     &firingQueues{},
		configErrs: errlog.New(o.Logf, "serve: "+kind+"s: reading config.toml"),
		runner: trigger.Runner{
			Group: o.Alias,
			Dir:   o.Paths.GroupConfigDir(o.Alias),
			Env: map[string]string{
				"DEFIOS_CONFIG_DIR": o.Paths.Config,
				"DEFIOS_DATA_DIR":   o.Paths.Data,
				"DEFIOS_SOCKET":     o.Paths.Socket,
			},
			Secrets: func() *secret.Resolver {
				return &secret.Resolver{Group: o.Alias, Getenv: o.Getenv, Vault: o.Secrets}
			},
			Getenv: o.Getenv,
			Logf:   o.Logf,
			Agent: func(ctx context.Context, name string, j config.Job, ev trigger.Event) error {
				return s.runAgentJob(ctx, store, name, j, ev)
			},
		},
	}
	if info, err := os.Stat(o.Paths.GroupConfigFile(o.Alias)); err == nil {
		t.mtime = info.ModTime()
	}
	return t
}

// reread takes up config.toml's Jobs, Triggers and max_depth if the file's
// modification time changed since it was last read. A file that does not
// parse is reported, and the last good config stays in force; a missing
// file has none.
func (t *triggers) reread() {
	path := t.s.opts.Paths.GroupConfigFile(t.s.opts.Alias)
	var mtime time.Time
	info, err := os.Stat(path)
	switch {
	case err == nil:
		mtime = info.ModTime()
	case !errors.Is(err, fs.ErrNotExist):
		t.configErrs.Report(err)
		return
	}
	if mtime.Equal(t.mtime) {
		return
	}
	t.mtime = mtime
	e, err := config.Load(t.s.opts.Paths, t.s.opts.Alias, nil, nil)
	t.configErrs.Report(err)
	if err == nil {
		t.cfg = e.Group
	}
}

// queue runs a firing of Trigger name on ev behind its earlier firings:
// jobs in order, as defs defines them, until one fails. cause is what the
// log says it fired on.
func (t *triggers) queue(name, cause string, jobs []string, defs map[string]config.Job, ev trigger.Event) {
	t.queues.add(name, func() { t.run(name, cause, jobs, defs, ev) })
}

func (t *triggers) run(name, cause string, jobs []string, defs map[string]config.Job, ev trigger.Event) {
	logf := t.s.opts.Logf
	prefix := "serve: " + t.kind + " " + name + ": "
	logf("%sfired %s", prefix, cause)
	r := t.runner
	r.Logf = func(format string, args ...any) { logf(prefix+format, args...) }
	for _, job := range jobs {
		if t.ctx.Err() != nil {
			return
		}
		if err := r.Run(t.ctx, job, defs[job], ev); err != nil {
			logf("%sjob %s failed: %v", prefix, job, err)
			return
		}
		logf("%sjob %s done", prefix, job)
	}
}

// baseline takes the log up to seq as the startup backlog, which fires
// nothing: the statuses as of it are where changes are seen from.
func (h *hookLoop) baseline(seq uint64) error {
	restores := h.store.Restores() // first: a restore landing mid-read rebaselines
	st, err := h.readStatuses(seq)
	if err != nil {
		return err
	}
	_, cur := trigger.StatusChanges(nil, st.list)
	h.commit(seq, cur, st.deadline)
	h.restores, h.baselined = restores, true
	return nil
}

// wake handles one move of the replica, up to the Wake's mark: a new
// baseline after a restore, else the rows applied since the last seq
// handled, then the status changes they made. Nothing fires unless
// everything reads, so a Wake handed out again fires nothing twice.
func (h *hookLoop) wake(wk cosl.Wake) error {
	upTo := wk.Mark.LastAppliedSeq
	if !h.baselined || wk.Mark.Restores != h.restores {
		return h.baseline(upTo)
	}
	if upTo <= h.seen {
		return nil
	}
	rows, err := h.store.Log(cosl.Query{AfterSeq: h.seen})
	if err != nil {
		return err
	}
	names := hookConversations(h.store)
	var evs []trigger.Event
	signs := false
	for _, r := range rows {
		if r.Seq > upTo {
			break // applied after this Wake: the next one's
		}
		signs = signs || slices.Contains(signreq.MsgTypes, r.MsgType)
		if ev, ok := rowEvent(r); ok {
			ev.Conversation = names(r.ConversationID)
			evs = append(evs, ev)
		}
	}
	var st statusRead
	if signs {
		if st, err = h.readStatuses(upTo); err != nil {
			return err
		}
	}
	depths := requestDepths(st.list)
	for _, ev := range evs {
		if d, ok := depths[ev.SignRequestID]; ok {
			ev.Depth = d // a sign.* event has its request's depth
		}
		h.fire(ev)
	}
	if !signs {
		h.seen = upTo
		return nil
	}
	h.commit(upTo, h.fireStatusChanges(names, st), st.deadline)
	return nil
}

// requestDepths is each Sign Request's depth in list, by msg id.
func requestDepths(list []signreq.Summary) map[string]uint32 {
	out := make(map[string]uint32, len(list))
	for _, s := range list {
		out[s.MsgID] = s.Depth
	}
	return out
}

// expire fires the status changes a passing deadline made, as of the last
// seq handled.
func (h *hookLoop) expire() error {
	st, err := h.readStatuses(h.seen)
	if err != nil {
		return err
	}
	h.commit(h.seen, h.fireStatusChanges(hookConversations(h.store), st), st.deadline)
	return nil
}

// statusRead is every Sign Request as of one seq, and the next deadline
// of one still open.
type statusRead struct {
	list     []signreq.Summary
	deadline time.Time
}

// readStatuses reads the Sign Requests' statuses from the rows at or below
// seq upTo, as of now.
func (h *hookLoop) readStatuses(upTo uint64) (statusRead, error) {
	rows, err := h.store.Log(cosl.Query{MsgTypes: signreq.MsgTypes})
	if err != nil {
		return statusRead{}, err
	}
	if i := slices.IndexFunc(rows, func(r cosl.Row) bool { return r.Seq > upTo }); i >= 0 {
		rows = rows[:i]
	}
	list, err := signreq.List(rows, time.Now())
	if err != nil {
		return statusRead{}, err
	}
	return statusRead{list: list, deadline: nextDeadline(rows, list)}, nil
}

// fireStatusChanges fires a request.<status> event for each request in st
// whose status differs from the last committed, and returns the statuses
// st holds.
func (h *hookLoop) fireStatusChanges(names func([]byte) string, st statusRead) map[string]string {
	evs, cur := trigger.StatusChanges(h.statuses, st.list)
	for _, ev := range evs {
		conv, _ := hex.DecodeString(ev.ConversationID)
		if len(conv) == 0 {
			conv = nil
		}
		ev.Conversation = names(conv)
		h.fire(ev)
	}
	return cur
}

// commit records the statuses as of seq, and the next open deadline.
func (h *hookLoop) commit(seq uint64, statuses map[string]string, deadline time.Time) {
	h.seen, h.statuses, h.deadline = seq, statuses, deadline
}

// armExpiry sets t to the next open request's deadline, if any.
func (h *hookLoop) armExpiry(t *time.Timer) {
	t.Stop() // since Go 1.23, a stopped timer delivers nothing stale
	if h.deadline.IsZero() {
		return
	}
	// A moment past it, so the status has turned when it is read.
	t.Reset(max(time.Until(h.deadline), 0) + 10*time.Millisecond)
}

// nextDeadline is the earliest deadline of a request in list still open or
// waiting, whose status its passing changes; zero for none.
func nextDeadline(rows []cosl.Row, list []signreq.Summary) time.Time {
	open := map[string]bool{}
	for _, s := range list {
		if s.Status == signreq.Open || s.Status == signreq.Waiting {
			open[s.MsgID] = true
		}
	}
	var next time.Time
	for _, r := range rows {
		if r.MsgType != cosl.TypeSignRequest || !open[hex.EncodeToString(r.MsgID[:])] {
			continue
		}
		msg, err := payload(r)
		if err != nil {
			continue
		}
		d := signreq.Deadline(r.CreatedAt, msg.(*coslv1.SignRequest))
		if next.IsZero() || d.Before(next) {
			next = d
		}
	}
	return next
}

// fire queues ev for each Hook it fires, with the Jobs as config.toml now
// defines them.
func (h *hookLoop) fire(ev trigger.Event) {
	maxDepth := h.cfg.Agent.MaxDepth
	if maxDepth == 0 {
		maxDepth = 1
	}
	ev.Group = h.s.opts.Alias
	for _, name := range trigger.Firing(h.cfg.Hooks, ev, h.s.opts.Node.NodeID, maxDepth) {
		ev := ev
		ev.Trigger = name
		// A reread replaces the maps, never changes them.
		h.queue(name, fmt.Sprintf("on %s at seq %d", ev.Type, ev.Seq), h.cfg.Hooks[name].Jobs, h.cfg.Jobs, ev)
	}
}

// firingQueues runs each Trigger's firings one at a time, in order;
// different Triggers' run side by side.
type firingQueues struct {
	mu     sync.Mutex
	queues map[string][]func()
	wg     sync.WaitGroup
}

// add queues run behind Trigger name's firings, starting its worker if idle.
func (q *firingQueues) add(name string, run func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queues == nil {
		q.queues = map[string][]func(){}
	}
	pending, busy := q.queues[name]
	q.queues[name] = append(pending, run)
	if busy {
		return
	}
	q.wg.Go(func() {
		for {
			q.mu.Lock()
			if len(q.queues[name]) == 0 {
				delete(q.queues, name)
				q.mu.Unlock()
				return
			}
			next := q.queues[name][0]
			q.mu.Unlock()
			next()
			q.mu.Lock()
			q.queues[name] = q.queues[name][1:]
			q.mu.Unlock()
		}
	})
}

// idle reports whether Trigger name has no firing queued or running.
func (q *firingQueues) idle(name string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, busy := q.queues[name]
	return !busy
}

// wait waits for every queued firing; ctx ending cuts them short.
func (q *firingQueues) wait() { q.wg.Wait() }

// rowEvent is the event a COSL row is, without its Conversation's name:
// false for a row that does not decode.
func rowEvent(r cosl.Row) (trigger.Event, bool) {
	ev := trigger.Event{
		Type: cosl.TypeName(r.MsgType), Seq: r.Seq, MsgID: hex.EncodeToString(r.MsgID[:]),
		From: r.Sender.String(), ConversationID: hex.EncodeToString(r.ConversationID),
	}
	msg, err := payload(r)
	if err != nil {
		return trigger.Event{}, false
	}
	user := func(h string) string {
		if h == "" {
			return config.HandleUser
		}
		return h
	}
	switch m := msg.(type) {
	case *coslv1.AppFreeform:
		ev.Text, ev.Handle, ev.Depth = m.Text, user(m.Handle), m.Depth
	case *coslv1.SignRequest:
		ev.Handle, ev.SignRequestID, ev.Depth = user(m.Handle), ev.MsgID, m.Depth
	case *coslv1.SignAccept:
		ev.Handle, ev.SignRequestID = user(m.Handle), hex.EncodeToString(m.SignRequestId)
	case *coslv1.SignRefuse:
		ev.Handle, ev.SignRequestID = user(m.Handle), hex.EncodeToString(m.SignRequestId)
	case *coslv1.SignBroadcast:
		ev.SignRequestID = hex.EncodeToString(m.SignRequestId)
	case *coslv1.SessionStart:
		if len(m.SignRequestId) > 0 {
			ev.SignRequestID = hex.EncodeToString(m.SignRequestId)
		}
	}
	return ev, true
}

// payload decodes r's payload message.
func payload(r cosl.Row) (any, error) {
	env, err := r.Decode()
	if err != nil {
		return nil, err
	}
	return cosl.DecodePayload(env.MsgType, env.Payload)
}

// hookConversations names a Conversation for a Hook: by its Workspace name,
// "group" for Group-level talk, or its hex ID if its Key cannot be read.
func hookConversations(store *cosl.Store) func([]byte) string {
	var names map[[32]byte]string
	if keys, err := key.Read(store); err == nil {
		names = key.WorkspaceNames(keys)
	}
	return func(id []byte) string {
		if id == nil {
			return config.ConversationGroup
		}
		if len(id) == 32 {
			if name, ok := names[[32]byte(id)]; ok {
				return name
			}
		}
		return hex.EncodeToString(id)
	}
}
