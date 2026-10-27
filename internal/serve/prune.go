package serve

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/errlog"
	"defios.com/internal/key"
	"defios.com/internal/recap"
)

// keepPruned is the prune policy (ADR 0012): with `prune = true` in this
// Node's Group config.toml, it destroys the payload of all talk a live
// Recap accounts for, on startup, whenever a Recap commits, and after a
// snapshot restore, until ctx ends. Each prune is logged with what it
// dropped. It wakes when the replica changes, or at the fallback tick,
// where it also rereads config.toml if the file changed, so turning
// `prune` on or off needs no restart (ADR 0014). While the policy is off
// it does not touch the replica.
func (s *server) keepPruned(ctx context.Context, store *cosl.Store) {
	p := s.newPrunePolicy(store)
	tick := time.NewTicker(cosl.FallbackTick)
	defer tick.Stop()
	for {
		p.wake()
		var changes <-chan struct{} // nil while off: only the tick wakes it
		if p.on {
			changes = p.follow.Changes()
		}
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-tick.C:
			p.reread()
		}
	}
}

// prunePolicy is keepPruned's state between wakes.
type prunePolicy struct {
	s          *server
	store      *cosl.Store
	on         bool
	mtime      time.Time // config.toml's as last read; zero while it is missing
	follow     *cosl.Follower
	keeper     *recap.Keeper
	errs       *errlog.Reporter
	configErrs *errlog.Reporter
}

// newPrunePolicy starts from the policy serve read as it started.
func (s *server) newPrunePolicy(store *cosl.Store) *prunePolicy {
	p := &prunePolicy{
		s:          s,
		store:      store,
		errs:       errlog.New(s.opts.Logf, "serve: prune policy"),
		configErrs: errlog.New(s.opts.Logf, "serve: prune policy: reading config.toml"),
	}
	if info, err := os.Stat(s.opts.Paths.GroupConfigFile(s.opts.Alias)); err == nil {
		p.mtime = info.ModTime()
	}
	p.set(s.opts.Config.Prune)
	return p
}

// set turns the policy on or off. Turned on, its next wake prunes
// everything covered, as on startup.
func (p *prunePolicy) set(on bool) {
	p.on = on
	if on {
		p.follow = p.store.Follow(cosl.FallbackTick)
		p.keeper = recap.NewKeeper(p.store)
	} else {
		p.follow, p.keeper = nil, nil
	}
}

// fallback is one fallback tick: reread config.toml, then wake.
func (p *prunePolicy) fallback() {
	p.reread()
	p.wake()
}

// reread takes up config.toml's `prune` if the file's modification time
// changed since it was last read. A file that does not parse is reported,
// and the policy keeps its last setting; a missing file is the default,
// off.
func (p *prunePolicy) reread() {
	path := p.s.opts.Paths.GroupConfigFile(p.s.opts.Alias)
	var mtime time.Time
	info, err := os.Stat(path)
	switch {
	case err == nil:
		mtime = info.ModTime()
	case !errors.Is(err, fs.ErrNotExist):
		p.configErrs.Report(err)
		return
	}
	if mtime.Equal(p.mtime) {
		return
	}
	p.mtime = mtime
	var cfg config.Config
	if !mtime.IsZero() {
		if cfg, err = config.ReadFile(path); err != nil {
			p.configErrs.Report(err)
			return
		}
	}
	p.configErrs.Report(nil)
	if cfg.Prune == p.on {
		return
	}
	p.set(cfg.Prune)
	if cfg.Prune {
		p.s.opts.Logf("serve: prune policy: on, as config.toml now says")
	} else {
		p.s.opts.Logf("serve: prune policy: off, as config.toml now says")
	}
}

// wake is one turn of the loop. With the policy off it does nothing. With
// nothing new on the replica it reads only the replica's Mark; otherwise
// the Keeper's gate reads one row, and prunes only after a Recap or a
// restore.
func (p *prunePolicy) wake() {
	if !p.on {
		return
	}
	// A wake not finished is handed out again, so a Poll with nothing new
	// means the last one succeeded: reporting its nil clears nothing early.
	_, ok, err := p.follow.Poll()
	if err == nil && ok {
		var pruned []recap.Pruned
		if pruned, err = p.keeper.Step(); err == nil {
			p.follow.Done()
			if len(pruned) > 0 {
				names := conversationNames(p.store)
				for _, r := range pruned {
					p.s.opts.Logf("serve: prune policy: pruned %d messages of %s up to seq %d", r.Messages, names(r.ConversationID), r.BaseSeq)
				}
			}
		}
	}
	p.errs.Report(err)
}

// conversationNames names a Conversation for the log as its Workspace is
// named, falling back to its ID if the Key cannot be read.
func conversationNames(store *cosl.Store) func([]byte) string {
	var names map[[32]byte]string
	if keys, err := key.Read(store); err == nil {
		names = key.WorkspaceNames(keys)
	}
	return func(id []byte) string {
		if id == nil {
			return "Group-level talk"
		}
		if name, ok := names[[32]byte(id)]; ok {
			return "Conversation " + name
		}
		return "Conversation " + hex.EncodeToString(id)
	}
}
