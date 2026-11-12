package shell_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/signreq"
	"defios.com/internal/trigger"
)

// eventuallyTalk waits for the Talk page's state to satisfy ok.
func eventuallyTalk(t *testing.T, s *shell.Shell, ok func(shell.TalkState) bool) shell.TalkState {
	t.Helper()
	return talkWithin(t, s, 3*time.Second, ok)
}

// talkWithin is eventuallyTalk waiting up to d, as real Nodes need.
func talkWithin(t *testing.T, s *shell.Shell, d time.Duration, ok func(shell.TalkState) bool) shell.TalkState {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		ts := s.Talk()
		if ok(ts) {
			return ts
		}
		if time.Now().After(deadline) {
			t.Fatalf("talk never matched; last: %+v", ts)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func items(n int) func(shell.TalkState) bool {
	return func(ts shell.TalkState) bool { return len(ts.Items) == n }
}

// served is a Shell serving alpha over r's fake Node.
func served(t *testing.T, r *rig) *shell.Shell {
	t.Helper()
	s := r.start(false)
	if st := s.Unlock("alpha", pw); st.View != shell.ViewServed {
		t.Fatalf("unlock: %+v", st)
	}
	return s
}

func TestOnlyThisPersonsOwnMessagesAreOnTheRight(t *testing.T) {
	r := newRig(t, "alpha")
	self := r.node.NodeID()
	for _, m := range []node.Message{
		{NodeID: self, DisplayName: "laptop", Mine: true, Text: "hello"},
		{NodeID: self, DisplayName: "laptop", Handle: "auditor", Depth: 1, Text: "looks fine"},
		{NodeID: self, DisplayName: "laptop", Depth: 1, Text: "job output"},
		{NodeID: "b0b", DisplayName: "bob", Text: "hi from bob"},
		{NodeID: "b0b", DisplayName: "bob", Handle: "scribe", Depth: 1, Text: "noted"},
	} {
		r.node.commit(node.GroupTalk, m)
	}
	ts := eventuallyTalk(t, served(t, r), items(5))

	want := []struct {
		side         shell.Side
		from, handle string
		job          bool
	}{
		{"right", "", "", false},
		{"left", "This Device", "auditor", false},
		{"left", "This Device", "", true},
		{"left", "bob", "", false},
		{"left", "bob", "scribe", false},
	}
	for i, w := range want {
		it := ts.Items[i]
		if it.Side != w.side || it.From != w.from || it.Handle != w.handle || it.Job != w.job {
			t.Errorf("item %d %q: side %s from %q handle %q job %v; want %+v", i, it.Text, it.Side, it.From, it.Handle, it.Job, w)
		}
	}
	if ts.Group != "alpha" || ts.Open != node.GroupTalk {
		t.Fatalf("open %q in %q", ts.Open, ts.Group)
	}
}

func TestAMessageCommittedWhileOpenShowsWithoutPolling(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Conversations) == 1 })
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "hi"})
	ts := eventuallyTalk(t, s, items(1))
	if ts.Items[0].Text != "hi" || ts.Items[0].From != "bob" {
		t.Fatalf("item: %+v", ts.Items[0])
	}
}

func TestWithNoMajorityASentMessageIsQueuedAndDropRemovesIt(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().noLeader = true
	s := served(t, r)
	ts := s.SendMessage("are you there?")
	if ts.Refused != nil || len(ts.Items) != 1 {
		t.Fatalf("after send: %+v", ts)
	}
	it := ts.Items[0]
	if !it.Queued || !it.Droppable || it.Side != "right" {
		t.Fatalf("queued item: %+v", it)
	}
	if ts := s.DropQueued(it.MsgID); len(ts.Items) != 0 || ts.Problem != nil {
		t.Fatalf("after drop: %+v", ts)
	}
}

func TestAQueuedMessageShowsCommittedOnceALeaderIsBack(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().noLeader = true
	s := served(t, r)
	s.SendMessage("later")
	r.node.drain()
	eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return len(ts.Items) == 1 && !ts.Items[0].Queued && !ts.Items[0].Droppable
	})
}

func TestMentioningAHandleNotIncludedIsRefusedBeforeSending(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().included[node.GroupTalk] = []string{"auditor"}
	s := served(t, r)
	ts := s.SendMessage("@scribe write this down")
	if ts.Refused == nil || ts.Refused.Code != "not-included" || !strings.Contains(ts.Refused.Message, "@scribe") {
		t.Fatalf("refused: %+v", ts.Refused)
	}
	if len(r.node.tk().sent) != 0 {
		t.Fatalf("sent anyway: %+v", r.node.tk().sent)
	}
	// Escaped, it summons nobody; an included Handle may be mentioned.
	if ts := s.SendMessage(`\@scribe and @auditor`); ts.Refused != nil {
		t.Fatalf("refused: %+v", ts.Refused)
	}
}

