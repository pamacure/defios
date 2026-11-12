package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

// TalkNode is what the Talk page needs of a Node; *node.Host is one. Every
// call names the served Group explicitly.
type TalkNode interface {
	Conversations(ctx context.Context, group string) (node.Conversations, error)
	Messages(ctx context.Context, group, conv string) ([]node.Message, error)
	Chat(ctx context.Context, group, conv string, limit int, transcripts bool) ([]node.ChatMessage, error)
	Send(ctx context.Context, group, conv string, m node.Outgoing) (node.Sent, error)
	Cancel(ctx context.Context, group, conv, chain string) (node.Cancelled, error)
	Handles(ctx context.Context, group, conv string) ([]string, error)
	DropQueued(ctx context.Context, group, msgID string) (string, error)
	SetKeyAlias(ctx context.Context, group, query, alias string) (node.Key, error)
	StartKeygen(ctx context.Context, group string, k node.Keygen) (node.Session, error)
	AcceptSession(ctx context.Context, group, id, alias string) error
	AbortSession(ctx context.Context, group, id, reason string) error
	AgentProfiles(ctx context.Context) ([]node.AgentProfile, error)
	IncludedAgents(ctx context.Context, group, conv string) ([]string, error)
	IncludeAgent(ctx context.Context, group, conv, handle string) (node.Inclusion, error)
	ExcludeAgent(ctx context.Context, group, conv, handle string) (node.Inclusion, error)
	Providers(ctx context.Context, group, conv string) ([]node.Provider, error)
	Networks(ctx context.Context, group, conv string) ([]node.Network, error)
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

	// The header: the Roster and its reachability.
	Roster            []RosterNode
	Online            int
	MajorityReachable bool

	// The transcript: what was said, in order, with system lines among
	// it; then the chains of agents running now.
	Items []Item
	Live  []LiveChain

	// Handles are the agents' Handles a message here may mention.
	Handles []string

	// The right column: the open Conversation's Key, nil for Group talk;
	// every Agent Profile and whether it is included here; and the
	// Providers and Networks this Conversation overrides.
	Key       *KeyDetail
	Agents    []Agent
	Overrides []Override

	// Refused is why the composer's message was not sent; it keeps what
	// was typed.
	Refused *Problem
	// Problem is what went wrong last, elsewhere on the page.
	Problem *Problem
}

