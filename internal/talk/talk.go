// Package talk is this Node's view of the talk of one Conversation, or of
// Group-level talk (docs/adr/0024-a-recap-is-a-local-file.md,
// docs/adr/0025-an-outbox-holds-what-a-node-writes-without-a-quorum.md):
// its committed app.freeform rows, then the messages it wrote that its
// replica does not hold yet, each decoded once, under this Node's Recap.
// serve and every frontend read the talk through it; it returns plain
// values, and formats nothing but the lean line agents read.
package talk

import (
	"bytes"
	"fmt"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/nodedir"
	"defios.com/internal/outbox"
	"defios.com/internal/recap"
	coslv1 "defios.com/proto/cosl/v1"
)

// Reader reads this Node's view of the talk of one Group.
type Reader struct {
	Paths nodedir.Paths
	Alias string
	Store *cosl.Store // this Node's replica
	// Mine is this Node's messages its replica may not hold: Live for a
	// running serve's Outbox, File for the Outbox on disk.
	Mine func() ([]outbox.Entry, error)
	// Roster names senders; nil leaves Message.Sender empty.
	Roster *group.Roster
}

// Live is a running serve's Outbox as Mine: what it committed that the
// replica has not applied, then what is queued.
func Live(o *outbox.Outbox) func() ([]outbox.Entry, error) {
	return func() ([]outbox.Entry, error) { return o.Unapplied(), nil }
}

// File is the Outbox at path as Mine, for a reader with no serve: what is
// queued in it, with no seq.
func File(path string) func() ([]outbox.Entry, error) {
	return func() ([]outbox.Entry, error) {
		queued, err := outbox.Read(path)
		if err != nil {
			return nil, err
		}
		mine := make([]outbox.Entry, len(queued))
		for i, q := range queued {
			mine[i] = outbox.Entry{Signed: q}
		}
		return mine, nil
	}
}

// Message is one app.freeform of the talk.
type Message struct {
	Row cosl.Row // its row; a queued one's has no seq
	// Handle is the agent that wrote it, "" for a person, and Depth its
	// place in a chain, 0 for a person (ADR 0015).
	Handle      string
	Text        string
	ContentType string
	Depth       uint32
	// Pruned is a row whose payload this Node has destroyed, and
	// Unreadable one whose payload does not parse (the FSM does not parse
	// payloads, ADR 0022): neither says anything.
	Pruned, Unreadable bool
	Sender             string // the sender's display name, "" with no Roster or none for it
}

// readable reports whether m says anything.
func (m Message) readable() bool { return !m.Pruned && !m.Unreadable }

// Messages is talk in order: committed by seq, then queued.
type Messages []Message

// Last is the last n of ms, or all of them when n is 0.
func (ms Messages) Last(n int) Messages {
	if n > 0 && len(ms) > n {
		return ms[len(ms)-n:]
	}
	return ms
}

// View is this Node's view of the talk.
type View struct {
	Messages Messages     // all of it, pruned and unreadable included
	Recap    *recap.Recap // this Node's, nil if it has none
}

// Last is the last n of v's messages, or all of them when n is 0.
func (v View) Last(n int) Messages { return v.Messages.Last(n) }

// Lean is the talk as this Node's agents read it, and as `conversation log
// --lean` prints it (ADR 0024).
type Lean struct {
	Recap *recap.Recap // this Node's, nil if it has none
	// Messages is the readable talk after Recap.BaseSeq, queued talk
	// included; all of it with no Recap.
	Messages Messages
	// Missing is whether this replica has pruned talk the Recap does not
	// account for, or any talk with no Recap: this Node's agents read
	// nothing in its place.
	Missing bool
}

// Lean is v under its Recap: nothing is folded from the log, only the
// Recap stands in for talk, and a queued message is always after it.
func (v View) Lean() Lean {
	l := Lean{Recap: v.Recap}
	for _, m := range v.Messages {
		if v.Recap != nil && !m.Row.Queued() && m.Row.Seq <= v.Recap.BaseSeq {
			continue
		}
		if m.Pruned {
			l.Missing = true
		}
		if m.readable() {
			l.Messages = append(l.Messages, m)
		}
	}
	return l
}

// Chat is the talk as chat: every readable message this replica holds,
// from the first, as a person or agent said it, with no Recap standing in.
func (v View) Chat() Messages {
	var out Messages
	for _, m := range v.Messages {
		if m.readable() {
			out = append(out, m)
		}
	}
	return out
}

// Read is this Node's view of conv's talk, nil conv for Group-level talk.
// It reads Mine before the replica: a message committed between the two
// reads is in both, and shows once, as committed (ADR 0025). It reads this
// Node's RECAP.md, never another Node's; one that does not parse is an
// error naming it.
func (r Reader) Read(conv []byte) (View, error) {
	rc, err := recap.Read(r.Paths.RecapFile(r.Alias, nodedir.ConversationName(conv)))
	if err != nil {
		return View{}, err
	}
	var mine []outbox.Entry
	if r.Mine != nil {
		if mine, err = r.Mine(); err != nil {
			return View{}, err
		}
	}
	rows, err := r.Store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}, GroupLevel: conv == nil, ConversationID: conv})
	if err != nil {
		return View{}, err
	}
	held := make(map[[32]byte]bool, len(rows))
	for _, row := range rows {
		held[row.MsgID] = true
	}
	for _, e := range mine {
		if held[e.MsgID] {
			continue
		}
		row, err := cosl.RowOf(e.Signed, e.Seq)
		if err != nil {
			return View{}, fmt.Errorf("talk: %w", err)
		}
		if row.MsgType == cosl.TypeAppFreeform && bytes.Equal(row.ConversationID, conv) {
			rows = append(rows, row)
		}
	}
	v := View{Recap: rc, Messages: make(Messages, 0, len(rows))}
	for _, row := range rows {
		m, err := r.message(row)
		if err != nil {
			return View{}, err
		}
		v.Messages = append(v.Messages, m)
	}
	return v, nil
}

// message is row decoded, once.
func (r Reader) message(row cosl.Row) (Message, error) {
	m := Message{Row: row, Pruned: row.Pruned()}
	if r.Roster != nil {
		if n, ok := r.Roster.Lookup(row.Sender); ok {
			m.Sender = n.DisplayName
		}
	}
	if m.Pruned {
		return m, nil
	}
	env, err := row.Decode()
	if err != nil {
		return Message{}, fmt.Errorf("talk: msg_id %x: %w", row.MsgID, err)
	}
	msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
	if err != nil {
		m.Unreadable = true
		return m, nil
	}
	ff := msg.(*coslv1.AppFreeform)
	m.Handle, m.Text, m.ContentType, m.Depth = ff.Handle, ff.Text, ff.ContentType, ff.Depth
	return m, nil
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
