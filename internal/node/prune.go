package node

import (
	"context"

	"defios.com/internal/cosl"
	"defios.com/internal/nodedir"
	"defios.com/internal/recap"
)

// Pruning is what a prune of a Conversation destroys (ADR 0012, ADR 0024),
// or would: Messages payloads of this Node's, no other Node's. With none
// destroyed, neither warning is set.
type Pruning struct {
	Messages int
	// Unaccounted: this Node's RECAP.md does not account for all of that
	// talk, so this Node's agents lose it with no account in its place.
	Unaccounted bool
	// NoPolicy: the Group has no prune policy, so a later snapshot restore
	// from a Node that kept the talk brings it back.
	NoPolicy bool
}

// PruneCheck is what Prune of conv through seq would destroy now,
// destroying nothing.
func (h *Host) PruneCheck(ctx context.Context, group, conv string, through uint64) (Pruning, error) {
	return h.prune(group, conv, through, false)
}

// Prune destroys the payload of conv's talk at or below seq through on
// this Node, keeping each row and its place in the chain. It is Node-local,
// tells no other Node, and needs the Group neither served nor locked.
func (h *Host) Prune(ctx context.Context, group, conv string, through uint64) (Pruning, error) {
	return h.prune(group, conv, through, true)
}

func (h *Host) prune(group, conv string, through uint64, destroy bool) (Pruning, error) {
	id, err := conversationID(conv)
	if err != nil {
		return Pruning{}, err
	}
	cfg, err := h.groupConfig(group)
	if err != nil {
		return Pruning{}, err
	}
	store, err := h.replica(group)
	if err != nil {
		return Pruning{}, err
	}
	defer store.Close()
	rc, err := recap.Read(h.p.RecapFile(group, nodedir.ConversationName(id)))
	if err != nil {
		return Pruning{}, err
	}
	rows, err := store.Log(cosl.Query{GroupLevel: id == nil, ConversationID: id, MsgTypes: []uint32{cosl.TypeAppFreeform}})
	if err != nil {
		return Pruning{}, err
	}
	var out Pruning
	for _, r := range rows {
		if r.Seq <= through && !r.Pruned() {
			out.Messages++
			out.Unaccounted = out.Unaccounted || !rc.Covers(r.Seq)
		}
	}
	if destroy {
		if out.Messages, err = store.Prune(id, through); err != nil {
			return Pruning{}, err
		}
	}
	if out.Messages == 0 {
		return Pruning{}, nil
	}
	out.NoPolicy = cfg.Prune == 0
	return out, nil
}
