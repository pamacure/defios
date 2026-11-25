package shell

import (
	"cmp"
	"context"
	"encoding/hex"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"defios.com/internal/base58"
	"defios.com/internal/config"
	"defios.com/internal/jobrun"
	"defios.com/internal/key"
	"defios.com/internal/mention"
	"defios.com/internal/network"
	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/transcript"
	"defios.com/internal/trigger"
)

// TalkNode is what the Talk page needs of a Node; *node.Host is one. Every
// call names the served Group explicitly.
type TalkNode interface {
	Conversations(ctx context.Context, group string) (node.Conversations, error)
	Messages(ctx context.Context, group, conv string) ([]node.Message, error)
	MarkRead(ctx context.Context, group, conv string, seq uint64) error
	Chat(ctx context.Context, group, conv string, limit int, transcripts bool) ([]node.ChatMessage, error)
	Send(ctx context.Context, group, conv string, m node.Outgoing) (node.Sent, error)
	Cancel(ctx context.Context, group, conv, chain string) (node.Cancelled, error)
	Handles(ctx context.Context, group, conv string) ([]string, error)
	DropQueued(ctx context.Context, group, msgID string) (string, error)
	SetKeyAlias(ctx context.Context, group, query, alias string) (node.Key, error)
	SetKeyNetworkType(ctx context.Context, group, query, typ string) (node.Key, error)
	StartKeygen(ctx context.Context, group string, k node.Keygen) (node.Session, error)
	AcceptSession(ctx context.Context, group, id, alias string) error
	AbortSession(ctx context.Context, group, id, reason string) error
	AgentProfiles(ctx context.Context) ([]node.AgentProfile, error)
	IncludedAgents(ctx context.Context, group, conv string) ([]string, error)
	SetIncludedAgents(ctx context.Context, group, conv string, handles []string) (node.Inclusion, error)
	Providers(ctx context.Context, group, conv string) ([]node.Provider, error)
	Networks(ctx context.Context, group, conv string) ([]node.Network, error)
	Transcript(ctx context.Context, group, conv, chain string) (node.Transcript, error)
	LiveTranscripts(ctx context.Context, group, conv string) ([]node.LiveTranscript, error)
	LiveRuns(ctx context.Context, group, conv, handle string) ([]node.Run, error)
	UnpostedRuns(ctx context.Context, group, conv string) ([]node.Run, error)
	SignRuns(ctx context.Context, group, conv string) ([]node.Run, error)
	JobRuns(ctx context.Context, group, job string, last int) ([]node.JobRun, error)
	JobRun(ctx context.Context, group string, id uint64) (node.JobRun, error)
	ClearTranscripts(ctx context.Context, group, conv, handle string) ([]string, error)
	Liveries(ctx context.Context) (map[string]string, error)
	SetLivery(ctx context.Context, party, livery string) error

	// Sign Requests.
	SignRequests(ctx context.Context, group string) ([]node.SignRequest, error)
	InspectSignRequest(ctx context.Context, group, msgID string) (node.SignRequestDetail, error)
	TransferTx(ctx context.Context, group, keyQuery string, tr node.Transfer) (node.TxSpec, error)
	FillTransactions(ctx context.Context, group, keyQuery string, txs []node.TxSpec) (node.Filled, error)
	ProposeSignRequest(ctx context.Context, group, keyQuery string, spec node.SignSpec, jobRun string) (node.Sent, error)
	AcceptSignRequest(ctx context.Context, group, msgID, jobRun string) (node.Answered, error)
	RefuseSignRequest(ctx context.Context, group, msgID, reason, jobRun string) (node.Answered, error)
	BroadcastSignRequest(ctx context.Context, group, msgID string) (node.Broadcast, error)
}

// TalkState is everything the Talk page renders, for the served Group.
type TalkState struct {
	// Seq orders TalkStates, as State.Seq does States.
	Seq uint64
	// Group is the served Group; "" while none is, and the page is empty.
	Group string
	// Open is the Conversation shown: node.GroupTalk or a Key's
	// Conversation ID.
	Open string

	// The left column: Group talk first, then one per Key; and the keygen
	// Sessions in progress, which become Keys.
	Conversations []Conversation
	Keygens       []Keygen

	// The header: the Roster and its reachability. Online counts the
	// reachable Roster Nodes; MajorityReachable is whether writes can
	// commit, as node.Status has it, which needs more than devices online.
	Roster            []RosterNode
	Online            int
	MajorityReachable bool
	// Devices are the Roster as Devices here lists it: this device first,
	// then the others by name.
	Devices []Device

	// The transcript: what was said and the Sign Requests proposed, in
	// order, with system lines among them; then the chains of agents
	// running now.
	Items []Item
	Live  []LiveChain
	// Focus is the Sign Request to select, as it was proposed here or a
	// notification about it was clicked; "" for none.
	Focus string

	// Liveries are every Livery this Node has given, by party key, for
	// the picker to grey out those another party holds.
	Liveries map[string]string

	// Handles are the agents' Handles a message here may mention.
	Handles []Handle
	// Transcripts are this device's agents with a live Transcript here,
	// and how large each has grown, shown beside Clear: a Transcript only
	// grows, so its size says when to clear it (ADR 0036).
	Transcripts []TranscriptSize

	// The right column: the open Conversation's Key, nil for Group talk;
	// every Agent Profile and whether it is included here; and the
	// Providers and Networks this Conversation overrides.
	// The open Key's Sign Requests are listed too.
	Key          *KeyDetail
	SignRequests []SignRow
	Agents       []Agent
	Overrides    []Override

	// Refused is why the composer's message was not sent; it keeps what
	// was typed. Problem is why another action failed. Each is on the
	// TalkState that action returns only, never kept for the next: it
	// happened once, and the page shows it once.
	Refused *Problem
	Problem *Problem
	// Unread is why reading the page last failed, kept while it does.
	Unread *Problem
}

