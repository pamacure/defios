package shell_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"defios.com/internal/key"
	"defios.com/internal/mention"
	"defios.com/internal/node"
	"defios.com/internal/outbox"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/transcript"
)

// The fake Node's talk: Conversations, their messages and Outbox, Keys and
// keygen Sessions, and which agents are included where. Its fields are
// guarded by fakeNode.mu.
type fakeTalk struct {
	committed map[string][]node.Message // by Conversation ID
	queued    map[string][]node.Message
	keys      []node.Key
	keygens   []node.Session
	// included are the Handles included per Conversation; one with none
	// of its own has none.
	included map[string][]string
	// refuseInclusion, set, is what SetIncludedAgents fails with.
	refuseInclusion error
	profiles        []string
	providers       map[string][]node.Provider
	networks        map[string][]node.Network
	noLeader        bool // Send queues
	noReplica       bool // the Group has no replica here yet
	seq             uint64
	sent            []node.Outgoing
	cancelled       []string // chains
	accepted        []string // Session IDs
	aborted         []string
	sign            *fakeSign
	// liveTranscripts are the Handles with a live Transcript, by
	// Conversation ID.
	liveTranscripts map[string][]node.LiveTranscript
	// liveRuns are the runs of each live Transcript, by
	// "<Conversation ID>/<Handle>".
	liveRuns map[string][]node.Run
	// read is each Conversation's read mark.
	read map[string]uint64
	// liveries are the Liveries this Node has given, by party key.
	liveries map[string]string
	// jobRuns are this Node's Job Runs, oldest first: ID i at i-1.
	jobRuns []node.JobRun
}

func (n *fakeNode) tk() *fakeTalk {
	if n.talk == nil {
		n.talk = &fakeTalk{
			committed: map[string][]node.Message{}, queued: map[string][]node.Message{},
			included:  map[string][]string{},
			providers: map[string][]node.Provider{}, networks: map[string][]node.Network{},
			liveTranscripts: map[string][]node.LiveTranscript{},
			liveRuns:        map[string][]node.Run{},
			read:            map[string]uint64{},
			liveries:        map[string]string{},
		}
	}
	return n.talk
}

// emit publishes ev to every subscriber.
func (n *fakeNode) emit(ev node.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ch := range n.subs {
		ch <- ev
	}
}

// commit is m committed to conv by any Node, and its Message event.
func (n *fakeNode) commit(conv string, m node.Message) node.Message {
	n.mu.Lock()
	t := n.tk()
	t.seq++
	m.Seq, m.Term = t.seq, 1
	if m.MsgID == "" {
		m.MsgID = fmt.Sprintf("%064x", t.seq)
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	t.committed[conv] = append(t.committed[conv], m)
	n.mu.Unlock()
	n.emit(node.Event{Message: &node.MessageEvent{Group: n.servedGroup(), Conversation: conv, Message: m}})
	return m
}

func (n *fakeNode) servedGroup() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.served
}

// drain commits every queued message, as a leader coming back does.
func (n *fakeNode) drain() {
	n.mu.Lock()
	t := n.tk()
	t.noLeader = false
	queued := t.queued
	t.queued = map[string][]node.Message{}
	n.mu.Unlock()
	for conv, ms := range queued {
		for _, m := range ms {
			m.Queued = false
			n.commit(conv, m)
		}
	}
	n.emit(node.Event{Outbox: &node.OutboxEvent{Group: n.servedGroup()}})
}

func (n *fakeNode) Conversations(_ context.Context, group string) (node.Conversations, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	if t.noReplica {
		return node.Conversations{}, &node.NoReplicaError{Group: group}
	}
	out := node.Conversations{Talk: []node.ConversationSummary{{ID: node.GroupTalk}}}
	for i := range t.keys {
		k := t.keys[i]
		out.Talk = append(out.Talk, node.ConversationSummary{ID: k.ConversationID, Key: &k})
	}
	for i := range out.Talk {
		c := &out.Talk[i]
		for _, m := range t.committed[c.ID] {
			switch {
			case m.Seq <= t.read[c.ID] || !m.Readable() || m.Mine:
			case m.Handle != "" || m.Depth > 0:
				c.UnreadAutomated = true
			default:
				c.Unread++
			}
		}
	}
	out.Keygens = slices.Clone(t.keygens)
	return out, nil
}