// chainEvent is a chain event of n's in conv.
func chainEvent(n *fakeNode, conv string, ev node.ChainEvent) {
	n.emit(node.Event{Chain: &node.ChainEventIn{Group: "alpha", Conversation: conv, Event: ev}})
}

func TestAnAgentsChainStreamsLiveCancelsAndLeavesATranscript(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().included[node.GroupTalk] = []string{"auditor"}
	s := served(t, r)
	s.SendMessage("@auditor check this")
	chain := r.node.tk().committed[node.GroupTalk][0].MsgID

	for _, ev := range []node.ChainEvent{
		{Type: serve.EventChainStart, Chain: chain},
		{Type: serve.EventAgentStart, Chain: chain, Handle: "auditor", Depth: 1},
		{Type: serve.EventAgentDelta, Chain: chain, Kind: "thinking", Text: "hmm"},
		{Type: serve.EventAgentDelta, Chain: chain, Kind: "text", Text: "Looks "},
		{Type: serve.EventAgentDelta, Chain: chain, Kind: "text", Text: "fine"},
	} {
		chainEvent(r.node, node.GroupTalk, ev)
	}
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return len(ts.Live) == 1 && len(ts.Live[0].Agents) == 1 && len(ts.Live[0].Agents[0].Blocks) == 2 &&
			ts.Live[0].Agents[0].Blocks[1].Text == "Looks fine"
	})
	if lc := ts.Live[0]; lc.Chain != chain || lc.Agents[0].Handle != "auditor" || lc.Agents[0].Blocks[0] != (shell.LiveBlock{Kind: "thinking", Text: "hmm"}) {
		t.Fatalf("live: %+v", lc)
	}

	if ts := s.CancelChain(chain); ts.Problem != nil || len(r.node.tk().cancelled) != 1 || r.node.tk().cancelled[0] != chain {
		t.Fatalf("cancel: %+v %v", ts.Problem, r.node.tk().cancelled)
	}

	// It said its piece before the cancel landed.
	said := fmt.Sprintf("%064x", 77)
	r.node.mu.Lock()
	r.node.tk().blocks[said] = []json.RawMessage{json.RawMessage(`{"type":"text","text":"Looks fine"}`)}
	r.node.mu.Unlock()
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventAgentCommit, Chain: chain, MsgID: said})
	r.node.commit(node.GroupTalk, node.Message{MsgID: said, NodeID: r.node.NodeID(), Handle: "auditor", Depth: 1, Text: "Looks fine"})
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventChainEnd, Chain: chain, Cancelled: true})

	ts = eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Live) == 0 && len(ts.Items) >= 2 })
	reply := ts.Items[1]
	if reply.MsgID != said || !reply.Transcript || reply.From != "This Device" || reply.Handle != "auditor" {
		t.Fatalf("reply: %+v", reply)
	}
	if v := s.Transcript(said); v.Problem != nil || len(v.Blocks) != 1 {
		t.Fatalf("transcript: %+v", v)
	}
}

func TestANewKeyIsAProvisionalRowUntilItsKeygenCompletes(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	if ts := s.NewKey("rsa", 2, ""); ts.Problem == nil || ts.Problem.Code != "bad-curve" {
		t.Fatalf("bad curve: %+v", ts.Problem)
	}
	ts := s.NewKey("ecdsa", 2, "treasury")
	if ts.Problem != nil || len(ts.Keygens) != 1 {
		t.Fatalf("after NewKey: %+v", ts)
	}
	kg := ts.Keygens[0]
	if kg.Curve != "ecdsa" || kg.T != 2 || kg.Alias != "treasury" || !kg.Accepted {
		t.Fatalf("keygen row: %+v", kg)
	}

	k := r.node.mint(kg.SessionID)
	ts = eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Keygens) == 0 && len(ts.Conversations) == 2 })
	if row := ts.Conversations[1]; row.ID != k.ConversationID || row.Alias != "treasury" || row.T != 2 || row.N != 2 {
		t.Fatalf("key row: %+v", row)
	}
	// Group talk says so.
	var lines []string
	for _, it := range ts.Items {
		if it.Kind == shell.KindSystem {
			lines = append(lines, it.Text)
		}
	}
	if len(lines) != 2 || !strings.Contains(lines[0], "being made") || !strings.Contains(lines[1], "New Key ready: treasury") {
		t.Fatalf("system lines: %q", lines)
	}
}