// Conversation is one row of the left column.
type Conversation struct {
	ID string // node.GroupTalk for Group talk
	// Alias is the Key's alias here, "" for none; Curve and ShortKey name
	// it otherwise. All "" for Group talk.
	Alias, Curve, ShortKey string
	// NetworkType is what this device uses the Key for, which picks its
	// icon: bitcoin, ethereum, solana, or "" for an ECDSA Key not yet set.
	NetworkType string
	T, N        int
	// OpenSignRequests counts its Sign Requests not yet settled.
	OpenSignRequests int
	// Unread counts what people on other devices said here past this
	// Node's read mark; UnreadAutomated is whether agents or Jobs said
	// anything. The open Conversation shows neither: it is being read.
	Unread          int
	UnreadAutomated bool
}

// Keygen is a keygen Session in progress: a provisional row.
type Keygen struct {
	SessionID string
	Curve     string
	T, N      int
	Alias     string // the Key alias it proposes, "" for none
	// Accepted: this device has accepted it, so its engine may start.
	Accepted bool
}

// RosterNode is one device of the Group.
type RosterNode struct {
	ID, Name  string
	Reachable bool
	Self      bool
}

// Device is one device of the Roster in Devices here. This device is
// "This Device", with no Party, Livery or Agents: its agents are under
// Agents here. Another's Agents are those seen writing in the open
// Conversation, in the order they first did.
type Device struct {
	Party, Name, Livery string
	Self                bool
	Agents              []Party
}

// Party is one party of talk a Livery marks (ADR 0037): Party is its key,
// as node.SetLivery takes it, and Livery "" for none.
type Party struct {
	Party, Handle, Livery string
}

// ItemKind is what a line of the transcript is.
type ItemKind string

const (
	KindMessage ItemKind = "message"
	// KindUnreadable is a placeholder for a message whose payload does
	// not read. A pruned message is left out.
	KindUnreadable ItemKind = "unreadable"
	KindSystem     ItemKind = "system"
	// KindRun is a run of this device's agent that posted nothing: it
	// stayed silent, failed or was cancelled. It shows as a message from
	// that agent, faded, read from its Transcript.
	KindRun ItemKind = "run"
	// KindJob is a Trigger of this device's firing, a Job run by hand on
	// it, or a Job it ran ending, read from its Job Run (ADR 0040).
	KindJob ItemKind = "job"
)

// Side is where a message sits in the transcript.
type Side string

const (
	// Right is this person's own messages only.
	Right Side = "right"
	// Left is everything else: other devices' people and agents, and this
	// device's own agents and Jobs.
	Left Side = "left"
)

// Item is one line of the transcript.
type Item struct {
	Kind  ItemKind
	MsgID string
	// Seq is where a committed message sits in the log; 0 for one queued
	// and for a system line. Reading up to it marks it read (ReadTo).
	Seq  uint64
	At   time.Time
	Side Side // "" for a system line
	// From is the device that wrote it, "This Device" for this one; ""
	// on the right. Handle is the agent's, and Job marks a Job's.
	From   string
	Handle string
	Job    bool
	Text   string
	// Queued waits in this device's Outbox; Droppable can be dropped
	// from it, being this person's.
	Queued, Droppable bool
	// Livery is the sender's, "" for none: on the left only, and never a
	// Job's, which is no party (ADR 0037).
	Livery string
	// Party is the sender's party key, to set its Livery by; "" for this
	// person's and a Job's.
	Party string
	// Parts are Text split at its mentions, each marked with the Livery
	// of the agent it names on the device that sent it.
	Parts []Part
	// A run of left-side messages from one sender shows its author line
	// on the first, Author, and its avatar on the last, Avatar. Another
	// sender, a line that is no left-side message, or a gap of more than
	// runGap ends a run.
	Author, Avatar bool
	sender         string // who wrote it, for runs; "" off the left
	// Chain is the chain of this device's agent's run that said it, or
	// with KindRun, of that run, which Step and Outcome name in it. The
	// panel's Details give the run (MessageDetails, RunDetails).
	Chain   string
	Step    int
	Outcome string
	// Summoned, on a message or KindRun of this device's agent, are the
	// agents its run summoned, in order (ADR 0041).
	Summoned []Party
	// JobRun is the ID of the Job Run a KindJob line is about: for a
	// firing, its first Job's.
	JobRun uint64
	// Sign is a Sign Request's card, for KindSign.
	Sign *SignCard
}

// Part is a run of a message's text: plain, or one mention as written,
// with the Handle it names and that agent's Livery, "" for none.
type Part struct {
	Text, Handle, Livery string
}

// TranscriptSize is one agent's live Transcript in the open Conversation:
// Tokens are the input tokens of the last model call it records, about
// what its next run replays; 0 before its first.
type TranscriptSize struct {
	Handle string
	Tokens int64
}

// Handle is one of this device's agents a message here may mention, and
// its Livery, "" for none.
type Handle struct {
	Handle, Livery string
}

// LiveChain is a chain of agents running here now.
type LiveChain struct {
	Chain  string // the msg_id that started it
	Agents []LiveAgent
}

// LiveAgent is one agent's output in a chain as it generates it.
type LiveAgent struct {
	Handle string
	Party  string // node.AgentParty(Handle)
	Livery string // "" for none
	Depth  uint32
	Blocks []LiveBlock
	// Summoned are the agents its run has summoned so far, in order
	// (ADR 0041).
	Summoned []Party
}

// LiveBlock is a run of output of one kind: thinking, text or tool.
type LiveBlock struct {
	Kind, Text string
}

// KeyDetail is a Key as the right column shows it.
type KeyDetail struct {
	Key, Alias, Curve, PublicKey string
	NetworkType                  string // as Conversation's
	// Addresses are where it funds and spends for its Network type: one
	// for Ethereum or Solana, one per bitcoin network of this
	// Conversation's Networks for Bitcoin (mainnet with none), and both
	// Ethereum's and Bitcoin's for an ECDSA Key whose type is unset.
	Addresses []KeyAddress
	// NetworkTypes are those its curve allows, for choosing among.
	NetworkTypes []string
	T, N         int
	Share        bool // this device holds a share
	// Reachable of its devices are reachable now; CanSign once t are.
	Reachable int
	CanSign   bool
}

// KeyAddress is one of a Key's addresses and what it is: "Ethereum
// address", "Bitcoin testnet address".
type KeyAddress struct{ Label, Address string }