func (n *fakeNode) MarkRead(_ context.Context, _, conv string, seq uint64) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	t.read[conv] = max(t.read[conv], seq)
	return nil
}

func (n *fakeNode) Messages(_ context.Context, group, conv string) ([]node.Message, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	if t.noReplica {
		return nil, &node.NoReplicaError{Group: group}
	}
	return append(slices.Clone(t.committed[conv]), t.queued[conv]...), nil
}

func (n *fakeNode) Chat(_ context.Context, group, conv string, limit int, transcripts bool) ([]node.ChatMessage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	var out []node.ChatMessage
	for _, m := range append(slices.Clone(t.committed[conv]), t.queued[conv]...) {
		if !m.Readable() {
			continue
		}
		cm := node.ChatMessage{Message: m}
		if transcripts {
			for chain, runs := range n.sg().runs {
				for _, r := range runs {
					if r.Outcome == transcript.OutcomeWrote && r.MsgID == m.MsgID {
						cm.Chain, cm.Summoned = chain, r.Summoned
					}
				}
			}
		}
		out = append(out, cm)
	}
	return out, nil
}

func (n *fakeNode) included(conv string) []string {
	t := n.tk()
	return t.included[conv]
}

func (n *fakeNode) Send(_ context.Context, group, conv string, m node.Outgoing) (node.Sent, error) {
	n.mu.Lock()
	if err := n.servedOrErr(group); err != nil {
		n.mu.Unlock()
		return node.Sent{}, err
	}
	t := n.tk()
	for _, h := range mention.Handles(m.Text) {
		if !slices.Contains(n.included(conv), h) {
			n.mu.Unlock()
			return node.Sent{}, fmt.Errorf("%w: @%s", serve.ErrBadHandle, h)
		}
	}
	t.sent = append(t.sent, m)
	msg := node.Message{NodeID: n.NodeID(), DisplayName: n.name, Mine: true, Text: m.Text, CreatedAt: time.Now()}
	if t.noLeader {
		t.seq++
		msg.MsgID, msg.Queued = fmt.Sprintf("%064x", 1000+t.seq), true
		t.queued[conv] = append(t.queued[conv], msg)
		n.mu.Unlock()
		n.emit(node.Event{Outbox: &node.OutboxEvent{Group: group}})
		return node.Sent{MsgID: msg.MsgID, Queued: true}, nil
	}
	n.mu.Unlock()
	c := n.commit(conv, msg)
	return node.Sent{Seq: c.Seq, MsgID: c.MsgID}, nil
}

func (n *fakeNode) Cancel(_ context.Context, group, conv, chain string) (node.Cancelled, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tk().cancelled = append(n.tk().cancelled, chain)
	return node.Cancelled{Cancelled: true, Chain: chain}, nil
}

func (n *fakeNode) Handles(_ context.Context, group, conv string) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.included(conv)), nil
}

func (n *fakeNode) DropQueued(_ context.Context, group, msgID string) (string, error) {
	n.mu.Lock()
	t := n.tk()
	for conv, ms := range t.queued {
		for i, m := range ms {
			if m.MsgID == msgID {
				t.queued[conv] = slices.Delete(ms, i, i+1)
				n.mu.Unlock()
				n.emit(node.Event{Outbox: &node.OutboxEvent{Group: group}})
				return msgID, nil
			}
		}
	}
	n.mu.Unlock()
	return "", outbox.ErrNotQueued
}

func (n *fakeNode) SetKeyNetworkType(_ context.Context, group, query, typ string) (node.Key, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	for i := range t.keys {
		if t.keys[i].ConversationID == query || t.keys[i].Key == query {
			t.keys[i].NetworkType = typ
			return t.keys[i], nil
		}
	}
	return node.Key{}, fmt.Errorf("no Key %s", query)
}