func TestAnotherDevicesKeygenIsAcceptedOrAbortedHere(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Conversations) == 1 })
	other := node.Session{SessionID: "0ther", Status: "open", Curve: "eddsa", T: 2, N: 2}
	r.node.mu.Lock()
	r.node.tk().keygens = append(r.node.tk().keygens, other, node.Session{SessionID: "th1rd", Status: "open", Curve: "ecdsa", T: 1, N: 2})
	r.node.mu.Unlock()
	r.node.emit(node.Event{Session: &node.SessionEvent{Group: "alpha", Session: other}})
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Keygens) == 2 })
	if ts.Keygens[0].Accepted {
		t.Fatalf("not accepted yet: %+v", ts.Keygens[0])
	}
	if ts := s.AcceptKeygen("0ther", "cold"); ts.Problem != nil || !ts.Keygens[0].Accepted {
		t.Fatalf("accept: %+v", ts)
	}
	if ts := s.AbortKeygen("th1rd", "wrong t"); ts.Problem != nil {
		t.Fatalf("abort: %+v", ts.Problem)
	}
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Keygens) == 1 })
	if a := r.node.tk().aborted; len(a) != 1 || a[0] != "th1rd" {
		t.Fatalf("aborted: %v", a)
	}
}

// withKey is r's fake Node with one Key minted in alpha.
func withKey(t *testing.T, r *rig) node.Key {
	t.Helper()
	r.node.mu.Lock()
	r.node.tk().keygens = []node.Session{{SessionID: "k3y", Status: "open", Curve: "ecdsa", T: 2, N: 2, ProposedAlias: "vault"}}
	r.node.mu.Unlock()
	return r.node.mint("k3y")
}

func TestAKeysConversationShowsItsKeyAndWhetherItCanSign(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	s := served(t, r)
	r.node.commit(k.ConversationID, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "about the key"})
	ts := s.OpenConversation(k.ConversationID)
	if ts.Open != k.ConversationID || len(ts.Items) != 1 || ts.Key == nil {
		t.Fatalf("open: %+v", ts)
	}
	if d := ts.Key; d.Alias != "vault" || d.Curve != "ecdsa" || d.PublicKey != k.PublicKey || d.T != 2 || d.N != 2 || !d.Share || d.CanSign {
		t.Fatalf("key: %+v", d)
	}
	// Group talk's messages stay in Group talk.
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "elsewhere"})

	r.node.mu.Lock()
	r.node.publish(node.Status{Group: "alpha", Serving: true, MajorityReachable: true,
		Roster: []node.RosterNode{{NodeID: r.node.NodeID(), DisplayName: "laptop", Reachable: true}, {NodeID: "b0b", DisplayName: "bob", Reachable: true}},
		Keys:   []node.KeyReach{{Key: k.Key, T: 2, Reachable: 2, CanSign: true}}})
	r.node.mu.Unlock()
	ts = eventuallyTalk(t, s, func(ts shell.TalkState) bool { return ts.Key != nil && ts.Key.CanSign })
	if ts.Online != 2 || !ts.MajorityReachable || len(ts.Roster) != 2 || !ts.Roster[0].Self || ts.Roster[1].Name != "bob" {
		t.Fatalf("roster: %+v", ts)
	}
	if len(ts.Items) != 1 {
		t.Fatalf("items: %+v", ts.Items)
	}

	if ts := s.SetKeyAlias("cold"); ts.Problem != nil || ts.Key.Alias != "cold" || ts.Conversations[1].Alias != "cold" {
		t.Fatalf("rename: %+v", ts)
	}
}

func TestIncludingAnAgentInAConversationKeepsTheGroupsAgents(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	r.node.tk().profiles = []string{"auditor", "scribe", "signer"}
	r.node.tk().included[node.GroupTalk] = []string{"auditor", "scribe"}
	s := served(t, r)
	ts := s.OpenConversation(k.ConversationID)
	if got := included(ts); got != "auditor,scribe" {
		t.Fatalf("follows the Group's: %s", got)
	}
	if ts := s.SetAgentIncluded("signer", true); ts.Problem != nil || included(ts) != "auditor,scribe,signer" {
		t.Fatalf("include: %+v %s", ts.Problem, included(ts))
	}
	if ts := s.SetAgentIncluded("auditor", false); ts.Problem != nil || included(ts) != "scribe,signer" || strings.Join(ts.Handles, ",") != "scribe,signer" {
		t.Fatalf("exclude: %+v %s", ts.Problem, included(ts))
	}
	// Group talk keeps its own.
	if ts := s.OpenConversation(node.GroupTalk); included(ts) != "auditor,scribe" {
		t.Fatalf("group: %s", included(ts))
	}
}