// keyAddresses are k's KeyDetail.Addresses, its Bitcoin ones on nets.
// An address that cannot be made (a malformed public key) is left out.
func keyAddresses(k node.Key, nets []string) []KeyAddress {
	pub, _ := hex.DecodeString(k.PublicKey)
	var out []KeyAddress
	add := func(label string, addr string, err error) {
		if err == nil && addr != "" {
			out = append(out, KeyAddress{Label: label, Address: addr})
		}
	}
	switch {
	case k.Curve == "eddsa":
		add("Solana address", base58.Encode(pub), nil)
	case k.NetworkType == network.Ethereum || k.NetworkType == "":
		addr, err := key.EthereumAddress(pub)
		add("Ethereum address", addr, err)
	}
	if k.Curve == "ecdsa" && k.NetworkType != network.Ethereum {
		if len(nets) == 0 {
			nets = []string{"mainnet"}
		}
		for _, net := range nets {
			label := "Bitcoin address"
			if net != "mainnet" {
				label = "Bitcoin " + net + " address"
			}
			addr, err := signreq.BitcoinAddress(net, pub)
			add(label, addr, err)
		}
	}
	return out
}

// Agent is an Agent Profile of this device, its party key and Livery,
// "" for none, and whether it answers here.
type Agent struct {
	Handle, Party, Livery string
	Included              bool
}

// OverrideKind is what an Override sets.
type OverrideKind string

const (
	OverrideProvider OverrideKind = "provider"
	OverrideNetwork  OverrideKind = "network"
)

// Override is a Provider or Network this Conversation sets over the
// Group's.
type Override struct {
	Kind  OverrideKind
	Alias string
	// Detail says what it points at: a Provider's URL, a Network's chain.
	Detail string
}

// talk is the Talk page's model: the served Group's Conversations, the one
// open and what happens in it, kept current from the Node's events.
type talk struct {
	ctx      context.Context
	onChange func(TalkState)

	// loadMu serializes reads from the Node, so the last read applied is
	// the latest; mu guards the fields below and is never held while the
	// Node works.
	loadMu sync.Mutex
	mu     sync.Mutex
	// work is the reads started off the caller's goroutine, which the
	// Shell's Close waits out.
	work  sync.WaitGroup
	n     TalkNode
	self  string
	group string
	gen   uint64 // bumped each time the served Group changes
	open  string
	seq   uint64

	convs  node.Conversations
	status node.Status
	msgs   []node.Message
	said   map[string]string // by msg_id, the chain of this device's run that said it
	// summoned are, by msg_id, the Handles the run that said it summoned.
	summoned map[string][]string
	// unposted are this device's runs here that posted nothing.
	unposted []node.Run
	// jobRuns are this device's Job Runs in the Group, newest first.
	jobRuns []node.JobRun
	// saidStale: a run, chain or Job ended, a Trigger fired, or a
	// Conversation opened, so said, unposted and jobRuns are read again
	// with the messages. Reading them reads every Transcript there and
	// every Job Run, so they are not read on every message.
	saidStale bool
	// saidVer counts each time said went stale, so a read that began
	// before the last time leaves it stale for the next.
	saidVer uint64
	// liveries are this Node's, by party key (ADR 0037).
	liveries map[string]string
	handles  []string
	sizes    []node.LiveTranscript
	agents   []Agent
	over     []Override
	// bitcoinNets are the bitcoin networks of the open Conversation's
	// Networks, which a Bitcoin Key's addresses are on.
	bitcoinNets []string
	system      map[string][]Item // by Conversation
	live        map[string][]*LiveChain
	accepted    map[string]bool // keygen Sessions this device accepted
	starting    int             // StartKeygen calls under way
	// reqs are the Group's Sign Requests, in log order; cards are the
	// open Conversation's, decoded, by msg_id.
	reqs  []node.SignRequest
	cards map[string]node.SignRequestDetail
	focus string
	// signVer counts what changed reqs or cards since they were read
	// whole, so a read that overlapped one is read again, not applied.
	signVer uint64
	// proposed is, by Sign Request msg_id, the run of this device's agent
	// or the Job Run of its exec Job that proposed it, read with said.
	proposed map[string]SignCause
	// failed is why each read last failed, by what it reads; while one
	// has, the next status change reads everything again, as a Group just
	// formed has no replica until Raft starts.
	failed map[string]*Problem
}

func newTalk(ctx context.Context, onChange func(TalkState)) *talk {
	if onChange == nil {
		onChange = func(TalkState) {}
	}
	return &talk{ctx: ctx, onChange: onChange}
}

// serve shows group's talk, served by n, opening Group talk if group is
// not the one shown. self is this Node's ID.
func (t *talk) serve(n TalkNode, self, group string, st node.Status) {
	t.mu.Lock()
	same := t.n == n && t.group == group
	t.status = st
	if !same {
		t.n, t.self, t.group, t.open = n, self, group, node.GroupTalk
		t.gen++
		t.convs, t.msgs, t.said, t.summoned, t.unposted, t.jobRuns, t.saidStale, t.liveries = node.Conversations{}, nil, nil, nil, nil, nil, true, nil
		t.saidVer++
		t.handles, t.sizes, t.agents, t.over, t.bitcoinNets = nil, nil, nil, nil, nil
		t.system, t.live = map[string][]Item{}, map[string][]*LiveChain{}
		t.accepted, t.proposed = map[string]bool{}, nil
		t.reqs, t.cards, t.focus = nil, map[string]node.SignRequestDetail{}, ""
		t.failed = map[string]*Problem{}
	}
	t.mu.Unlock()
	t.emit()
	if !same {
		t.work.Go(t.loadAll)
	}
}

// stop empties the page, as serving stops.
func (t *talk) stop() {
	t.mu.Lock()
	if t.group == "" {
		t.mu.Unlock()
		return
	}
	t.n, t.group = nil, ""
	t.gen++
	t.mu.Unlock()
	t.emit()
}

// at is what a load reads from: the Node, Group and Conversation shown now,
// and the generation, to check nothing changed before applying it.
type at struct {
	n     TalkNode
	group string
	conv  string
	gen   uint64
}

func (t *talk) now() (at, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return at{t.n, t.group, t.open, t.gen}, t.n != nil
}

// still reports whether a is still what is shown; t.mu is held.
func (t *talk) still(a at) bool { return t.gen == a.gen && t.open == a.conv }

