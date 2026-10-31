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
)

// pruneEvery is how often the prune policy runs between restores.
const pruneEvery = 24 * time.Hour

// keepPruned is the prune policy (ADR 0024): with an age as `prune` in
// this Node's Group config.toml, it destroys the payload of all talk older
// than that age, in every Conversation, on startup, after a snapshot
// restore, and once a day, until ctx ends. Each run is logged with what it
// dropped. It wakes when the replica changes, to catch a restore, or at the
// fallback tick, where it also rereads config.toml if the file changed, so
// setting or removing `prune` needs no restart (ADR 0014). While there is
// no policy it does not touch the replica.
func (s *server) keepPruned(ctx context.Context, store *cosl.Store) {
	p := s.newPrunePolicy(store)
	tick := time.NewTicker(cosl.FallbackTick)
	defer tick.Stop()
	p.wake()
	for {
		var changes <-chan struct{} // nil with no policy: only the tick wakes it
		if p.age != 0 {
			changes = store.Changed()
		}
		select {
		case <-ctx.Done():
			return
		case <-changes:
			p.wake()
		case <-tick.C:
			p.fallback()
		}
	}
}

// prunePolicy is keepPruned's state between wakes.
type prunePolicy struct {
	s          *server
	store      *cosl.Store
	age        config.Age
	mtime      time.Time // config.toml's as last read; zero while it is missing
	now        func() time.Time
	ran        time.Time // when it last pruned; zero until it has
	restores   uint64    // the Store's restore count as of that run
	errs       *errlog.Reporter
	configErrs *errlog.Reporter
}

// newPrunePolicy starts from the policy serve read as it started.
func (s *server) newPrunePolicy(store *cosl.Store) *prunePolicy {
	p := &prunePolicy{
		s:          s,
		store:      store,
		age:        s.opts.Config.Prune,
		now:        time.Now,
		errs:       errlog.New(s.opts.Logf, "serve: prune policy"),
		configErrs: errlog.New(s.opts.Logf, "serve: prune policy: reading config.toml"),
	}
	if info, err := os.Stat(s.opts.Paths.GroupConfigFile(s.opts.Alias)); err == nil {
		p.mtime = info.ModTime()
	}
	return p
}

// fallback is one fallback tick: reread config.toml, then wake.
func (p *prunePolicy) fallback() {
	p.reread()
	p.wake()
}

// reread takes up config.toml's `prune` if the file's modification time
// changed since it was last read. A file that does not parse is reported,
// and the policy keeps its last setting; a missing file is the default,
// no policy. A changed age prunes at the next wake, as on startup.
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
	if cfg.Prune == p.age {
		return
	}
	p.age, p.ran = cfg.Prune, time.Time{}
	if p.age == 0 {
		p.s.opts.Logf("serve: prune policy: none, as config.toml now says")
		return
	}
	p.s.opts.Logf("serve: prune policy: older than %s, as config.toml now says", p.age)
}

// wake is one turn of the loop. With no policy it does nothing. Otherwise
// it prunes if it never has, if the replica has been restored since it
// last did, or if that was a day ago; else it reads only the clock and the
// Store's restore count.
func (p *prunePolicy) wake() {
	if p.age == 0 {
		return
	}
	restores, now := p.store.Restores(), p.now()
	if !p.ran.IsZero() && restores == p.restores && now.Sub(p.ran) < pruneEvery {
		return
	}
	p.errs.Report(p.prune(now, restores))
}

// prune destroys the payload of the talk older than the policy's age as of
// now, and logs what it dropped. The restore count is read before it, so a
// restore landing mid-prune is pruned at the next wake.
func (p *prunePolicy) prune(now time.Time, restores uint64) error {
	pruned, err := p.store.PruneAllOlderThan(now.Add(-time.Duration(p.age)))
	if err != nil {
		return err
	}
	p.ran, p.restores = now, restores
	if len(pruned) == 0 {
		return nil
	}
	names := conversationNames(p.store)
	for _, r := range pruned {
		p.s.opts.Logf("serve: prune policy: pruned %d messages of %s older than %s", r.Messages, names(r.ConversationID), p.age)
	}
	return nil
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
