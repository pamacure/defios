package node

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/talk"
	"defios.com/internal/transcript"
)

// Talk failures a front end tells apart, besides ErrNotServed and
// NoReplicaError: key.ErrUnknown and key.ErrAmbiguous (a Key query naming
// no Key or two), cosl.ErrBadText, serve.ErrBadHandle (a mention or Handle
// not included where the message is sent), serve.ErrNoConversation,
// serve.ErrNotAChain and serve.ErrAmbiguousChain (a chain's msg_id prefix
// naming none or two), raftx.ErrNoLeader and raftx.ErrNotInserted.

// NoReplicaError: the Group has no COSL replica on this Node yet, so there
// is no talk and no Key to read. It has one once the Group is valid and
// Raft starts.
type NoReplicaError struct{ Group string }

func (e *NoReplicaError) Error() string {
	return fmt.Sprintf("this Node has no COSL replica for Group %q yet; it is created once the Group is valid and Raft starts", e.Group)
}

// GroupTalk, as a Conversation ID, is the Group-level talk.
const GroupTalk = ""

// ConversationSummary is one Conversation a front end lists: the
// Group-level talk, or one Key's.
type ConversationSummary struct {
	ID  string // hex Conversation ID; GroupTalk for the Group-level talk
	Key *Key   // the Key it is about; nil for the Group-level talk
	// OpenSignRequests counts its Sign Requests not yet settled: open,
	// waiting or signing.
	OpenSignRequests int
}

// Conversations are what a Talk page lists.
type Conversations struct {
	// Talk is the Group-level talk first, then one per Key in mint order.
	Talk []ConversationSummary
	// Keygens are the keygen Sessions still open, which mint a Key and its
	// Conversation once they complete.
	Keygens []session.Summary
}

// Message is one message of a Conversation as this Node holds it.
type Message struct {
	Seq, Term uint64 // 0 while queued
	MsgID     string // hex
	CreatedAt time.Time
	// NodeID is the Node that wrote it, hex, and DisplayName its name in
	// the Roster, "" if the Roster has none for it.
	NodeID      string
	DisplayName string
	// Handle is the agent that wrote it, "" for a person or a Job; Depth
	// its place in a chain, 0 for a person (ADR 0015, ADR 0032).
	Handle string
	Depth  uint32
	// Mine is this person's: this Node's, written by no agent and no Job.
	Mine              bool
	Text, ContentType string
	// Pruned: this Node has destroyed its payload, so only who and when
	// remain. Unreadable: its payload does not parse. Neither has Text,
	// Handle or Depth.
	Pruned, Unreadable bool
	// Queued: it waits in this Node's Outbox, not on the log yet.
	Queued bool
}

// Readable reports whether m says anything.
func (m Message) Readable() bool { return !m.Pruned && !m.Unreadable }

// Lean is a Conversation as this Node's agents read it (ADR 0024).
type Lean struct {
	Recap *Recap // this Node's, nil if it has none
	// Messages is the readable talk after the Recap, all of it with none.
	Messages []Message
	// Missing: this Node has pruned talk the Recap does not account for.
	Missing bool
}

// Recap is this Node's account of a Conversation up to BaseSeq.
type Recap struct {
	BaseSeq uint64
	Text    string
}

// ChatMessage is one message of the chat, with the Transcript blocks of
// the run of this Node's that said it when asked for and it has one.
type ChatMessage struct {
	Message
	Blocks []json.RawMessage
}

// Outgoing is a message to send. Handle "" is no agent's: this person's
// at Depth 0, or an exec Job's at its DEFIOS_DEPTH (ADR 0032).
type Outgoing struct {
	Text   string
	Handle string // without its "@"
	Depth  uint32
}

// Sent is a message after Send: committed at Seq, or Queued.
type Sent struct {
	Seq, Term uint64
	MsgID     string // hex: it names the chain its mentions start
	Queued    bool
}

// Cancelled is what Cancel found: whether a chain was cancelled, and which
// by the hex msg_id that started it.
type Cancelled struct {
	Cancelled bool
	Chain     string
}

// ChainEvent is one event of agents answering in a Conversation, or of a
// Trigger firing there and its Jobs ending, as `conversation follow`
// streams it.
type ChainEvent = serve.ChainEvent

// ChainStream is one Conversation's chain events as they happen.
type ChainStream = serve.ChainStream

// Transcript is what this Node's chain did in a Conversation: its lines as
// stored, and the runs of its agents folded from them.
type Transcript struct {
	Chain string // the hex msg_id that started it
	Lines []transcript.Line
	Runs  []transcript.Run
}

// replica opens groupAlias's COSL replica; the caller closes it.
func (h *Host) replica(groupAlias string) (*cosl.Store, error) {
	if err := h.knownGroup(groupAlias); err != nil {
		return nil, err
	}
	path := h.p.CoslFile(groupAlias)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, &NoReplicaError{Group: groupAlias}
	} else if err != nil {
		return nil, err
	}
	return cosl.Open(path)
}