func (t *talk) loadAll() {
	t.loadConversations()
	t.loadOpen()
}

// loadConversations reads the left column again.
func (t *talk) loadConversations() {
	a, ok := t.now()
	if !ok {
		return
	}
	t.loadMu.Lock()
	convs, err := a.n.Conversations(t.ctx, a.group)
	t.loadMu.Unlock()
	t.mu.Lock()
	if t.gen != a.gen {
		t.mu.Unlock()
		return
	}
	if t.read("conversations", err) {
		t.convs = convs
	}
	t.mu.Unlock()
	t.emit()
}

// read records how reading what failed, and reports whether it did not;
// t.mu is held.
func (t *talk) read(what string, err error) bool {
	if err != nil {
		t.failed[what] = problemOf(err)
		return false
	}
	delete(t.failed, what)
	return true
}

// loadOpen reads the open Conversation: its talk, its Sign Requests and
// its right column.
func (t *talk) loadOpen() {
	t.loadMessages()
	t.loadSigns()
	t.loadSide()
	t.loadSizes()
}

// loadSizes reads the open Conversation's live Transcripts and their
// sizes: as it opens, as a chain or Job there ends, and after a Clear.
func (t *talk) loadSizes() {
	a, ok := t.now()
	if !ok {
		return
	}
	t.loadMu.Lock()
	sizes, err := a.n.LiveTranscripts(t.ctx, a.group, a.conv)
	t.loadMu.Unlock()
	t.mu.Lock()
	if !t.still(a) {
		t.mu.Unlock()
		return
	}
	if t.read("sizes", err) {
		t.sizes = sizes
	}
	t.mu.Unlock()
	t.emit()
}

// loadSigns reads the Group's Sign Requests again, and the open
// Conversation's decoded for their cards.
func (t *talk) loadSigns() {
	a, ok := t.now()
	if !ok {
		return
	}
	t.mu.Lock()
	ver := t.signVer
	t.mu.Unlock()
	t.loadMu.Lock()
	reqs, err := a.n.SignRequests(t.ctx, a.group)
	cards := map[string]node.SignRequestDetail{}
	for _, r := range reqs {
		if err != nil {
			break
		}
		if r.Conversation == a.conv {
			cards[r.MsgID], err = a.n.InspectSignRequest(t.ctx, a.group, r.MsgID)
		}
	}
	t.loadMu.Unlock()
	t.mu.Lock()
	if !t.still(a) {
		t.mu.Unlock()
		return
	}
	if t.signVer != ver {
		// An event or a card read came since: what was read may be older.
		t.mu.Unlock()
		t.work.Go(t.loadSigns)
		return
	}
	if t.read("signs", err) {
		t.reqs, t.cards = reqs, cards
	}
	t.mu.Unlock()
	t.emit()
}

// loadCard reads Sign Request msgID's card again.
func (t *talk) loadCard(msgID string) {
	a, ok := t.now()
	if !ok {
		return
	}
	t.loadMu.Lock()
	d, err := a.n.InspectSignRequest(t.ctx, a.group, msgID)
	t.loadMu.Unlock()
	t.mu.Lock()
	if !t.still(a) {
		t.mu.Unlock()
		return
	}
	if t.read("signs", err) {
		t.cards[msgID] = d
		t.signVer++
	}
	t.mu.Unlock()
	t.emit()
}

// loadMessages reads the open Conversation's talk again, queued messages
// included, and which messages have a Transcript when that may have
// changed.
func (t *talk) loadMessages() {
	a, ok := t.now()
	if !ok {
		return
	}
	t.mu.Lock()
	withSaid, ver := t.saidStale, t.saidVer
	t.mu.Unlock()
	t.loadMu.Lock()
	msgs, err := a.n.Messages(t.ctx, a.group, a.conv)
	var chat []node.ChatMessage
	var unposted []node.Run
	var jobRuns []node.JobRun
	var signRuns []node.Run
	if err == nil && withSaid {
		chat, err = a.n.Chat(t.ctx, a.group, a.conv, 0, true)
		if err == nil {
			unposted, err = a.n.UnpostedRuns(t.ctx, a.group, a.conv)
		}
		if err == nil {
			signRuns, err = a.n.SignRuns(t.ctx, a.group, a.conv)
		}
		if err == nil {
			jobRuns, err = a.n.JobRuns(t.ctx, a.group, "", 0)
		}
	}
	// The Node gives another device's agent a Livery as its first
	// message arrives, so they are read with the messages.
	liveries, lerr := a.n.Liveries(t.ctx)
	t.loadMu.Unlock()
	t.mu.Lock()
	if !t.still(a) {
		t.mu.Unlock()
		return
	}
	t.setLiveries(liveries, lerr)
	if t.read("messages", err) {
		t.msgs = msgs
		if withSaid {
			t.said, t.summoned, t.unposted, t.jobRuns, t.saidStale = map[string]string{}, map[string][]string{}, unposted, jobRuns, t.saidVer != ver
			for _, m := range chat {
				if m.Chain != "" {
					t.said[m.MsgID] = m.Chain
				}
				if len(m.Summoned) > 0 {
					t.summoned[m.MsgID] = m.Summoned
				}
			}
			t.proposed = map[string]SignCause{}
			for _, r := range signRuns {
				for _, ac := range r.Acted {
					if ac.Tool == config.ToolSignPropose {
						t.proposed[ac.MsgID] = SignCause{Chain: r.Chain, Step: r.Step, MsgID: r.MsgID}
					}
				}
			}
			for _, r := range jobRuns {
				for _, sg := range r.Signs {
					if sg.Act == jobrun.Propose {
						t.proposed[sg.MsgID] = SignCause{JobRun: r.ID}
					}
				}
			}
		}
	}
	t.mu.Unlock()
	t.emit()
}

// loadSide reads the open Conversation's Handles, agents and overrides.
func (t *talk) loadSide() {
	a, ok := t.now()
	if !ok {
		return
	}
	t.loadMu.Lock()
	sd, err := t.readSide(a)
	// An agent added here was given a Livery.
	liveries, lerr := a.n.Liveries(t.ctx)
	t.loadMu.Unlock()
	t.mu.Lock()
	if !t.still(a) {
		t.mu.Unlock()
		return
	}
	t.setLiveries(liveries, lerr)
	if t.read("side", err) {
		t.handles, t.agents, t.over, t.bitcoinNets = sd.handles, sd.agents, sd.over, sd.bitcoinNets
	}
	t.mu.Unlock()
	t.emit()
}

