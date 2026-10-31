package recap

import (
	"fmt"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

// Said is one message of the lean view: a row and what it says, with the
// Handle of the agent that wrote it, empty for a person.
type Said struct {
	Row    cosl.Row
	Handle string
	Text   string
}

// View is one Conversation as this Node's agents read it, and as
// `conversation log --lean` prints it (ADR 0024).
type View struct {
	Recap *Recap // this Node's, nil if it has none
	Talk  []Said // the talk after Recap.BaseSeq this replica still holds; all of it with no Recap
	// Missing is whether this replica has pruned talk the Recap does not
	// account for, or any talk with no Recap: this Node's agents read
	// nothing in its place.
	Missing bool
}

// Lean is the view of one Conversation's rows, in seq order as a Query on
// its Conversation ID (or GroupLevel) returns them, under this Node's Recap
// r, nil for none. Nothing is folded from the log: only r stands in for
// talk.
func Lean(r *Recap, rows []cosl.Row) (View, error) {
	v := View{Recap: r}
	for _, row := range rows {
		if row.MsgType != cosl.TypeAppFreeform || (r != nil && row.Seq <= r.BaseSeq) {
			continue
		}
		if row.Pruned() {
			v.Missing = true
			continue
		}
		env, err := row.Decode()
		if err != nil {
			return View{}, err
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			return View{}, fmt.Errorf("recap: seq %d: %w", row.Seq, err)
		}
		ff := msg.(*coslv1.AppFreeform)
		v.Talk = append(v.Talk, Said{Row: row, Handle: ff.Handle, Text: ff.Text})
	}
	return v, nil
}

// Check is `conversation recap`'s precondition: baseSeq names an
// app.freeform among rows, one Conversation's. A pruned one still counts:
// its row, and so its place in the talk, remains.
func Check(rows []cosl.Row, baseSeq uint64) error {
	for _, r := range rows {
		if r.Seq == baseSeq && r.MsgType == cosl.TypeAppFreeform {
			return nil
		}
	}
	return fmt.Errorf("%w: seq %d is not one on this replica (if it was just sent, it may not have arrived yet)", ErrBadBase, baseSeq)
}

// Speaker is how a lean line names who wrote it: the sender's display
// name, and an agent's Handle after it.
func Speaker(displayName, handle string) string {
	if handle == "" {
		return displayName
	}
	return displayName + "/@" + handle
}

// LeanLine is one lean line, `<speaker>: <text>` with newlines escaped.
func LeanLine(displayName, handle, text string) string {
	return Speaker(displayName, handle) + ": " + cosl.EscapeNewlines(text)
}
