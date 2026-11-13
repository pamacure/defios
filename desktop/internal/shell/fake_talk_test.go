package shell_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"defios.com/internal/key"
	"defios.com/internal/mention"
	"defios.com/internal/node"
	"defios.com/internal/outbox"
	"defios.com/internal/serve"
	"defios.com/internal/session"
)

// The fake Node's talk: Conversations, their messages and Outbox, Keys and
// keygen Sessions, and which agents are included where. Its fields are
// guarded by fakeNode.mu.
type fakeTalk struct {
	committed map[string][]node.Message // by Conversation ID
	queued    map[string][]node.Message
	blocks    map[string][]json.RawMessage // a message's Transcript blocks, by msg_id
	keys      []node.Key
	keygens   []node.Session
	// included are the Handles included per Conversation; a Conversation
	// with none of its own follows GroupTalk's.
	included  map[string][]string
	profiles  []string
	providers map[string][]node.Provider
	networks  map[string][]node.Network
	noLeader  bool // Send queues
	noReplica bool // the Group has no replica here yet
	seq       uint64
	sent      []node.Outgoing
	cancelled []string // chains
	accepted  []string // Session IDs
	aborted   []string
	sign      *fakeSign
}

func (n *fakeNode) tk() *fakeTalk {
	if n.talk == nil {
		n.talk = &fakeTalk{
			committed: map[string][]node.Message{}, queued: map[string][]node.Message{},
			blocks: map[string][]json.RawMessage{}, included: map[string][]string{},
			providers: map[string][]node.Provider{}, networks: map[string][]node.Network{},
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
	out.Keygens = slices.Clone(t.keygens)
	return out, nil
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
			cm.Blocks = t.blocks[m.MsgID]
		}
		out = append(out, cm)
	}
	return out, nil
}

func (n *fakeNode) included(conv string) []string {
	t := n.tk()
	if l, ok := t.included[conv]; ok {
		return l
	}
	return t.included[node.GroupTalk]
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
	s := node.Session{SessionID: fmt.Sprintf("5e55%028d", len(t.keygens)+1), Status: session.Open, Curve: k.Curve, T: uint32(k.T), N: 2, ProposedAlias: k.Alias}
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
	k := node.Key{Key: s.Curve + ":" + pub, Alias: s.ProposedAlias, Curve: s.Curve, PublicKey: pub,
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

func (n *fakeNode) IncludeAgent(_ context.Context, group, conv, handle string) (node.Inclusion, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	l, own := t.included[conv]
	if !own {
		t.included[conv] = []string{handle}
		return node.Inclusion{Handles: []string{handle}, OwnList: conv != node.GroupTalk}, nil
	}
	if !slices.Contains(l, handle) {
		t.included[conv] = append(l, handle)
	}
	return node.Inclusion{Handles: slices.Clone(t.included[conv])}, nil
}

func (n *fakeNode) ExcludeAgent(_ context.Context, group, conv, handle string) (node.Inclusion, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.tk()
	l, own := t.included[conv]
	if !own && conv != node.GroupTalk {
		return node.Inclusion{}, node.ErrFollowsGroup
	}
	t.included[conv] = slices.DeleteFunc(slices.Clone(l), func(x string) bool { return x == handle })
	return node.Inclusion{Handles: slices.Clone(t.included[conv])}, nil
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