// loadLiveries reads the Liveries again, as one was set.
func (t *talk) loadLiveries() {
	a, ok := t.now()
	if !ok {
		return
	}
	t.loadMu.Lock()
	liveries, err := a.n.Liveries(t.ctx)
	t.loadMu.Unlock()
	t.mu.Lock()
	if t.gen == a.gen {
		t.setLiveries(liveries, err)
	}
	t.mu.Unlock()
	t.emit()
}

// setLiveries keeps the Liveries just read; t.mu is held. Liveries only
// mark talk, so one that fails to read keeps those read before rather
// than stop the page.
func (t *talk) setLiveries(l map[string]string, err error) {
	if err == nil {
		t.liveries = l
	}
}

// side is what the open Conversation's side columns show beside its talk.
type side struct {
	handles     []string
	agents      []Agent
	over        []Override
	bitcoinNets []string
}

func (t *talk) readSide(a at) (side, error) {
	var sd side
	var err error
	if sd.handles, err = a.n.Handles(t.ctx, a.group, a.conv); err != nil {
		return side{}, err
	}
	included, err := a.n.IncludedAgents(t.ctx, a.group, a.conv)
	if err != nil {
		return side{}, err
	}
	profiles, err := a.n.AgentProfiles(t.ctx)
	if err != nil {
		return side{}, err
	}
	sd.agents = make([]Agent, 0, len(profiles))
	for _, p := range profiles {
		sd.agents = append(sd.agents, Agent{Handle: p.Handle, Party: node.AgentParty(p.Handle), Included: slices.Contains(included, p.Handle)})
	}
	sd.over, sd.bitcoinNets, err = t.overrides(a)
	return sd, err
}

// overrides are the Providers and Networks in force in a's Conversation
// that are not the Group's own, and the bitcoin networks of all its
// Networks in force.
func (t *talk) overrides(a at) ([]Override, []string, error) {
	if a.conv == node.GroupTalk {
		return nil, nil, nil
	}
	var out []Override
	here, err := a.n.Providers(t.ctx, a.group, a.conv)
	if err != nil {
		return nil, nil, err
	}
	base, err := a.n.Providers(t.ctx, a.group, node.GroupTalk)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range here {
		if !slices.ContainsFunc(base, func(b node.Provider) bool { return reflect.DeepEqual(b, p) }) {
			out = append(out, Override{Kind: OverrideProvider, Alias: p.Alias, Detail: p.URL})
		}
	}
	nets, err := a.n.Networks(t.ctx, a.group, a.conv)
	if err != nil {
		return nil, nil, err
	}
	baseNets, err := a.n.Networks(t.ctx, a.group, node.GroupTalk)
	if err != nil {
		return nil, nil, err
	}
	var btc []string
	for _, nw := range nets {
		if !slices.ContainsFunc(baseNets, func(b node.Network) bool { return reflect.DeepEqual(b, nw) }) {
			out = append(out, Override{Kind: OverrideNetwork, Alias: nw.Alias, Detail: nw.Chain})
		}
		if nw.Type == network.Bitcoin && !slices.Contains(btc, nw.BitcoinNetwork) {
			btc = append(btc, nw.BitcoinNetwork)
		}
	}
	return out, btc, nil
}

// State is the Talk page now.
func (t *talk) State() TalkState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot()
}

func (t *talk) emit() {
	t.mu.Lock()
	t.seq++
	st := t.snapshot()
	t.mu.Unlock()
	t.onChange(st)
}

// snapshot is the TalkState; t.mu is held.
func (t *talk) snapshot() TalkState {
	// Lists are never nil: the page iterates them, and nil is JSON null.
	st := TalkState{
		Seq: t.seq, Group: t.group,
		Conversations: []Conversation{}, Keygens: []Keygen{}, Roster: []RosterNode{}, Devices: []Device{},
		Live: []LiveChain{}, Handles: []Handle{}, Transcripts: []TranscriptSize{}, Agents: []Agent{}, Overrides: []Override{},
		SignRequests: []SignRow{},
	}
	if t.group == "" {
		return st
	}
	st.Open, st.Focus = t.open, t.focus
	st.SignRequests = t.signRows()
	for _, what := range []string{"conversations", "messages", "signs", "side", "sizes"} {
		if st.Unread == nil {
			st.Unread = t.failed[what]
		}
	}
	for _, hd := range t.handles {
		st.Handles = append(st.Handles, Handle{Handle: hd, Livery: t.liveries[node.AgentParty(hd)]})
	}
	for _, l := range t.sizes {
		st.Transcripts = append(st.Transcripts, TranscriptSize{Handle: l.Handle, Tokens: l.Tokens})
	}
	for _, a := range t.agents {
		a.Livery = t.liveries[a.Party]
		st.Agents = append(st.Agents, a)
	}
	st.Liveries = maps.Clone(t.liveries)
	if st.Liveries == nil {
		st.Liveries = map[string]string{}
	}
	st.Overrides = append(st.Overrides, t.over...)
	st.MajorityReachable = t.status.MajorityReachable
	for _, r := range t.status.Roster {
		st.Roster = append(st.Roster, RosterNode{ID: r.NodeID, Name: r.DisplayName, Reachable: r.Reachable, Self: r.NodeID == t.self})
		if r.Reachable {
			st.Online++
		}
	}
	st.Devices = t.devices()
	for _, c := range t.convs.Talk {
		row := Conversation{ID: c.ID, OpenSignRequests: c.OpenSignRequests}
		if c.ID != t.open {
			row.Unread, row.UnreadAutomated = c.Unread, c.UnreadAutomated
		}
		if k := c.Key; k != nil {
			row.Alias, row.Curve, row.ShortKey, row.T, row.N = k.Alias, k.Curve, shortKey(k.PublicKey), k.T, k.N
			row.NetworkType = k.NetworkType
			if c.ID == t.open {
				st.Key = t.keyDetail(*k)
			}
		}
		st.Conversations = append(st.Conversations, row)
	}
	for _, s := range t.convs.Keygens {
		st.Keygens = append(st.Keygens, Keygen{
			SessionID: s.SessionID, Curve: s.Curve, T: int(s.T), N: int(s.N), Alias: s.ProposedAlias,
			Accepted: t.accepted[s.SessionID],
		})
	}
	st.Items = t.items()
	for _, c := range t.live[t.open] {
		lc := LiveChain{Chain: c.Chain, Agents: []LiveAgent{}}
		for _, a := range c.Agents {
			var summoned []string
			for _, p := range a.Summoned {
				summoned = append(summoned, p.Handle)
			}
			lc.Agents = append(lc.Agents, LiveAgent{
				Handle: a.Handle, Party: t.party(t.self, a.Handle), Livery: t.liveries[t.party(t.self, a.Handle)], Depth: a.Depth, Blocks: append([]LiveBlock{}, a.Blocks...),
				Summoned: t.summons(summoned),
			})
		}
		st.Live = append(st.Live, lc)
	}
	return st
}