func (n *fakeNode) SetKeyAlias(_ context.Context, group, query, alias string) (node.Key, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	for i := range t.keys {
		if t.keys[i].ConversationID == query || t.keys[i].Key == query {
			for _, o := range t.keys {
				if alias != "" && o.Alias == alias && o.Key != t.keys[i].Key {
					return node.Key{}, &node.KeyAliasTakenError{Alias: alias, Key: o.Key}
				}
			}
			t.keys[i].Alias = alias
			return t.keys[i], nil
		}
	}
	return node.Key{}, key.ErrUnknown
}

func (n *fakeNode) StartKeygen(_ context.Context, group string, k node.Keygen) (node.Session, error) {
	n.mu.Lock()
	if err := n.servedOrErr(group); err != nil {
		n.mu.Unlock()
		return node.Session{}, err
	}
	if k.Curve != "ecdsa" && k.Curve != "eddsa" {
		n.mu.Unlock()
		return node.Session{}, session.ErrBadCurve
	}
	t := n.tk()
	s := node.Session{SessionID: fmt.Sprintf("5e55%028d", len(t.keygens)+1), Status: session.Open, Curve: k.Curve, T: uint32(k.T), N: 2, ProposedAlias: k.Alias, ProposedNetworkType: k.NetworkType}
	t.keygens = append(t.keygens, s)
	n.mu.Unlock()
	n.emit(node.Event{Session: &node.SessionEvent{Group: group, Session: s}})
	return s, nil
}

// mint completes keygen Session id into a Key, as its Session ending does.
func (n *fakeNode) mint(id string) node.Key {
	n.mu.Lock()
	t := n.tk()
	var s node.Session
	t.keygens = slices.DeleteFunc(t.keygens, func(x node.Session) bool {
		if x.SessionID == id {
			s = x
			return true
		}
		return false
	})
	pub := fmt.Sprintf("02%064x", len(t.keys)+1)
	k := node.Key{Key: s.Curve + ":" + pub, Alias: s.ProposedAlias, Curve: s.Curve, PublicKey: pub, Workspace: s.Curve + "-" + pub[:12],
		ConversationID: fmt.Sprintf("c0%062x", len(t.keys)+1), SessionID: id, T: int(s.T), N: int(s.N), Share: true}
	t.keys = append(t.keys, k)
	s.Status = session.Complete
	n.mu.Unlock()
	n.emit(node.Event{Session: &node.SessionEvent{Group: n.servedGroup(), Session: s}})
	return k
}

func (n *fakeNode) AcceptSession(_ context.Context, group, id, alias string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tk().accepted = append(n.tk().accepted, id)
	return nil
}

func (n *fakeNode) AbortSession(_ context.Context, group, id, reason string) error {
	n.mu.Lock()
	t := n.tk()
	t.aborted = append(t.aborted, id)
	var s node.Session
	t.keygens = slices.DeleteFunc(t.keygens, func(x node.Session) bool {
		if x.SessionID == id {
			s = x
			return true
		}
		return false
	})
	n.mu.Unlock()
	if s.SessionID == "" {
		return session.ErrUnknownSession
	}
	s.Status = session.Aborted
	n.emit(node.Event{Session: &node.SessionEvent{Group: group, Session: s}})
	return nil
}

func (n *fakeNode) AgentProfiles(context.Context) ([]node.AgentProfile, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []node.AgentProfile
	for _, h := range n.tk().profiles {
		out = append(out, node.AgentProfile{Handle: h})
	}
	return out, nil
}

func (n *fakeNode) IncludedAgents(_ context.Context, group, conv string) ([]string, error) {
	return n.Handles(context.Background(), group, conv)
}

func (n *fakeNode) SetIncludedAgents(_ context.Context, group, conv string, handles []string) (node.Inclusion, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	if t.refuseInclusion != nil {
		return node.Inclusion{}, t.refuseInclusion
	}
	t.included[conv] = slices.Clone(handles)
	return node.Inclusion{Handles: slices.Clone(handles)}, nil
}