func TestExcludingFromAConversationThatFollowsTheGroupKeepsTheOthers(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	r.node.tk().profiles = []string{"auditor", "scribe"}
	r.node.tk().included[node.GroupTalk] = []string{"auditor", "scribe"}
	s := served(t, r)
	s.OpenConversation(k.ConversationID)
	if ts := s.SetAgentIncluded("scribe", false); ts.Problem != nil || included(ts) != "auditor" {
		t.Fatalf("exclude: %+v %s", ts.Problem, included(ts))
	}
	if ts := s.SetAgentIncluded("auditor", false); ts.Problem != nil || included(ts) != "" {
		t.Fatalf("exclude the last: %+v %s", ts.Problem, included(ts))
	}
}

func included(ts shell.TalkState) string {
	var hs []string
	for _, a := range ts.Agents {
		if a.Included {
			hs = append(hs, a.Handle)
		}
	}
	return strings.Join(hs, ",")
}

func TestAConversationShowsTheProvidersAndNetworksItOverrides(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	tk := r.node.tk()
	tk.providers[node.GroupTalk] = []node.Provider{{Alias: "anthropic", URL: "https://api.anthropic.com/v1/messages"}}
	tk.providers[k.ConversationID] = []node.Provider{{Alias: "anthropic", URL: "https://proxy.example/v1/messages"}}
	tk.networks[node.GroupTalk] = []node.Network{{Alias: "eth", Chain: "ethereum chain 1"}}
	tk.networks[k.ConversationID] = []node.Network{{Alias: "eth", Chain: "ethereum chain 1"}, {Alias: "sepolia", Chain: "ethereum chain 11155111"}}
	s := served(t, r)
	if ts := s.OpenConversation(node.GroupTalk); len(ts.Overrides) != 0 {
		t.Fatalf("group talk overrides nothing: %+v", ts.Overrides)
	}
	ts := s.OpenConversation(k.ConversationID)
	want := []shell.Override{
		{Kind: "provider", Alias: "anthropic", Detail: "https://proxy.example/v1/messages"},
		{Kind: "network", Alias: "sepolia", Detail: "ethereum chain 11155111"},
	}
	if fmt.Sprint(ts.Overrides) != fmt.Sprint(want) {
		t.Fatalf("overrides: %+v", ts.Overrides)
	}
}

func TestPrunedMessagesArePlaceholders(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Pruned: true})
	ts := eventuallyTalk(t, served(t, r), items(1))
	if it := ts.Items[0]; it.Kind != "pruned" || it.From != "bob" || it.Side != "left" || it.Text != "" {
		t.Fatalf("pruned: %+v", it)
	}
}

func TestSignRequestStatusAndTriggerFiringsAreSystemLines(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	s := served(t, r)
	s.OpenConversation(k.ConversationID)
	req := func(status string) {
		r.node.emit(node.Event{SignRequest: &node.SignRequestEvent{Group: "alpha", Request: signreq.Summary{
			MsgID: "5r", Conversation: k.ConversationID, Status: status, Reason: "pay rent"}}})
	}
	req("open")
	req("open") // a stance, no status change
	req("signed")
	ok := true
	chainEvent(r.node, k.ConversationID, node.ChainEvent{Type: serve.EventTriggerFire, Trigger: "review", Event: &trigger.Event{Type: "sign_request.open"}})
	chainEvent(r.node, k.ConversationID, node.ChainEvent{Type: serve.EventJobEnd, Job: "check", OK: &ok})
	ts := eventuallyTalk(t, s, items(4))
	var got []string
	for _, it := range ts.Items {
		got = append(got, string(it.Kind)+": "+it.Text)
	}
	want := []string{
		"system: Sign Request “pay rent” is open.",
		"system: Sign Request “pay rent” is signed.",
		"system: Trigger review fired on sign_request.open.",
		"system: Job check finished.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s", strings.Join(got, "\n"))
	}
}

func TestTalkReadsAgainOnceTheGroupsReplicaExists(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().noReplica = true
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "hi"})
	s := served(t, r)
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return ts.Problem != nil && ts.Problem.Code == "no-replica" })

	r.node.mu.Lock()
	r.node.tk().noReplica = false
	r.node.publish(node.Status{Group: "alpha", Serving: true, MajorityReachable: true})
	r.node.mu.Unlock()
	eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return ts.Problem == nil && len(ts.Conversations) == 1 && len(ts.Items) == 1
	})
}