// devices are the Roster for Devices here; t.mu is held.
func (t *talk) devices() []Device {
	seen := map[string][]Party{} // each device's agents writing here
	for _, m := range t.msgs {
		if m.NodeID == t.self || m.Handle == "" || !m.Readable() {
			continue
		}
		p := t.party(m.NodeID, m.Handle)
		if !slices.ContainsFunc(seen[m.NodeID], func(a Party) bool { return a.Party == p }) {
			seen[m.NodeID] = append(seen[m.NodeID], Party{Party: p, Handle: m.Handle, Livery: t.liveries[p]})
		}
	}
	var out []Device
	self := false
	for _, r := range t.status.Roster {
		if r.NodeID == t.self {
			self = true
			continue
		}
		p := t.party(r.NodeID, "")
		out = append(out, Device{Party: p, Name: t.device(r.NodeID), Livery: t.liveries[p], Agents: append([]Party{}, seen[r.NodeID]...)})
	}
	slices.SortStableFunc(out, func(a, b Device) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	if self {
		out = append([]Device{{Name: t.device(t.self), Self: true, Agents: []Party{}}}, out...)
	}
	if out == nil {
		out = []Device{}
	}
	return out
}

func (t *talk) keyDetail(k node.Key) *KeyDetail {
	d := &KeyDetail{Key: k.Key, Alias: k.Alias, Curve: k.Curve, PublicKey: k.PublicKey, NetworkType: k.NetworkType, T: k.T, N: k.N, Share: k.Share}
	d.Addresses = keyAddresses(k, t.bitcoinNets)
	for _, typ := range network.Types() {
		if (network.Chain{Type: typ}).Curve() == k.Curve {
			d.NetworkTypes = append(d.NetworkTypes, typ)
		}
	}
	for _, r := range t.status.Keys {
		if r.Key == k.Key {
			d.Reachable, d.CanSign = r.Reachable, r.CanSign
		}
	}
	return d
}

// items are the open Conversation's messages and Sign Requests in log
// order, queued messages last, with this device's runs that posted
// nothing placed among them (placeRuns) and its system and Job lines
// merged in by time; t.mu is held.
func (t *talk) items() []Item {
	// The lines of no message, by time.
	timed := append(t.jobLines(), t.system[t.open]...)
	slices.SortStableFunc(timed, func(a, b Item) int { return a.At.Compare(b.At) })
	var cards []node.SignRequestDetail
	for _, r := range t.reqs {
		if d, ok := t.cards[r.MsgID]; ok && r.Conversation == t.open {
			cards = append(cards, d)
		}
	}
	logged := make([]Item, 0, len(t.msgs)+len(cards))
	for _, m := range t.msgs {
		if m.Pruned {
			continue
		}
		for len(cards) > 0 && (m.Queued || cards[0].Seq < m.Seq) {
			logged = append(logged, t.signItem(cards[0]))
			cards = cards[1:]
		}
		logged = append(logged, t.item(m))
	}
	for _, d := range cards {
		logged = append(logged, t.signItem(d))
	}
	logged = t.placeRuns(logged)
	out := make([]Item, 0, len(logged)+len(timed))
	i := 0
	for _, it := range logged {
		for i < len(timed) && timed[i].At.Before(it.At) {
			out = append(out, timed[i])
			i++
		}
		out = append(out, it)
	}
	for ; i < len(timed); i++ {
		out = append(out, timed[i])
	}
	runs(out)
	return out
}

// jobLines are the open Conversation's lines from this device's Job
// Runs, those of a Trigger firing on what was said here, or for Group
// talk on a Schedule too, and of a Job run by hand on it, or for Group
// talk on nothing: each firing once, as its first Job Run, each Job run
// by hand once, and each Job that ended. A Job never run, or running
// still, has no line of its ending; t.mu is held.
func (t *talk) jobLines() []Item {
	var out []Item
	for _, r := range slices.Backward(t.jobRuns) {
		if r.Event.ConversationID != t.open {
			continue
		}
		switch {
		case r.ByHand:
			out = append(out, Item{Kind: KindJob, JobRun: r.ID, At: r.Fired, Text: fmt.Sprintf("Job %s run by hand.", r.Job)})
		case r.ID == r.Firing:
			what := r.Event.Type
			if what == "" || what == trigger.ScheduleEvent {
				what = "its schedule"
			}
			out = append(out, Item{Kind: KindJob, JobRun: r.ID, At: r.Fired, Text: fmt.Sprintf("Trigger %s fired on %s.", r.Trigger, what)})
		}
		at := r.Ended
		if at.IsZero() {
			at = r.Started // ended by a serve stopping first
		}
		switch r.State {
		case node.JobOK:
			out = append(out, Item{Kind: KindJob, JobRun: r.ID, At: at, Text: fmt.Sprintf("Job %s finished.", r.Job)})
		case node.JobFailed:
			out = append(out, Item{Kind: KindJob, JobRun: r.ID, At: at, Text: fmt.Sprintf("Job %s failed: %s", r.Job, r.Error)})
		}
	}
	return out
}

// unpostedText is what a run that posted nothing shows, by its outcome:
// a failure's error is only in its details.
var unpostedText = map[string]string{
	transcript.OutcomeSilent:    "Finished without replying.",
	transcript.OutcomeError:     "Failed.",
	transcript.OutcomeCancelled: "Cancelled.",
}

// placeRuns is logged, a Conversation's items, with the open
// Conversation's runs of this device's agents that posted nothing placed
// among them, each before the first item after it ended; t.mu is held.
func (t *talk) placeRuns(logged []Item) []Item {
	if len(t.unposted) == 0 {
		return logged
	}
	type placed struct {
		at  int // how many logged items come before it
		run Item
	}
	var runs []placed
	for _, r := range t.unposted {
		at := len(logged)
		if i := slices.IndexFunc(logged, func(l Item) bool { return r.Ended.Before(l.At) }); i >= 0 {
			at = i
		}
		runs = append(runs, placed{at, t.runItem(r)})
	}
	slices.SortStableFunc(runs, func(a, b placed) int {
		if c := cmp.Compare(a.at, b.at); c != 0 {
			return c
		}
		return a.run.At.Compare(b.run.At)
	})
	out := make([]Item, 0, len(logged)+len(runs))
	for i, l := range logged {
		for len(runs) > 0 && runs[0].at == i {
			out = append(out, runs[0].run)
			runs = runs[1:]
		}
		out = append(out, l)
	}
	for _, p := range runs {
		out = append(out, p.run)
	}
	return out
}

// runItem is r, a run of this device's agent that posted nothing, as a
// message from that agent; t.mu is held.
func (t *talk) runItem(r node.Run) Item {
	party := t.party(t.self, r.Handle)
	return Item{
		Kind: KindRun, At: r.Ended, Side: Left, From: "This Device", Handle: r.Handle,
		Text: unpostedText[r.Outcome], Party: party, Livery: t.liveries[party],
		sender: sender(t.self, r.Handle, false),
		Chain:  r.Chain, Step: r.Step, Outcome: r.Outcome,
		Summoned: t.summons(r.Summoned),
	}
}

// summons are handles, the agents of this device a run summoned, as
// parties in their Liveries; t.mu is held.
func (t *talk) summons(handles []string) []Party {
	out := []Party{}
	for _, h := range handles {
		p := t.party(t.self, h)
		out = append(out, Party{Party: p, Handle: h, Livery: t.liveries[p]})
	}
	return out
}

// sender names who wrote a left-side line, for runs of one sender's.
func sender(nodeID, handle string, job bool) string {
	return fmt.Sprintf("%s/%s/%v", nodeID, handle, job)
}

// runGap is the longest pause within a run of one sender's messages.
const runGap = 5 * time.Minute

// runs marks where each run of one sender's messages in items starts and
// ends.
func runs(items []Item) {
	joins := func(a, b *Item) bool { return a.sender == b.sender && b.At.Sub(a.At) <= runGap }
	for i := range items {
		it := &items[i]
		if it.sender == "" {
			continue
		}
		it.Author = i == 0 || !joins(&items[i-1], it)
		it.Avatar = i == len(items)-1 || !joins(it, &items[i+1])
	}
}

// item is m as the transcript shows it: on the right only if it is this
// person's own, else on the left, labelled with its device and its agent
// or Job.
func (t *talk) item(m node.Message) Item {
	it := Item{Kind: KindMessage, MsgID: m.MsgID, Seq: m.Seq, At: m.CreatedAt, Text: m.Text, Queued: m.Queued}
	if !m.Readable() {
		it.Kind = KindUnreadable
	} else {
		it.Parts = t.parts(m)
	}
	if m.Mine {
		it.Side, it.Droppable = Right, m.Queued
		return it
	}
	it.Side, it.Handle, it.Job = Left, m.Handle, m.Readable() && m.Handle == "" && m.Depth > 0
	if !it.Job {
		it.Party = t.party(m.NodeID, m.Handle)
		it.Livery = t.liveries[it.Party]
	}
	it.sender = sender(m.NodeID, m.Handle, it.Job)
	it.From = m.DisplayName
	switch {
	case m.NodeID == t.self:
		it.From = "This Device"
	case it.From == "":
		it.From = shortKey(m.NodeID)
	}
	it.Chain = t.said[m.MsgID]
	it.Summoned = t.summons(t.summoned[m.MsgID])
	return it
}

// party is the party key of nodeID's agent handle, or with no handle of
// the Node nodeID: its person.
func (t *talk) party(nodeID, handle string) string {
	switch {
	case nodeID == t.self && handle == "":
		return "" // this person, who is no party
	case nodeID == t.self:
		return node.AgentParty(handle)
	case handle == "":
		return node.NodeParty(nodeID)
	}
	return node.NodeAgentParty(nodeID, handle)
}

// parts are m's text split at its mentions, each resolved against the
// device that sent m; t.mu is held.
func (t *talk) parts(m node.Message) []Part {
	sender := m.NodeID
	if m.Mine {
		sender = t.self // queued, it may name none
	}
	var out []Part
	at := 0
	for _, sp := range mention.Spans(m.Text) {
		if sp.Start > at {
			out = append(out, Part{Text: m.Text[at:sp.Start]})
		}
		out = append(out, Part{Text: m.Text[sp.Start:sp.End], Handle: sp.Handle, Livery: t.liveries[t.party(sender, sp.Handle)]})
		at = sp.End
	}
	if at < len(m.Text) || len(out) == 0 {
		out = append(out, Part{Text: m.Text[at:]})
	}
	return out
}

// shortKey is a hex key or ID shortened for a label.
func shortKey(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:6] + "…" + h[len(h)-4:]
}

