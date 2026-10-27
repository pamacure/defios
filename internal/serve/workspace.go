package serve

import (
	"context"
	"errors"
	"fmt"
	"os"

	"defios.com/internal/cosl"
	"defios.com/internal/errlog"
	"defios.com/internal/key"
)

// watchWorkspaces gives every minted Key's Conversation a Workspace in
// this Group's, named by key.WorkspaceNames (ADR 0013), on startup and as
// Keys are minted, until ctx ends, so every Node holds one. The log, not
// this Node's keygen engine, is what says a Key exists, so a Key minted
// while this serve was down, or brought by catch-up or a snapshot restore,
// is covered too. It wakes when the replica changes, or at the fallback
// tick (ADR 0014). Its markdown is not read yet; its config.toml is read
// where a Key-scoped action uses it.
func (s *server) watchWorkspaces(ctx context.Context, store *cosl.Store) {
	w := s.newWorkspaceWatcher(store)
	for {
		w.wake()
		if !w.follow.Wait(ctx) {
			return
		}
	}
}

// workspaceWatcher is watchWorkspaces' state between wakes.
type workspaceWatcher struct {
	s      *server
	store  *cosl.Store
	follow *cosl.Follower
	made   map[[32]byte]bool // Workspaces this watcher has made or found
	errs   *errlog.Reporter
}

func (s *server) newWorkspaceWatcher(store *cosl.Store) *workspaceWatcher {
	return &workspaceWatcher{
		s:      s,
		store:  store,
		follow: store.Follow(cosl.FallbackTick),
		made:   map[[32]byte]bool{},
		errs:   errlog.New(s.opts.Logf, "serve: Conversation Workspaces"),
	}
}

// wake is one turn of the loop. With nothing new on the replica it reads
// only its Mark. Otherwise it looks for a session.complete since the last
// wake it finished, and only if there is one (or the replica was
// restored) lists Keys, from the Session rows alone.
func (w *workspaceWatcher) wake() {
	// A wake not finished is handed out again, so a Poll with nothing new
	// means the last one succeeded: reporting its nil clears nothing early.
	wk, ok, err := w.follow.Poll()
	if err == nil && ok {
		err = w.catchUp(wk)
		if err == nil {
			w.follow.Done()
		}
	}
	w.errs.Report(err)
}

func (w *workspaceWatcher) catchUp(wk cosl.Wake) error {
	if !wk.Rescan {
		completes, err := w.store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeSessionComplete}, AfterSeq: wk.Since, Limit: 1})
		if err != nil {
			return err
		}
		if len(completes) == 0 {
			return nil
		}
	}
	keys, err := key.Read(w.store)
	if err != nil {
		return fmt.Errorf("listing Keys: %w", err)
	}
	names := key.WorkspaceNames(keys)
	var errs []error
	for _, k := range keys { // in mint order, so a repeated error reads alike
		id := key.ConversationID(k.Curve, k.PublicKey)
		name := names[id]
		if w.made[id] {
			continue
		}
		if err := os.MkdirAll(w.s.opts.Paths.ConversationWorkspace(w.s.opts.Alias, name), 0o700); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		w.made[id] = true
	}
	return errors.Join(errs...)
}