// conversationID is a hex Conversation ID as bytes, nil for GroupTalk.
func conversationID(conv string) ([]byte, error) {
	if conv == GroupTalk {
		return nil, nil
	}
	id, err := hex.DecodeString(conv)
	if err != nil || len(id) != 32 {
		return nil, invalidf("Conversation ID %q is not 32-byte hex", conv)
	}
	return id, nil
}

// ResolveConversation is the Conversation ID of the Key query names in
// groupAlias: curve:hex, its Conversation ID or its alias.
func (h *Host) ResolveConversation(_ context.Context, groupAlias, query string) (string, error) {
	store, err := h.replica(groupAlias)
	if err != nil {
		return "", err
	}
	defer store.Close()
	keys, err := key.Read(store)
	if err != nil {
		return "", err
	}
	k, err := config.FindKey(h.p, groupAlias, keys, query)
	if err != nil {
		return "", err
	}
	return k.Conversation(), nil
}

// Conversations lists groupAlias's Conversations and the keygen Sessions
// in progress. Share presence is as Keys reports it with no passphrase.
func (h *Host) Conversations(ctx context.Context, groupAlias string) (Conversations, error) {
	keys, err := h.Keys(ctx, groupAlias, nil)
	if err != nil {
		return Conversations{}, err
	}
	store, err := h.replica(groupAlias)
	if err != nil {
		return Conversations{}, err
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{MsgTypes: signreq.MsgTypes})
	if err != nil {
		return Conversations{}, err
	}
	requests, err := signreq.List(rows, time.Now())
	if err != nil {
		return Conversations{}, err
	}
	open := map[string]int{}
	for _, r := range requests {
		switch r.Status {
		case signreq.Open, signreq.Waiting, signreq.Signing:
			open[r.Conversation]++
		}
	}
	out := Conversations{Talk: []ConversationSummary{{ID: GroupTalk}}}
	for i := range keys {
		k := &keys[i]
		out.Talk = append(out.Talk, ConversationSummary{ID: k.ConversationID, Key: k, OpenSignRequests: open[k.ConversationID]})
	}
	sessions, err := session.List(rows, serve.SignInfoOf(rows))
	if err != nil {
		return Conversations{}, err
	}
	for _, s := range sessions {
		if s.SignRequest == "" && s.Status == session.Open {
			out.Keygens = append(out.Keygens, s)
		}
	}
	return out, nil
}

// reader is groupAlias's talk as this Node holds it, from the files: a
// running serve persists its Outbox before each change.
func (h *Host) reader(groupAlias string) (talk.Reader, func(), error) {
	rec, err := group.LoadAlias(h.p.GroupsDir(), groupAlias)
	if err != nil {
		return talk.Reader{}, nil, err
	}
	store, err := h.replica(groupAlias)
	if err != nil {
		return talk.Reader{}, nil, err
	}
	roster := rec.Roster()
	r := talk.Reader{Paths: h.p, Alias: groupAlias, Store: store, Mine: talk.File(h.p.OutboxFile(groupAlias)), Roster: &roster}
	return r, func() { store.Close() }, nil
}

// read is conv's talk in groupAlias.
func (h *Host) read(groupAlias, conv string) (talk.View, talk.Reader, []byte, func(), error) {
	id, err := conversationID(conv)
	if err != nil {
		return talk.View{}, talk.Reader{}, nil, nil, err
	}
	r, done, err := h.reader(groupAlias)
	if err != nil {
		return talk.View{}, talk.Reader{}, nil, nil, err
	}
	v, err := r.Read(id)
	if err != nil {
		done()
		return talk.View{}, talk.Reader{}, nil, nil, err
	}
	return v, r, id, done, nil
}

// Messages is every message of conv this Node holds, in order: committed
// by seq, pruned ones included, then those queued in its Outbox.
func (h *Host) Messages(_ context.Context, groupAlias, conv string) ([]Message, error) {
	v, _, _, done, err := h.read(groupAlias, conv)
	if err != nil {
		return nil, err
	}
	defer done()
	return h.messages(v.Messages), nil
}

// Lean is conv as this Node's agents read it: its Recap, then the talk
// after it.
func (h *Host) Lean(_ context.Context, groupAlias, conv string) (Lean, error) {
	v, _, _, done, err := h.read(groupAlias, conv)
	if err != nil {
		return Lean{}, err
	}
	defer done()
	l, err := v.Lean()
	if err != nil {
		return Lean{}, err
	}
	out := Lean{Messages: h.messages(l.Messages), Missing: l.Missing}
	if l.Recap != nil {
		out.Recap = &Recap{BaseSeq: l.Recap.BaseSeq, Text: l.Recap.Text}
	}
	return out, nil
}

// Chat is conv's readable talk, the latest limit of it (0 for all); with
// transcripts, each message this Node's chain said carries its run's
// Transcript blocks.
func (h *Host) Chat(_ context.Context, groupAlias, conv string, limit int, transcripts bool) ([]ChatMessage, error) {
	v, r, id, done, err := h.read(groupAlias, conv)
	if err != nil {
		return nil, err
	}
	defer done()
	msgs := v.Chat().Last(limit)
	expanded := make([]talk.Expanded, len(msgs))
	for i, m := range msgs {
		expanded[i].Message = m
	}
	if transcripts {
		if expanded, err = r.Expand(id, msgs); err != nil {
			return nil, err
		}
	}
	out := make([]ChatMessage, len(expanded))
	for i, e := range expanded {
		out[i] = ChatMessage{Message: h.message(e.Message), Blocks: e.Blocks}
	}
	return out, nil
}