// event applies one of n's events to the page, if n serves its Group.
// It never waits on the Node: what it must read again, it reads off this
// goroutine.
func (t *talk) event(n TalkNode, ev node.Event) {
	t.mu.Lock()
	if t.n != n || t.group == "" {
		t.mu.Unlock()
		return
	}
	group, open := t.group, t.open
	var reload func()
	switch {
	case ev.Status != nil:
		t.status = *ev.Status
	case ev.Message != nil && ev.Message.Group == group:
		if ev.Message.Conversation == open {
			reload = t.loadMessages
		} else if !ev.Message.Message.Mine {
			reload = t.loadConversations // its row's unread
		}
	case ev.Outbox != nil && ev.Outbox.Group == group:
		reload = t.loadMessages
	case ev.Session != nil && ev.Session.Group == group:
		t.session(ev.Session.Session)
		reload = t.loadConversations
	case ev.KeyAlias != nil && ev.KeyAlias.Group == group:
		reload = t.loadConversations
	case ev.SignRequest != nil && ev.SignRequest.Group == group:
		r := ev.SignRequest.Request
		t.setRequest(r)
		reload = t.loadConversations
		if r.Conversation == open {
			reload = func() {
				t.loadCard(r.MsgID)
				t.loadConversations()
			}
		}
	case ev.HookRun != nil && ev.HookRun.Group == group:
		t.setRun(ev.HookRun.Run)
	case ev.Chain != nil && ev.Chain.Group == group:
		if t.chain(ev.Chain.Conversation, ev.Chain.Event) && ev.Chain.Conversation == open {
			t.saidStale = true
			t.saidVer++
			reload = func() {
				t.loadMessages()
				t.loadSizes()
			}
		}
	default:
		t.mu.Unlock()
		return
	}
	if len(t.failed) > 0 {
		reload = t.loadAll
	}
	t.mu.Unlock()
	t.emit()
	if reload != nil {
		t.work.Go(reload)
	}
}

