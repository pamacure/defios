package recap

import (
	"fmt"

	"defios.com/internal/cosl"
)

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
