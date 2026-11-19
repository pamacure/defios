// Package talk is this Node's view of the talk of one Conversation, or of
// Group-level talk (docs/adr/0024-a-recap-is-a-local-file.md,
// docs/adr/0025-an-outbox-holds-what-a-node-writes-without-a-quorum.md):
// its committed app.freeform rows, then the messages it wrote that its
// replica does not hold yet, each decoded once, under this Node's Recap;
// and its chat expanded with the Transcripts of this Node's chains.
// serve and every frontend read the talk through it; it returns plain
// values, and formats nothing but the lines agents read.
package talk

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	// Unapplied is a message of this Node's that its replica does not
	// hold yet: queued, or committed, with its seq, and not yet applied.
	// The replica may still lack talk before such a seq (ADR 0025).
	Unapplied bool
}

// MsgID is m's msg_id in hex, as a chain and a Transcript name it (ADR
// 0025).
func (m Message) MsgID() string { return hex.EncodeToString(m.Row.MsgID[:]) }

// Readable reports whether m says anything: it is neither pruned nor
// unreadable.
func (m Message) Readable() bool { return !m.Pruned && !m.Unreadable }

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
	Messages Messages // all of it, pruned and unreadable included
	// recap is this Node's Recap, nil if it has none; recapErr is why it
	// did not read, which only the lean view needs.
	recap    *recap.Recap
	recapErr error
}

// Lean is the talk under this Node's Recap, as `conversation log --lean`
// prints it (ADR 0024). Agents are not given it: each replays its own
// Transcript, and reads the talk itself (ADR 0036).
type Lean struct {
	Recap *recap.Recap // this Node's, nil if it has none
	// Messages is the readable talk after Recap.BaseSeq, queued talk
	// included; all of it with no Recap.
	Messages Messages
	// Missing is whether this replica has pruned talk the Recap does not
	// account for, or any talk with no Recap: nothing stands in its
	// place.
	Missing bool
}

// Lean is v under its Recap: nothing is folded from the log, only the
// Recap stands in for talk, and a queued message is always after it. A
// RECAP.md that does not parse is an error naming it.
func (v View) Lean() (Lean, error) {
	if v.recapErr != nil {
		return Lean{}, v.recapErr
	}
	l := Lean{Recap: v.recap}
	for _, m := range v.Messages {
		if v.recap != nil && !m.Row.Queued() && m.Row.Seq <= v.recap.BaseSeq {
			continue
		}
		if m.Pruned {
			l.Missing = true
		}
		if m.Readable() {
			l.Messages = append(l.Messages, m)
		}
	}
	return l, nil
}

// Chat is the talk as chat: every readable message this replica holds,
// from the first, as a person or agent said it, with no Recap standing in.
func (v View) Chat() Messages {
	var out Messages
	for _, m := range v.Messages {
		if m.Readable() {
			out = append(out, m)
		}
	}
	return out
}

// Read is this Node's view of conv's talk, nil conv for Group-level talk.
// It reads Mine before the replica: a message committed between the two
// reads is in both, and shows once, as committed (ADR 0025). It reads this
// Node's RECAP.md, never another Node's; one that does not parse fails
// only View.Lean.
func (r Reader) Read(conv []byte) (View, error) {
	rc, recapErr := recap.Read(r.Paths.RecapFile(r.Alias, nodedir.ConversationName(conv)))
	var mine []outbox.Entry
	var err error
	if r.Mine != nil {
		if mine, err = r.Mine(); err != nil {
			return View{}, err
		}
	}
	rows, err := r.Store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}, GroupLevel: conv == nil, ConversationID: conv})
	if err != nil {
		return View{}, err
	}
	applied := len(rows) // the rest are this Node's own, from Mine
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
	v := View{Messages: make(Messages, 0, len(rows)), recap: rc, recapErr: recapErr}
	for i, row := range rows {
		m, err := r.message(row)
		if err != nil {
			return View{}, err
		}
		m.Unapplied = i >= applied
		v.Messages = append(v.Messages, m)
	}
	return v, nil
}

// message is row decoded, once.
func (r Reader) message(row cosl.Row) (Message, error) { return Decode(row, r.Roster) }

