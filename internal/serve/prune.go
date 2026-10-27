package serve

import (
	"context"
	"encoding/hex"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/recap"
)

// pruneTick paces the prune policy's check for a new Recap or a restore.
// Talk sits unpruned for at most this long; nothing reads it meanwhile.
const pruneTick = time.Second

// keepPruned is the prune policy (ADR 0012): with `prune = true` in this
// Node's Group config.toml, it destroys the payload of all talk a live
// Recap accounts for, on startup, whenever a Recap commits, and after a
// snapshot restore, until ctx ends. Each prune is logged with what it
// dropped.
func (s *server) keepPruned(ctx context.Context, store *cosl.Store) {
	k := recap.NewKeeper(store)
	for {
		pruned, err := k.Step()
		if err != nil {
			s.opts.Logf("serve: prune policy: %v", err)
		}
		if len(pruned) > 0 {
			names := conversationNames(store)
			for _, p := range pruned {
				s.opts.Logf("serve: prune policy: pruned %d messages of %s up to seq %d", p.Messages, names(p.ConversationID), p.BaseSeq)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pruneTick):
		}
	}
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