// Conversation is one row of the left column.
type Conversation struct {
	ID string // node.GroupTalk for Group talk
	// Alias is the Key's alias here, "" for none; Curve and ShortKey name
	// it otherwise. All "" for Group talk.
	Alias, Curve, ShortKey string
	T, N                   int
	// OpenSignRequests counts its Sign Requests not yet settled.
	OpenSignRequests int
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

// ItemKind is what a line of the transcript is.
type ItemKind string

const (
	KindMessage ItemKind = "message"
	// KindPruned is a placeholder for a message whose payload is gone or
	// unreadable.
	KindPruned ItemKind = "pruned"
	KindSystem ItemKind = "system"
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
	At    time.Time
	Side  Side // "" for a system line
	// From is the device that wrote it, "This Device" for this one; ""
	// on the right. Handle is the agent's, and Job marks a Job's.
	From   string
	Handle string
	Job    bool
	Text   string
	// Queued waits in this device's Outbox; Droppable can be dropped
	// from it, being this person's.
	Queued, Droppable bool
	// Transcript: this device ran the chain that said it, and Transcript
	// gives its run.
	Transcript bool
}

// LiveChain is a chain of agents running here now.
type LiveChain struct {
	Chain  string // the msg_id that started it
	Agents []LiveAgent
}

// LiveAgent is one agent's output in a chain as it generates it.
type LiveAgent struct {
	Handle string
	Depth  uint32
	Blocks []LiveBlock
}

// LiveBlock is a run of output of one kind: thinking, text or tool.
type LiveBlock struct {
	Kind, Text string
}

// KeyDetail is a Key as the right column shows it.
type KeyDetail struct {
	Key, Alias, Curve, PublicKey, Address string
	T, N                                  int
	Share                                 bool // this device holds a share
	// Reachable of its devices are reachable now; CanSign once t are.
	Reachable int
	CanSign   bool
}

// Agent is an Agent Profile of this device, and whether it answers here.
type Agent struct {
	Handle   string
	Included bool
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
	n      TalkNode
	self   string
	group  string
	gen    uint64 // bumped each time the served Group changes
	open   string
	seq    uint64

	convs  node.Conversations
	status node.Status
	msgs   []node.Message
	said   map[string][]json.RawMessage // Transcript blocks by msg_id
	// saidStale: a chain or Job ended, or a Conversation opened, so said
	// is read again with the messages. Reading it expands every
	// Transcript there, so it is not read on every message.
	saidStale bool
	handles   []string
	agents    []Agent
	over      []Override
	system    map[string][]Item // by Conversation
	live      map[string][]*LiveChain
	accepted  map[string]bool   // keygen Sessions this device accepted
	signed    map[string]string // each Sign Request's last status seen
	refused   *Problem
	problem   *Problem // an action's
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
		t.convs, t.msgs, t.said, t.saidStale = node.Conversations{}, nil, nil, true
		t.handles, t.agents, t.over = nil, nil, nil
		t.system, t.live = map[string][]Item{}, map[string][]*LiveChain{}
		t.accepted, t.signed = map[string]bool{}, map[string]string{}
		t.refused, t.problem = nil, nil
		t.failed = map[string]*Problem{}
	}
	t.mu.Unlock()
	t.emit()
	if !same {
		go t.loadAll()
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

// loadOpen reads the open Conversation: its talk and its right column.
func (t *talk) loadOpen() {
	t.loadMessages()
	t.loadSide()
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
	withSaid := t.saidStale
	t.mu.Unlock()
	t.loadMu.Lock()
	msgs, err := a.n.Messages(t.ctx, a.group, a.conv)
	var chat []node.ChatMessage
	if err == nil && withSaid {
		chat, err = a.n.Chat(t.ctx, a.group, a.conv, 0, true)
	}
	t.loadMu.Unlock()
	t.mu.Lock()
	if !t.still(a) {
		t.mu.Unlock()
		return
	}
	if t.read("messages", err) {
		t.msgs = msgs
		if withSaid {
			t.said, t.saidStale = map[string][]json.RawMessage{}, false
			for _, m := range chat {
				if len(m.Blocks) > 0 {
					t.said[m.MsgID] = m.Blocks
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
	handles, agents, over, err := t.readSide(a)
	t.loadMu.Unlock()
	t.mu.Lock()
	if !t.still(a) {
		t.mu.Unlock()
		return
	}
	if t.read("side", err) {
		t.handles, t.agents, t.over = handles, agents, over
	}
	t.mu.Unlock()
	t.emit()
}

func (t *talk) readSide(a at) ([]string, []Agent, []Override, error) {
	handles, err := a.n.Handles(t.ctx, a.group, a.conv)
	if err != nil {
		return nil, nil, nil, err
	}
	included, err := a.n.IncludedAgents(t.ctx, a.group, a.conv)
	if err != nil {
		return nil, nil, nil, err
	}
	profiles, err := a.n.AgentProfiles(t.ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	agents := make([]Agent, 0, len(profiles))
	for _, p := range profiles {
		agents = append(agents, Agent{Handle: p.Handle, Included: slices.Contains(included, p.Handle)})
	}
	over, err := t.overrides(a)
	return handles, agents, over, err
}

// overrides are the Providers and Networks in force in a's Conversation
// that are not the Group's own.
func (t *talk) overrides(a at) ([]Override, error) {
	if a.conv == node.GroupTalk {
		return nil, nil
	}
	var out []Override
	here, err := a.n.Providers(t.ctx, a.group, a.conv)
	if err != nil {
		return nil, err
	}
	base, err := a.n.Providers(t.ctx, a.group, node.GroupTalk)
	if err != nil {
		return nil, err
	}
	for _, p := range here {
		if !slices.ContainsFunc(base, func(b node.Provider) bool { return reflect.DeepEqual(b, p) }) {
			out = append(out, Override{Kind: OverrideProvider, Alias: p.Alias, Detail: p.URL})
		}
	}
	nets, err := a.n.Networks(t.ctx, a.group, a.conv)
	if err != nil {
		return nil, err
	}
	baseNets, err := a.n.Networks(t.ctx, a.group, node.GroupTalk)
	if err != nil {
		return nil, err
	}
	for _, nw := range nets {
		if !slices.ContainsFunc(baseNets, func(b node.Network) bool { return reflect.DeepEqual(b, nw) }) {
			out = append(out, Override{Kind: OverrideNetwork, Alias: nw.Alias, Detail: nw.Chain})
		}
	}
	return out, nil
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
	st := TalkState{Seq: t.seq, Group: t.group}
	if t.group == "" {
		return st
	}
	st.Open, st.Refused, st.Problem = t.open, t.refused, t.problem
	for _, what := range []string{"conversations", "messages", "side"} {
		if st.Problem == nil {
			st.Problem = t.failed[what]
		}
	}
	st.Handles = slices.Clone(t.handles)
	st.Agents = slices.Clone(t.agents)
	st.Overrides = slices.Clone(t.over)
	st.MajorityReachable = t.status.MajorityReachable
	for _, r := range t.status.Roster {
		st.Roster = append(st.Roster, RosterNode{ID: r.NodeID, Name: r.DisplayName, Reachable: r.Reachable, Self: r.NodeID == t.self})
		if r.Reachable {
			st.Online++
		}
	}
	for _, c := range t.convs.Talk {
		row := Conversation{ID: c.ID, OpenSignRequests: c.OpenSignRequests}
		if k := c.Key; k != nil {
			row.Alias, row.Curve, row.ShortKey, row.T, row.N = k.Alias, k.Curve, shortKey(k.PublicKey), k.T, k.N
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
		lc := LiveChain{Chain: c.Chain}
		for _, a := range c.Agents {
			lc.Agents = append(lc.Agents, LiveAgent{Handle: a.Handle, Depth: a.Depth, Blocks: slices.Clone(a.Blocks)})
		}
		st.Live = append(st.Live, lc)
	}
	return st
}

func (t *talk) keyDetail(k node.Key) *KeyDetail {
	d := &KeyDetail{Key: k.Key, Alias: k.Alias, Curve: k.Curve, PublicKey: k.PublicKey, Address: k.Address, T: k.T, N: k.N, Share: k.Share}
	for _, r := range t.status.Keys {
		if r.Key == k.Key {
			d.Reachable, d.CanSign = r.Reachable, r.CanSign
		}
	}
	return d
}

// items are the open Conversation's messages in log order, queued ones
// last, with its system lines merged in by time; t.mu is held.
func (t *talk) items() []Item {
	sys := t.system[t.open]
	out := make([]Item, 0, len(t.msgs)+len(sys))
	i := 0
	for _, m := range t.msgs {
		it := t.item(m)
		for i < len(sys) && sys[i].At.Before(it.At) {
			out = append(out, sys[i])
			i++
		}
		out = append(out, it)
	}
	for ; i < len(sys); i++ {
		out = append(out, sys[i])
	}
	return out
}

// item is m as the transcript shows it: on the right only if it is this
// person's own, else on the left, labelled with its device and its agent
// or Job.
func (t *talk) item(m node.Message) Item {
	it := Item{Kind: KindMessage, MsgID: m.MsgID, At: m.CreatedAt, Text: m.Text, Queued: m.Queued}
	if !m.Readable() {
		it.Kind = KindPruned
	}
	if m.Mine {
		it.Side, it.Droppable = Right, m.Queued
		return it
	}
	it.Side, it.Handle, it.Job = Left, m.Handle, m.Readable() && m.Handle == "" && m.Depth > 0
	it.From = m.DisplayName
	switch {
	case m.NodeID == t.self:
		it.From = "This Device"
	case it.From == "":
		it.From = shortKey(m.NodeID)
	}
	_, it.Transcript = t.said[m.MsgID]
	return it
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
		}
	case ev.Outbox != nil && ev.Outbox.Group == group:
		reload = t.loadMessages
	case ev.Session != nil && ev.Session.Group == group:
		t.session(ev.Session.Session)
		reload = t.loadConversations
	case ev.SignRequest != nil && ev.SignRequest.Group == group:
		t.signRequest(ev.SignRequest.Request)
		reload = t.loadConversations
	case ev.Chain != nil && ev.Chain.Group == group:
		if t.chain(ev.Chain.Conversation, ev.Chain.Event) && ev.Chain.Conversation == open {
			t.saidStale = true
			reload = t.loadMessages
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
		go reload()
	}
}

// chain applies a chain event in conv, and reports whether the chain
// ended, so what it said and its Transcript are to be read; t.mu is held.
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
	case serve.EventAgentCommit, serve.EventAgentSilent:
		done()
	case serve.EventAgentError:
		done()
		t.say(conv, fmt.Sprintf("@%s failed: %s", ev.Handle, ev.Error))
	case serve.EventAgentSkipped:
		t.say(conv, fmt.Sprintf("@%s was skipped: %s", ev.Handle, ev.Reason))
	case serve.EventChainEnd:
		t.live[conv] = slices.DeleteFunc(t.live[conv], func(c *LiveChain) bool { return c.Chain == ev.Chain })
		switch {
		case ev.Error != "":
			t.say(conv, "The agents stopped: "+ev.Error)
		case ev.Cancelled:
			t.say(conv, "The agents were cancelled.")
		}
		return true
	case serve.EventTriggerFire:
		what := "its schedule"
		if ev.Event != nil && ev.Event.Type != "" {
			what = ev.Event.Type
		}
		t.say(conv, fmt.Sprintf("Trigger %s fired on %s.", ev.Trigger, what))
	case serve.EventJobEnd:
		if ev.OK != nil && *ev.OK {
			t.say(conv, fmt.Sprintf("Job %s finished.", ev.Job))
		} else {
			t.say(conv, fmt.Sprintf("Job %s failed: %s", ev.Job, ev.Error))
		}
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

// signRequest tells the Sign Request's Conversation that its status
// changed; t.mu is held.
func (t *talk) signRequest(r signreq.Summary) {
	if t.signed[r.MsgID] == r.Status {
		return
	}
	t.signed[r.MsgID] = r.Status
	what := "Sign Request"
	if r.Reason != "" {
		what += fmt.Sprintf(" “%s”", r.Reason)
	}
	t.say(r.Conversation, fmt.Sprintf("%s is %s.", what, r.Status))
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