func (h *Host) messages(ms talk.Messages) []Message {
	out := make([]Message, len(ms))
	for i, m := range ms {
		out[i] = h.message(m)
	}
	return out
}

// message is m as a front end shows it.
func (h *Host) message(m talk.Message) Message {
	r := m.Row
	out := Message{
		Seq: r.Seq, Term: r.Term, MsgID: m.MsgID(),
		CreatedAt: time.Unix(0, r.CreatedAt), NodeID: r.Sender.String(), DisplayName: m.Sender,
		Pruned: m.Pruned, Unreadable: m.Unreadable, Queued: r.Queued(),
	}
	if m.Readable() {
		out.Handle, out.Depth, out.Text, out.ContentType = m.Handle, m.Depth, m.Text, m.ContentType
		out.Mine = r.Sender == h.pub.NodeID && m.Handle == "" && m.Depth == 0
	}
	return out
}

// Send writes a message to conv in groupAlias, which must be served: it
// commits, or is queued in the Outbox when no leader is known. A message
// of no agent's that mentions Handles starts their chain at once; Follow
// before Send to see it from its start.
func (h *Host) Send(ctx context.Context, groupAlias, conv string, m Outgoing) (Sent, error) {
	if _, err := cosl.Freeform(m.Text); err != nil {
		return Sent{}, err
	}
	id, err := conversationID(conv)
	if err != nil {
		return Sent{}, err
	}
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Sent{}, err
	}
	v, err := api.ConversationSendAs(ctx, m.Text, m.Handle, m.Depth, id)
	if err != nil {
		return Sent{}, wrapServed(err)
	}
	return Sent(v), nil
}

// Cancel cancels the chain of agents running in conv, or with chain, a
// hex prefix of a msg_id, the one it started, running or queued. What the
// agent answering would have said is not committed.
func (h *Host) Cancel(ctx context.Context, groupAlias, conv, chain string) (Cancelled, error) {
	id, err := conversationID(conv)
	if err != nil {
		return Cancelled{}, err
	}
	if _, err := serve.ChainPrefix(chain); err != nil {
		return Cancelled{}, &InvalidError{Err: err}
	}
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Cancelled{}, err
	}
	v, err := api.ConversationCancel(ctx, id, chain)
	if err != nil {
		return Cancelled{}, wrapServed(err)
	}
	return Cancelled(v), nil
}

// Follow streams conv's chain events in groupAlias, which must be served,
// until ctx ends or the stream is closed: a chain running now from its
// agent answering, with its output so far, and every chain after it from
// its start. Following cancels nothing.
func (h *Host) Follow(ctx context.Context, groupAlias, conv string) (*ChainStream, error) {
	id, err := conversationID(conv)
	if err != nil {
		return nil, err
	}
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return nil, err
	}
	s, err := api.ConversationStream(ctx, id)
	if err != nil {
		return nil, wrapServed(err)
	}
	return s, nil
}

// Transcript is the Transcript of this Node's chain in conv started by the
// message whose msg_id starts with chain.
func (h *Host) Transcript(_ context.Context, groupAlias, conv, chain string) (Transcript, error) {
	p, err := serve.ChainPrefix(chain)
	if err != nil {
		return Transcript{}, &InvalidError{Err: err}
	}
	if p == "" {
		return Transcript{}, invalidf("name the chain: its msg id, or a unique prefix of it")
	}
	id, err := conversationID(conv)
	if err != nil {
		return Transcript{}, err
	}
	if err := h.knownGroup(groupAlias); err != nil {
		return Transcript{}, err
	}
	chains, err := transcript.List(h.p, groupAlias, id)
	if err != nil {
		return Transcript{}, err
	}
	full, err := serve.MatchChain(p, chains)
	if err != nil {
		return Transcript{}, err
	}
	t, err := transcript.Read(h.p, groupAlias, id, full)
	if err != nil {
		return Transcript{}, err
	}
	return Transcript{Chain: full, Lines: t.Lines, Runs: t.Runs()}, nil
}

// Handles are the agents' Handles included in conv, which a message there
// may mention, in config order.
func (h *Host) Handles(_ context.Context, groupAlias, conv string) ([]string, error) {
	id, err := conversationID(conv)
	if err != nil {
		return nil, err
	}
	var keys []key.Key
	if id != nil {
		store, err := h.replica(groupAlias)
		if err != nil {
			return nil, err
		}
		keys, err = key.Read(store)
		store.Close()
		if err != nil {
			return nil, err
		}
	} else if err := h.knownGroup(groupAlias); err != nil {
		return nil, err
	}
	e, err := config.Load(h.p, groupAlias, id, keys)
	if err != nil {
		return nil, err
	}
	inc, err := e.Included()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(inc.Profiles))
	for i, p := range inc.Profiles {
		out[i] = p.Handle
	}
	return out, nil
}
