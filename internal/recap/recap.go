// Package recap folds a Conversation's talk as readers see it
// (docs/adr/0012-recap-on-the-log-pruning-off-it.md): the live Recap stands in
// place of the app.freeform rows it accounts for. As with Networks
// (docs/adr/0008-networks-on-cosl.md) Raft order is the authority and a Recap
// supersedes by fold, never by a write to the replica.
package recap

import (
	"errors"
	"fmt"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

var (
	// ErrStale is a Recap that covers no further than the live one.
	ErrStale = errors.New("recap: base seq must be beyond the live Recap's")
	// ErrBadBase is a Recap whose base seq names no talk of its Conversation.
	ErrBadBase = errors.New("recap: base seq must name an app.freeform of this Conversation")
)

// Recap is the live Recap of a folded Conversation.
type Recap struct {
	Row     cosl.Row
	Text    string
	BaseSeq uint64 // the highest seq it accounts for
	Covers  int    // app.freeform rows at or below BaseSeq, derived at read time
}

// View is one Conversation as readers see it.
type View struct {
	Live *Recap     // nil until a Recap applies
	Talk []cosl.Row // app.freeform rows after Live.BaseSeq; every one with no Recap
}

// Fold replays one Conversation's rows, in seq order as a Query on its
// Conversation ID (or GroupLevel) returns them, into the view readers see.
// A Recap that does not apply to the state it meets is skipped, identically
// on every Node: one whose base_seq is not beyond the live Recap's, one whose
// base_seq names no earlier app.freeform of this Conversation, and one whose
// payload is undecodable or whose text app.freeform would refuse.
func Fold(rows []cosl.Row) (View, error) {
	var live *Recap
	var talk []cosl.Row
	isTalk := map[uint64]bool{}
	for _, r := range rows {
		switch r.MsgType {
		case cosl.TypeAppFreeform:
			talk = append(talk, r)
			isTalk[r.Seq] = true
		case cosl.TypeAppRecap:
			env, err := r.Decode()
			if err != nil {
				return View{}, fmt.Errorf("recap: seq %d: %w", r.Seq, err)
			}
			// The FSM does not parse payloads, so an undecodable one is
			// skipped rather than allowed to break every reader.
			msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
			if err != nil {
				continue
			}
			p := msg.(*coslv1.AppRecap)
			if cosl.CheckText(p.Text) != nil || !isTalk[p.BaseSeq] {
				continue
			}
			if live != nil && p.BaseSeq <= live.BaseSeq {
				continue
			}
			live = &Recap{Row: r, Text: p.Text, BaseSeq: p.BaseSeq}
		}
	}
	v := View{Live: live}
	for _, r := range talk {
		if live != nil && r.Seq <= live.BaseSeq {
			live.Covers++
			continue
		}
		v.Talk = append(v.Talk, r)
	}
	return v, nil
}

// Check is the writer's precondition for appending a Recap covering up to
// baseSeq to the Conversation folded into v; Fold would skip one that fails
// it. Every row already on the replica precedes the new one.
func Check(v View, baseSeq uint64) error {
	if v.Live != nil && baseSeq <= v.Live.BaseSeq {
		return fmt.Errorf("%w: it covers up to seq %d", ErrStale, v.Live.BaseSeq)
	}
	for _, r := range v.Talk {
		if r.Seq == baseSeq {
			return nil
		}
	}
	return fmt.Errorf("%w: seq %d is not one on this replica (if it was just sent, it may not have arrived yet)", ErrBadBase, baseSeq)
}
