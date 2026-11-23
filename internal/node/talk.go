package node

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/nodedir"
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
	// Unread counts the messages people wrote past this Node's read mark,
	// this person's own aside; UnreadAutomated is whether agents or Jobs
	// wrote any there, this Node's included. All of it is unread until
	// MarkRead first marks it.
	Unread          int
	UnreadAutomated bool
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

// Lean is a Conversation under this Node's Recap (ADR 0024).
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
	// Chain, with transcripts, is the chain of the run that said it.
	Chain string
	// Summoned, with transcripts, are the Handles that run summoned with
	// talk_summon, in order (ADR 0041).
	Summoned []string
}

// Outgoing is a message to send. Handle "" is no agent's: this person's
// at Depth 0, or an exec Job's at its DEFIOS_DEPTH (ADR 0032).
type Outgoing struct {
	Text   string
	Handle string // without its "@"
	Depth  uint32
	// JobRun is DEFIOS_JOB_RUN when an exec Job writes it: the token naming
	// its Job Run, on which serve records the message (ADR 0040). It never
	// reaches the COSL.
	JobRun string
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

// Transcript is what this Node's chain did in a Conversation, rebuilt from
// its agents' Transcripts (ADR 0036): the lines of its runs as stored, by
// step, and the runs folded from them.
type Transcript struct {
	Chain string // the hex msg_id that started it
	Lines []transcript.Line
	Runs  []transcript.Run
	// Cleared are the steps whose run is in a Transcript since cleared,
	// in order.
	Cleared []int
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
	for i := range out.Talk {
		if err := h.unread(store, groupAlias, &out.Talk[i]); err != nil {
			return Conversations{}, err
		}
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

// unread counts c's talk past this Node's read mark into c.
func (h *Host) unread(store *cosl.Store, groupAlias string, c *ConversationSummary) error {
	id, err := conversationID(c.ID)
	if err != nil {
		return err
	}
	mark, err := readMark(h.p.ReadMarkFile(groupAlias, nodedir.ConversationName(id)))
	if err != nil {
		return err
	}
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}, GroupLevel: id == nil, ConversationID: id, AfterSeq: mark})
	if err != nil {
		return err
	}
	for _, row := range rows {
		m, err := talk.Decode(row, nil)
		if err != nil {
			return err
		}
		switch {
		case !m.Readable():
		case m.Handle != "" || m.Depth > 0:
			c.UnreadAutomated = true
		case row.Sender != h.pub.NodeID:
			c.Unread++
		}
	}
	return nil
}

// MarkRead marks conv's talk in groupAlias read up to seq on this Node.
// A mark never moves back: a seq at or before it changes nothing.
func (h *Host) MarkRead(_ context.Context, groupAlias, conv string, seq uint64) error {
	id, err := conversationID(conv)
	if err != nil {
		return err
	}
	if err := h.knownGroup(groupAlias); err != nil {
		return err
	}
	h.readMu.Lock()
	defer h.readMu.Unlock()
	path := h.p.ReadMarkFile(groupAlias, nodedir.ConversationName(id))
	mark, err := readMark(path)
	if err != nil || seq <= mark {
		return err
	}
	return writeMark(path, seq)
}

// readMark is the seq a read-mark file holds, 0 if there is none.
func readMark(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	seq, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("read mark %s: %w", path, err)
	}
	return seq, nil
}

// writeMark replaces path's read mark with seq, whole or not at all.
func writeMark(path string, seq uint64) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".read-mark.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	_, err = tmp.WriteString(strconv.FormatUint(seq, 10) + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return err
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
	h.seeRosters()
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

// Lean is conv under this Node's Recap: the Recap, then the talk after
// it.
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
	h.seeAgents(h.otherAgents(msgs))
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
		out[i] = ChatMessage{Message: h.message(e.Message), Blocks: e.Blocks, Chain: e.Chain, Summoned: e.Summoned}
	}
	return out, nil
}