// Decode is one app.freeform row as a Message, its sender named from
// roster, which may be nil.
func Decode(row cosl.Row, roster *group.Roster) (Message, error) {
	m := Message{Row: row, Pruned: row.Pruned()}
	if roster != nil {
		if n, ok := roster.Lookup(row.Sender); ok {
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

// DefaultPage is how many committed lines a Page holds when none is asked
// for, and MaxPage the most it ever holds.
const (
	DefaultPage = 50
	MaxPage     = 200
)

// Page is one page of talk as an agent reads it with talk_read (ADR 0036
// and ADR 0039).
type Page struct {
	// Lines are the page, oldest first, one JSON object each: a message,
	// one gap for a stretch this Node has pruned, then each message with
	// no seq yet (see Entry).
	Lines []string
	// Earlier is the before that gives the page before this one, 0 when
	// this page reaches the start of the talk.
	Earlier uint64
}

// Entry is one line of a Page, its keys kept short to spend few tokens: a
// message, with its seq or queued, when its sender wrote it (as a turn's
// line gives it, see Written), who and what; or a stretch this Node
// has pruned, by its first and last seq.
type Entry struct {
	Seq        uint64  `json:"seq,omitempty"`
	Queued     bool    `json:"queued,omitempty"`
	At         string  `json:"at,omitempty"`
	From       string  `json:"from,omitempty"` // the sender's display name
	Handle     string  `json:"handle,omitempty"`
	Text       *string `json:"text,omitempty"`
	PrunedFrom uint64  `json:"pruned_from,omitempty"`
	PrunedTo   uint64  `json:"pruned_to,omitempty"`
}

// entry is m, which is readable, as a Page's Entry.
func (m Message) entry(name func(Message) string) Entry {
	text := m.Text
	return Entry{Seq: m.Row.Seq, Queued: m.Row.Queued(), At: m.Written(), From: name(m), Handle: m.Handle, Text: &text}
}

// jsonLine is v as one line of JSON, its <, > and & as written.
func jsonLine(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // an Entry always encodes
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// Page is the last committed lines of ms, at most last of them
// (DefaultPage when last is not positive, and never over MaxPage), then
// every queued message, so that each is on the latest page; or with
// before, the last of the talk before that seq, which no queued message
// is. name names a message's sender. An unreadable message is on no page,
// and ends a pruned stretch.
func (ms Messages) Page(last int, before uint64, name func(Message) string) Page {
	if last <= 0 {
		last = DefaultPage
	}
	last = min(last, MaxPage)
	lines, queued := ms.lines(before, nil)
	var p Page
	if cut := len(lines) - last; cut > 0 {
		p.Earlier = lines[cut-1].to + 1
		lines = lines[cut:]
	}
	for _, l := range lines {
		e := Entry{PrunedFrom: l.from, PrunedTo: l.to}
		if l.m != nil {
			e = l.m.entry(name)
		}
		p.Lines = append(p.Lines, jsonLine(e))
	}
	for _, m := range queued {
		p.Lines = append(p.Lines, jsonLine(m.entry(name)))
	}
	return p
}

// Lines is all of ms as an agent's turn reads it, oldest first: a
// message's Line, a gap line for a stretch this Node has pruned, then the
// queued messages, however many there are, less each message leftOut
// reports (none with nil leftOut). A message left out ends a pruned
// stretch, as an unreadable one does: no gap line covers it.
func (ms Messages) Lines(leftOut func(Message) bool, name func(Message) string) []string {
	committed, queued := ms.lines(0, leftOut)
	text := make([]string, 0, len(committed)+len(queued))
	for _, l := range committed {
		switch {
		case l.m != nil:
			text = append(text, l.m.Line(name))
		case l.from == l.to:
			text = append(text, fmt.Sprintf("[%d pruned on this Node]", l.from))
		default:
			text = append(text, fmt.Sprintf("[%d–%d pruned on this Node]", l.from, l.to))
		}
	}
	for _, m := range queued {
		text = append(text, m.Line(name))
	}
	return text
}

// line is one committed line of talk as an agent reads it: a message, or
// a pruned stretch, from its first seq to its last.
type line struct {
	m        *Message // nil for a pruned stretch
	from, to uint64
}

// lines are ms as an agent reads them: the committed lines, a message or
// a pruned stretch each, then the queued messages; with before, only the
// talk before that seq, which no queued message is. A message leftOut
// reports is on no line, and ends a pruned stretch.
func (ms Messages) lines(before uint64, leftOut func(Message) bool) (committed []line, queued Messages) {
	inGap := false // whether the message before was pruned
	for i, m := range ms {
		seq := m.Row.Seq
		if before > 0 && (m.Row.Queued() || seq >= before) {
			continue
		}
		if leftOut != nil && leftOut(m) {
			inGap = false
			continue
		}
		wasGap := inGap
		inGap = m.Pruned
		switch {
		case m.Pruned && wasGap:
			committed[len(committed)-1].to = seq
		case m.Pruned:
			committed = append(committed, line{from: seq, to: seq})
		case m.Unreadable:
		case m.Row.Queued():
			queued = append(queued, m)
		default:
			committed = append(committed, line{m: &ms[i], from: seq, to: seq})
		}
	}
	return committed, queued
}

// Line is m, which is readable, as an agent's turn reads it: `[<seq>
// <time>] <speaker>: <text>`, with `queued` in place of the seq for one
// that has no seq yet (ADR 0036). The time is when its sender wrote it,
// by the sender's clock, in UTC to the minute (ADR 0039).
func (m Message) Line(name func(Message) string) string {
	at := "queued"
	if !m.Row.Queued() {
		at = fmt.Sprint(m.Row.Seq)
	}
	return "[" + at + " " + m.Written() + "] " + LeanLine(name(m), m.Handle, m.Text)
}

// Written is when m's sender wrote it, by the sender's clock, as an agent
// reads it: `YYYY-MM-DD HH:MM` in UTC, readable without converting a
// timestamp (ADR 0039).
func (m Message) Written() string {
	return time.Unix(0, m.Row.CreatedAt).UTC().Format("2006-01-02 15:04")
}