// chain applies a chain event in conv, and reports whether a run or the
// chain ended, or a Trigger fired, a Job was run by hand or a Job ended,
// so what it said, its Transcript and the Job Runs are to be read; t.mu
// is held.
func (t *talk) chain(conv string, ev node.ChainEvent) (ended bool) {
	find := func() *LiveChain {
		for _, c := range t.live[conv] {
			if c.Chain == ev.Chain {
				return c
			}
		}
		// Joined mid-chain, as when the app opens while one runs.
		c := &LiveChain{Chain: ev.Chain}
		t.live[conv] = append(t.live[conv], c)
		return c
	}
	agent := func() *LiveAgent {
		c := find()
		if len(c.Agents) == 0 {
			c.Agents = append(c.Agents, LiveAgent{Handle: ev.Handle, Depth: ev.Depth})
		}
		return &c.Agents[len(c.Agents)-1]
	}
	// done takes the agent that ended out of the live chain: what it said
	// shows as a message of its own. An event that names no Handle is the
	// one answering now, the last to start.
	done := func() {
		c := find()
		i := len(c.Agents) - 1
		if ev.Handle != "" {
			i = slices.IndexFunc(c.Agents, func(a LiveAgent) bool { return a.Handle == ev.Handle })
		}
		if i >= 0 {
			c.Agents = slices.Delete(c.Agents, i, i+1)
		}
	}
	switch ev.Type {
	case serve.EventChainStart:
		find()
	case serve.EventAgentStart:
		c := find()
		c.Agents = append(c.Agents, LiveAgent{Handle: ev.Handle, Depth: ev.Depth})
	case serve.EventAgentDelta:
		a := agent()
		if n := len(a.Blocks); n > 0 && a.Blocks[n-1].Kind == ev.Kind {
			a.Blocks[n-1].Text += ev.Text
		} else {
			a.Blocks = append(a.Blocks, LiveBlock{Kind: ev.Kind, Text: ev.Text})
		}
	case serve.EventAgentReset:
		agent().Blocks = nil
	case serve.EventAgentCommit:
		// Its summons stay on what it said: the Transcript, read again
		// once the chain ends, has them too.
		if a := agent(); len(a.Summoned) > 0 && ev.MsgID != "" {
			if t.summoned == nil {
				t.summoned = map[string][]string{}
			}
			for _, p := range a.Summoned {
				t.summoned[ev.MsgID] = append(t.summoned[ev.MsgID], p.Handle)
			}
		}
		done()
	case serve.EventAgentSilent, serve.EventAgentError:
		done()
		// Its run posted nothing: its Transcript, read again, shows it
		// as a message from it.
		return true
	case serve.EventAgentSummoned:
		// It goes on the latest run of its summoner, which is still
		// answering: a summon is accepted mid-run.
		c := find()
		for i := len(c.Agents) - 1; i >= 0; i-- {
			if c.Agents[i].Handle == ev.By {
				c.Agents[i].Summoned = append(c.Agents[i].Summoned, Party{Handle: ev.Handle})
				break
			}
		}
	case serve.EventAgentSkipped:
		t.say(conv, fmt.Sprintf("@%s was skipped: %s", ev.Handle, ev.Reason))
	case serve.EventAgentTranscript:
		t.say(conv, fmt.Sprintf("@%s starts a new Transcript: %s.", ev.Handle, ev.Reason))
	case serve.EventChainEnd:
		t.live[conv] = slices.DeleteFunc(t.live[conv], func(c *LiveChain) bool { return c.Chain == ev.Chain })
		// A cancelled run shows from its Transcript, read again.
		if ev.Error != "" {
			t.say(conv, "The agents stopped: "+ev.Error)
		}
		return true
	case serve.EventTriggerFire, serve.EventJobRun, serve.EventJobEnd:
		// Its Job Run, kept before it was sent, is read again.
		return true
	}
	return false
}

// say adds a system line to conv; t.mu is held.
func (t *talk) say(conv, text string) {
	t.system[conv] = append(t.system[conv], Item{Kind: KindSystem, At: time.Now(), Text: text})
}

// Session's moves show as its Sign Request's. t.mu is held.
func (t *talk) session(sum node.Session) {
	if sum.SignRequest != "" {
		return
	}
	what := fmt.Sprintf("%s, %d of %d", curveName(sum.Curve), sum.T, sum.N)
	if sum.ProposedAlias != "" {
		what = sum.ProposedAlias + " (" + what + ")"
	}
	switch sum.Status {
	case session.Open:
		t.say(node.GroupTalk, "A new Key is being made: "+what+".")
	case session.Complete:
		t.say(node.GroupTalk, "New Key ready: "+what+".")
	case session.Aborted:
		t.say(node.GroupTalk, "Making the Key "+what+" was aborted.")
	}
}

func curveName(c string) string {
	switch c {
	case "ecdsa":
		return "ECDSA"
	case "eddsa":
		return "EdDSA"
	}
	return c
}
