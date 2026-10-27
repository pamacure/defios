package recap

import (
	"errors"

	"defios.com/internal/cosl"
)

// ErrNoRecap is a Conversation with no live Recap: nothing has accounted for
// its talk, so none of it may be pruned.
var ErrNoRecap = errors.New("recap: no Recap accounts for this Conversation's talk")

// Prune destroys, on this Node only, the payloads of the talk the live
// Recap of a Conversation accounts for (nil conversationID is Group-level
// talk): its app.freeform rows at or below the Recap's base_seq. Recaps keep
// their text. It reports that base_seq and how many payloads it destroyed;
// talk already pruned is not counted again (ADR 0012).
func Prune(store *cosl.Store, conversationID []byte) (uint64, int, error) {
	q := cosl.Query{ConversationID: conversationID, GroupLevel: conversationID == nil}
	rows, err := store.Log(q)
	if err != nil {
		return 0, 0, err
	}
	v, err := Fold(rows)
	if err != nil {
		return 0, 0, err
	}
	if v.Live == nil {
		return 0, 0, ErrNoRecap
	}
	n, err := store.Prune(conversationID, v.Live.BaseSeq)
	return v.Live.BaseSeq, n, err
}
