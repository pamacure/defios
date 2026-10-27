package recap

import (
	"errors"

	"defios.com/internal/cosl"
)

// Keeper re-applies Pruning for a Node whose prune policy is on, so that no
// talk a live Recap accounts for keeps its payload (ADR 0012). A Recap that
// commits widens what is covered, and a snapshot restore from a Node that
// kept its talk brings payloads back, so each is a reason to prune again.
type Keeper struct {
	store    *cosl.Store
	stepped  bool   // whether any Step has pruned yet
	recapSeq uint64 // the latest app.recap row as of the last prune
	restores uint64 // the Store's restore count as of the last prune
}

// Pruned is what one automatic prune destroyed in one Conversation (nil
// ConversationID is Group-level talk).
type Pruned struct {
	ConversationID []byte
	BaseSeq        uint64
	Messages       int
}

// NewKeeper keeps store pruned.
func NewKeeper(store *cosl.Store) *Keeper { return &Keeper{store: store} }

// Step prunes every Conversation's covered talk if this is the first Step,
// a Recap has committed since the last, or the replica has been restored
// since; otherwise it reads one row and does nothing. It reports each
// Conversation where it destroyed a payload. After an error the next Step
// tries again.
func (k *Keeper) Step() ([]Pruned, error) {
	// Read the triggers first, so a Recap or restore landing mid-prune is
	// seen by the next Step.
	restores := k.store.Restores()
	typ := cosl.TypeAppRecap
	latest, err := k.store.Log(cosl.Query{MsgType: &typ, Limit: 1})
	if err != nil {
		return nil, err
	}
	var recapSeq uint64
	if len(latest) > 0 {
		recapSeq = latest[0].Seq
	}
	if k.stepped && recapSeq == k.recapSeq && restores == k.restores {
		return nil, nil
	}
	out, err := pruneAll(k.store)
	if err != nil {
		return nil, err
	}
	k.stepped, k.recapSeq, k.restores = true, recapSeq, restores
	return out, nil
}

// pruneAll prunes every Conversation that has a Recap, Group-level talk
// included, in the order their first Recaps committed.
func pruneAll(store *cosl.Store) ([]Pruned, error) {
	typ := cosl.TypeAppRecap
	recaps, err := store.Log(cosl.Query{MsgType: &typ})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Pruned
	for _, r := range recaps {
		key := string(r.ConversationID) // "" for Group-level: never a 32-byte ID
		if seen[key] {
			continue
		}
		seen[key] = true
		base, n, err := Prune(store, r.ConversationID)
		if errors.Is(err, ErrNoRecap) {
			continue // every Recap it has is one the fold skips
		}
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, Pruned{ConversationID: r.ConversationID, BaseSeq: base, Messages: n})
		}
	}
	return out, nil
}