func (h *Host) messages(ms talk.Messages) []Message {
	h.seeAgents(h.otherAgents(ms))
	out := make([]Message, len(ms))
	for i, m := range ms {
		out[i] = h.message(m)
	}
	return out
}

// otherAgents is the party key of each other Node's Agent that wrote one
// of ms, once each.
func (h *Host) otherAgents(ms []talk.Message) []string {
	var out []string
	for _, m := range ms {
		if m.Readable() && m.Handle != "" && m.Row.Sender != h.pub.NodeID {
			if p := NodeAgentParty(m.Row.Sender.String(), m.Handle); !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
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
	v, err := api.ConversationSendAs(ctx, m.Text, m.Handle, m.Depth, m.JobRun, id)
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

// Transcript is the runs of this Node's chain in conv whose id, the msg_id
// of the message that started it, starts with chain.
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
	ts, err := transcript.ReadConversation(h.p, groupAlias, id)
	if err != nil {
		return Transcript{}, err
	}
	full, err := serve.MatchChain(p, transcript.Chains(ts))
	if errors.Is(err, serve.ErrNotAChain) {
		return Transcript{}, h.chainNotFound(p, ChainPlace{Group: groupAlias, Conversation: conv})
	}
	if err != nil {
		return Transcript{}, err
	}
	lines := transcript.ChainLines(ts, full)
	out := Transcript{Chain: full, Lines: lines, Runs: transcript.Runs(lines)}
	for _, t := range ts {
		for _, l := range t.Lines {
			if !t.Live && l.Chain == full && !slices.Contains(out.Cleared, l.Step) {
				out.Cleared = append(out.Cleared, l.Step)
			}
		}
	}
	slices.Sort(out.Cleared)
	return out, nil
}

// Run is one run of one of this Node's Agents, as its Transcript shows it.
type Run = transcript.Run

// LiveRuns are the runs of the live Transcript of this Node's Agent
// handle in conv, in the order it ran them: what it remembers of its work
// there (ADR 0036); none if it has no live Transcript.
func (h *Host) LiveRuns(_ context.Context, groupAlias, conv, handle string) ([]Run, error) {
	id, err := conversationID(conv)
	if err != nil {
		return nil, err
	}
	if err := config.ValidateHandle(handle); err != nil {
		return nil, &InvalidError{Err: err}
	}
	if err := h.knownGroup(groupAlias); err != nil {
		return nil, err
	}
	lines, err := transcript.LiveLines(h.p, groupAlias, id, handle)
	if err != nil {
		return nil, err
	}
	return transcript.Runs(lines), nil
}

// UnpostedRuns are the runs of this Node's Agents in conv that posted
// nothing, stayed silent, failed or were cancelled, from their live and
// cleared Transcripts alike (transcript.Unposted).
func (h *Host) UnpostedRuns(_ context.Context, groupAlias, conv string) ([]Run, error) {
	id, err := conversationID(conv)
	if err != nil {
		return nil, err
	}
	if err := h.knownGroup(groupAlias); err != nil {
		return nil, err
	}
	ts, err := transcript.ReadConversation(h.p, groupAlias, id)
	if err != nil {
		return nil, err
	}
	return transcript.Unposted(ts), nil
}

// SignRuns are the runs of this Node's Agents in conv whose sign Tools
// acted on a Sign Request, from their live and cleared Transcripts alike
// (transcript.Acting).
func (h *Host) SignRuns(_ context.Context, groupAlias, conv string) ([]Run, error) {
	id, err := conversationID(conv)
	if err != nil {
		return nil, err
	}
	if err := h.knownGroup(groupAlias); err != nil {
		return nil, err
	}
	ts, err := transcript.ReadConversation(h.p, groupAlias, id)
	if err != nil {
		return nil, err
	}
	return transcript.Acting(ts), nil
}

// ChainPlace is a Conversation of a Group on this Node: its alias, and
// the hex Conversation ID, GroupTalk for the Group-level talk.
type ChainPlace struct {
	Group        string
	Conversation string
}

func (pl ChainPlace) String() string {
	if pl.Conversation == GroupTalk {
		return fmt.Sprintf("Group %q, Group-level talk", pl.Group)
	}
	return fmt.Sprintf("Group %q, Key Conversation %s", pl.Group, pl.Conversation)
}

// ChainAt is a chain with a run in Transcripts on this Node: its hex
// msg_id and the Conversation it is in.
type ChainAt struct {
	Chain string
	ChainPlace
}

// ChainNotFoundError is a Transcript lookup whose prefix names no chain
// with a run in the Conversation Searched. Elsewhere are the chains it
// names in this Node's other Conversations, in every Group, by Group alias
// then Conversation. It is serve.ErrNotAChain.
type ChainNotFoundError struct {
	Prefix    string // the hex msg_id prefix, lower case
	Searched  ChainPlace
	Elsewhere []ChainAt
}

// chainNotFound is the ChainNotFoundError of prefix in searched, with
// where else on this Node it names a chain. A Group whose Transcripts
// cannot be read is passed over: the lookup failed anyway.
func (h *Host) chainNotFound(prefix string, searched ChainPlace) *ChainNotFoundError {
	e := &ChainNotFoundError{Prefix: prefix, Searched: searched}
	aliases, _ := h.aliases()
	for _, alias := range aliases {
		convs, err := transcript.ReadByConversation(h.p, alias)
		if err != nil {
			continue
		}
		for _, c := range convs {
			pl := ChainPlace{Group: alias, Conversation: GroupTalk}
			if c.ID != nil {
				pl.Conversation = hex.EncodeToString(c.ID)
			}
			if pl == searched {
				continue
			}
			for _, chain := range transcript.Chains(c.Transcripts) {
				if strings.HasPrefix(chain, prefix) {
					e.Elsewhere = append(e.Elsewhere, ChainAt{Chain: chain, ChainPlace: pl})
				}
			}
		}
	}
	return e
}

func (e *ChainNotFoundError) Error() string {
	return fmt.Sprintf("no chain of this Node's in %s, starts with %s", e.Searched, e.Prefix)
}

func (e *ChainNotFoundError) Unwrap() error { return serve.ErrNotAChain }

// LiveTranscript is one of this Node's Agents with a live Transcript in a
// Conversation, and how large it has grown: Tokens are the input tokens of
// the last model call it records, 0 before its first (ADR 0036).
type LiveTranscript = transcript.Size

// LiveTranscripts are this Node's Agents with a live Transcript in conv,
// by Handle in order, with their sizes: those a Clear there would clear.
func (h *Host) LiveTranscripts(_ context.Context, groupAlias, conv string) ([]LiveTranscript, error) {
	id, err := conversationID(conv)
	if err != nil {
		return nil, err
	}
	if err := h.knownGroup(groupAlias); err != nil {
		return nil, err
	}
	return transcript.Sizes(h.p, groupAlias, id)
}

// ClearTranscripts clears the Agent handle in conv, or with handle ""
// every Agent there: it closes each one's live Transcript, kept and not
// deleted, and that Agent's next run there starts a new one (ADR 0036). A
// run still going finishes in the one closed. It returns the Handles it
// cleared, in order: none if no live Transcript was there. Nothing on the
// COSL changes, and it needs the Group neither served nor unlocked.
func (h *Host) ClearTranscripts(_ context.Context, groupAlias, conv, handle string) ([]string, error) {
	id, err := conversationID(conv)
	if err != nil {
		return nil, err
	}
	if handle != "" {
		if err := config.ValidateHandle(handle); err != nil {
			return nil, &InvalidError{Err: err}
		}
	}
	if err := h.knownGroup(groupAlias); err != nil {
		return nil, err
	}
	return transcript.Clear(h.p, groupAlias, id, handle)
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
