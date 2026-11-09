package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/errlog"
	"defios.com/internal/key"
)

// load is the settings in force where conversation's talk is sent,
// Group-level talk for a nil conversation, read at each use so an edit
// needs no restart. The Keys, which name the Conversation's Workspace, are
// read only for a Conversation.
func (s *server) load(store *cosl.Store, conversation []byte) (config.Effective, error) {
	var keys []key.Key
	if len(conversation) > 0 {
		var err error
		if keys, err = key.Read(store); err != nil {
			return config.Effective{}, err
		}
	}
	return config.Load(s.opts.Paths, s.opts.Alias, conversation, keys)
}

// watchWorkspaces gives every minted Key's Conversation a Workspace in
// this Group's, named by key.WorkspaceNames (ADR 0013), on startup and as
// Keys are minted, until ctx ends, so every Node holds one. The log, not
// this Node's keygen engine, is what says a Key exists, so a Key minted
// while this serve was down, or brought by catch-up or a snapshot restore,
// is covered too. It wakes when the replica changes, or at the fallback
// tick (ADR 0014). Its markdown is not read yet; its config.toml is read
// where a Key-scoped action uses it. Creating the Workspace is the one time
// the alias proposed on the Key's keygen is applied (ADR 0033).
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
func (w *workspaceWatcher) wake() { w.errs.Report(w.follow.Act(w.catchUp)) }

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
	var aliases map[string]config.KeyAlias // read only if an alias is to be written
	for _, k := range keys {               // in mint order, so a repeated error reads alike
		id := key.ConversationID(k.Curve, k.PublicKey)
		name := names[id]
		if w.made[id] {
			continue
		}
		created, err := createDir(w.s.opts.Paths.ConversationWorkspace(w.s.opts.Alias, name))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		w.made[id] = true
		chosen := w.s.holds.of(k.SessionID)
		if created && (chosen != "" || k.ProposedAlias != "" || k.ProposalInvalid) {
			if aliases == nil {
				aliases = config.ReadKeyAliases(w.s.opts.Paths, w.s.opts.Alias, keys)
			}
			if err := applyAlias(k, chosen, keys, aliases); err != nil {
				w.s.opts.Logf("serve: warning: Key %s keeps no alias on this Node: %v", k.Ref(), err)
			}
		}
		w.s.holds.release(k.SessionID) // only now, for its config.toml now says it is taken
	}
	return errors.Join(errs...)
}

// createDir makes dir and reports whether this call is what created it, so
// that only one caller, once, acts on its being new.
func createDir(dir string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return false, err
	}
	err := os.Mkdir(dir, 0o700)
	if errors.Is(err, os.ErrExist) {
		return false, os.MkdirAll(dir, 0o700) // fails if the name is not a directory
	}
	return err == nil, err
}

// applyAlias writes k's alias into its new Workspace's config.toml and records
// it in aliases, for the next Key this wake: chosen, this Node's own from
// session accept, or else the one proposed at keygen. An error says why it does
// not apply here, which never stops the Key: the mint has already happened, and
// a proposal comes from another Node and cannot block a keygen (ADR 0033).
func applyAlias(k key.Key, chosen string, keys []key.Key, aliases map[string]config.KeyAlias) error {
	alias, origin := chosen, "chosen at session accept"
	if chosen == "" {
		if k.ProposalInvalid {
			return errors.New("the alias proposed at keygen is invalid") // and not repeated here
		}
		alias, origin = k.ProposedAlias, "proposed at keygen"
	}
	holders, err := config.AliasHolders(keys, aliases, alias)
	if err != nil {
		return err
	}
	if len(holders) > 0 {
		return fmt.Errorf("the alias %q %s is already on %s", alias, origin, holders[0].Ref())
	}
	own := aliases[k.Ref()]
	if err := config.WriteKeyAlias(own.File, alias); err != nil {
		return err
	}
	own.Alias = alias
	aliases[k.Ref()] = own
	return nil
}
