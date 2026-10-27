package serve

import (
	"context"
	"os"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
)

// workspaceTick paces the scan for newly minted Keys. A directory
// nothing reads yet does not need to appear faster.
const workspaceTick = time.Second

// watchWorkspaces gives every minted Key's Conversation a Workspace in
// this Group's (ADR 0013), on startup and as Keys are minted, until ctx
// ends, so every Node holds one. The log, not this Node's keygen engine, is
// what says a Key exists, so a Key minted while this serve was down is
// covered too. Its markdown is not read yet; its config.toml is read where
// a Key-scoped action uses it.
func (s *server) watchWorkspaces(ctx context.Context, store *cosl.Store) {
	made := map[[32]byte]bool{}
	for {
		s.ensureWorkspaces(store, made)
		select {
		case <-ctx.Done():
			return
		case <-time.After(workspaceTick):
		}
	}
}

func (s *server) ensureWorkspaces(store *cosl.Store, made map[[32]byte]bool) {
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		s.opts.Logf("serve: reading replica for Conversation Workspaces: %v", err)
		return
	}
	keys, err := key.List(rows)
	if err != nil {
		s.opts.Logf("serve: listing Keys for Conversation Workspaces: %v", err)
		return
	}
	for _, k := range keys {
		id := key.ConversationID(k.Curve, k.PublicKey)
		if made[id] {
			continue
		}
		if err := os.MkdirAll(s.opts.Paths.ConversationWorkspace(s.opts.Alias, id), 0o700); err != nil {
			s.opts.Logf("serve: Conversation Workspace for %x: %v", id, err)
			continue
		}
		made[id] = true
	}
}