func (n *fakeNode) Providers(_ context.Context, group, conv string) ([]node.Provider, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	if l, ok := t.providers[conv]; ok {
		return slices.Clone(l), nil
	}
	return slices.Clone(t.providers[node.GroupTalk]), nil
}

func (n *fakeNode) Networks(_ context.Context, group, conv string) ([]node.Network, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	if l, ok := t.networks[conv]; ok {
		return slices.Clone(l), nil
	}
	return slices.Clone(t.networks[node.GroupTalk]), nil
}

// live gives handles a live Transcript in conv, of no size yet.
func (n *fakeNode) live(conv string, handles ...string) {
	for _, h := range handles {
		n.sized(conv, h, 0)
	}
}

// sized gives handle a live Transcript in conv of tokens, or makes the one
// it has that size.
func (n *fakeNode) sized(conv, handle string, tokens int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	for i, l := range t.liveTranscripts[conv] {
		if l.Handle == handle {
			t.liveTranscripts[conv][i].Tokens = tokens
			return
		}
	}
	t.liveTranscripts[conv] = append(t.liveTranscripts[conv], node.LiveTranscript{Handle: handle, Tokens: tokens})
}

// liveIn are the Handles with a live Transcript in conv.
func (n *fakeNode) liveIn(conv string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, l := range n.tk().liveTranscripts[conv] {
		out = append(out, l.Handle)
	}
	return out
}

func (n *fakeNode) LiveTranscripts(_ context.Context, group, conv string) ([]node.LiveTranscript, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.tk().liveTranscripts[conv]), nil
}

func (n *fakeNode) LiveRuns(_ context.Context, group, conv, handle string) ([]node.Run, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.tk().liveRuns[conv+"/"+handle]), nil
}

// UnpostedRuns are the runs that posted nothing among this Node's chains'
// runs, cleared or not, by chain.
func (n *fakeNode) UnpostedRuns(_ context.Context, group, conv string) ([]node.Run, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []node.Run
	for _, chain := range slices.Sorted(maps.Keys(n.sg().runs)) {
		for _, r := range n.sg().runs[chain] {
			switch r.Outcome {
			case transcript.OutcomeSilent, transcript.OutcomeError, transcript.OutcomeCancelled:
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// SignRuns are the runs whose sign Tools acted among this Node's chains'
// runs, by chain.
func (n *fakeNode) SignRuns(_ context.Context, group, conv string) ([]node.Run, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []node.Run
	for _, chain := range slices.Sorted(maps.Keys(n.sg().runs)) {
		for _, r := range n.sg().runs[chain] {
			if len(r.Acted) > 0 {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func (n *fakeNode) ClearTranscripts(_ context.Context, group, conv, handle string) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	var cleared []string
	var left []node.LiveTranscript
	for _, l := range t.liveTranscripts[conv] {
		if handle == "" || l.Handle == handle {
			cleared = append(cleared, l.Handle)
		} else {
			left = append(left, l)
		}
	}
	t.liveTranscripts[conv] = left
	return cleared, nil
}

func (n *fakeNode) Liveries(context.Context) (map[string]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return maps.Clone(n.tk().liveries), nil
}

// SetLivery sets party's Livery as the Node does: none clears it, random
// gives a free one, and one another party holds is refused.
func (n *fakeNode) SetLivery(_ context.Context, party, livery string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	held := n.tk().liveries
	taken := map[string]string{}
	for p, l := range held {
		taken[l] = p
	}
	switch livery {
	case node.NoLivery:
		delete(held, party)
		return nil
	case node.RandomLivery:
		for _, c := range node.LiveryColours {
			for _, b := range node.LiveryBricks {
				if l := c + "-" + b; taken[l] == "" {
					held[party] = l
					return nil
				}
			}
		}
		return node.ErrNoFreeLivery
	}
	if p := taken[livery]; p != "" && p != party {
		return &node.LiveryTakenError{Livery: livery, Holder: p}
	}
	held[party] = livery
	return nil
}
