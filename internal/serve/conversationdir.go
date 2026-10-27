package serve

import (
	"context"
	"os"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
)

// conversationDirTick paces the scan for newly minted Keys. A directory
// nothing reads yet does not need to appear faster.
const conversationDirTick = time.Second

// watchConversationDirs gives Group-level talk and every minted Key's
// Conversation an agent config directory under this Group's config
// (ADR 0012), on startup and as Keys are minted, until ctx ends. Group-level
// is made here too, not only by group new, so a Group scaffolded before the
// directory existed gains it. The log, not this Node's keygen engine,
// is what says a Key exists, so a Key minted while this serve was down is
// covered too. Nothing inside the directory is read.
func (s *server) watchConversationDirs(ctx context.Context, store *cosl.Store) {
	if err := os.MkdirAll(s.opts.Paths.ConversationConfigDir(s.opts.Alias, nil), 0o700); err != nil {
		s.opts.Logf("serve: group-level Conversation config: %v", err)
	}
	made := map[[32]byte]bool{}
	for {
		s.ensureConversationDirs(store, made)
		select {
		case <-ctx.Done():
			return
		case <-time.After(conversationDirTick):
		}
	}
}

func (s *server) ensureConversationDirs(store *cosl.Store, made map[[32]byte]bool) {
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		s.opts.Logf("serve: reading replica for Conversation config: %v", err)
		return
	}
	keys, err := key.List(rows)
	if err != nil {
		s.opts.Logf("serve: listing Keys for Conversation config: %v", err)
		return
	}
	for _, k := range keys {
		id := key.ConversationID(k.Curve, k.PublicKey)
		if made[id] {
			continue
		}
		if err := os.MkdirAll(s.opts.Paths.ConversationConfigDir(s.opts.Alias, id[:]), 0o700); err != nil {
			s.opts.Logf("serve: Conversation config for %x: %v", id, err)
			continue
		}
		made[id] = true
	}
}
