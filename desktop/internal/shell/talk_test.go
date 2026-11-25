package shell_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/transcript"
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
	r.node.sg().runs[chain] = []transcript.Run{{Chain: chain, Step: 1, Handle: "auditor", Depth: 1, Outcome: transcript.OutcomeSaid, MsgID: said,
		Blocks: []json.RawMessage{json.RawMessage(`{"type":"text","text":"Looks fine"}`)}}}
	r.node.mu.Unlock()
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventAgentCommit, Chain: chain, MsgID: said})
	r.node.commit(node.GroupTalk, node.Message{MsgID: said, NodeID: r.node.NodeID(), Handle: "auditor", Depth: 1, Text: "Looks fine"})
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventChainEnd, Chain: chain, Cancelled: true})

	ts = eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Live) == 0 && len(ts.Items) >= 2 })
	reply := ts.Items[1]
	if reply.MsgID != said || reply.Chain != chain || reply.From != "This Device" || reply.Handle != "auditor" {
		t.Fatalf("reply: %+v", reply)
	}
	if d := s.MessageDetails(said); d.Problem != nil || d.Run == nil || len(d.Run.Output) != 1 {
		t.Fatalf("details: %+v", d)
	}
}

// An agent that ends its turn without talk_write shows as a faded message
// from it once its run ends, with what it wrote in its Transcript, not
// nothing.
func TestAnAgentThatStaysSilentLeavesAMessageWithItsTranscript(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().included[node.GroupTalk] = []string{"auditor"}
	s := served(t, r)
	s.SendMessage("@auditor state your purpose")
	chain := r.node.tk().committed[node.GroupTalk][0].MsgID

	for _, ev := range []node.ChainEvent{
		{Type: serve.EventChainStart, Chain: chain},
		{Type: serve.EventAgentStart, Chain: chain, Handle: "auditor", Depth: 1},
		{Type: serve.EventAgentDelta, Chain: chain, Kind: "text", Text: "I audit."},
	} {
		chainEvent(r.node, node.GroupTalk, ev)
	}
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Live) == 1 })
	r.node.mu.Lock()
	r.node.sg().runs[chain] = []transcript.Run{{Chain: chain, Step: 1, Handle: "auditor", Depth: 1, Outcome: transcript.OutcomeSilent, Ended: time.Now(),
		Blocks: []json.RawMessage{json.RawMessage(`{"type":"text","text":"I audit."}`)}}}
	r.node.mu.Unlock()
	// The chain has yet to end: the run's end shows it.
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventAgentSilent, Chain: chain, Handle: "auditor"})
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Items) == 2 })
	if line := ts.Items[1]; line.Kind != shell.KindRun || line.Handle != "auditor" || line.Chain != chain || line.Text != "Finished without replying." {
		t.Fatalf("silent run: %+v", line)
	}
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventChainEnd, Chain: chain})
	ts = eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Live) == 0 })
	if len(ts.Items) != 2 {
		t.Fatalf("items: %+v", ts.Items)
	}
	if d := s.RunDetails(chain, 1); d.Problem != nil || d.Run == nil || len(d.Run.Output) != 1 || d.Run.Outcome != transcript.OutcomeSilent {
		t.Fatalf("its run: %+v", d)
	}
}

// A summon shows on the agent that made it: live as talk_summon accepts
// it, then from its Transcript on what its run left, the faded row of a
// silent run or the message it said, each Handle in its agent's Livery
// (ADR 0041).
func TestSummonsShowOnTheRunThatMadeThem(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().included[node.GroupTalk] = []string{"router", "auditor", "scribe"}
	r.node.tk().liveries = map[string]string{node.AgentParty("auditor"): "blue-stud"}
	s := served(t, r)
	s.SendMessage("@router who should look?")
	chain := r.node.tk().committed[node.GroupTalk][0].MsgID

	for _, ev := range []node.ChainEvent{
		{Type: serve.EventChainStart, Chain: chain},
		{Type: serve.EventAgentStart, Chain: chain, Handle: "router", Depth: 1},
		{Type: serve.EventAgentSummoned, Chain: chain, Handle: "auditor", By: "router"},
	} {
		chainEvent(r.node, node.GroupTalk, ev)
	}
	auditor := shell.Party{Party: node.AgentParty("auditor"), Handle: "auditor", Livery: "blue-stud"}
	eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return len(ts.Live) == 1 && len(ts.Live[0].Agents) == 1 && slices.Equal(ts.Live[0].Agents[0].Summoned, []shell.Party{auditor})
	})

	said := fmt.Sprintf("%064x", 78)
	r.node.mu.Lock()
	r.node.sg().runs[chain] = []transcript.Run{
		{Chain: chain, Step: 1, Handle: "router", Depth: 1, Outcome: transcript.OutcomeSilent, Ended: time.Now(), Summoned: []string{"auditor"}},
		{Chain: chain, Step: 2, Handle: "auditor", Depth: 2, Outcome: transcript.OutcomeSaid, MsgID: said, Summoned: []string{"scribe"}},
	}
	r.node.mu.Unlock()
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventAgentSilent, Chain: chain, Handle: "router"})
	r.node.commit(node.GroupTalk, node.Message{MsgID: said, NodeID: r.node.NodeID(), Handle: "auditor", Depth: 2, Text: "scribe, note this"})
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventChainEnd, Chain: chain})

	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return len(ts.Live) == 0 && len(ts.Items) == 3 && ts.Items[2].Chain != ""
	})
	if run := ts.Items[1]; run.Kind != shell.KindRun || !slices.Equal(run.Summoned, []shell.Party{auditor}) {
		t.Fatalf("silent run: %+v", run)
	}
	scribe := shell.Party{Party: node.AgentParty("scribe"), Handle: "scribe"}
	if msg := ts.Items[2]; msg.MsgID != said || !slices.Equal(msg.Summoned, []shell.Party{scribe}) {
		t.Fatalf("said message: %+v", msg)
	}
}

// A run's summons stay on the message it said from its commit, while the
// agents it summoned answer, before the chain ends (ADR 0041).
func TestASaidRunsSummonsStayOnItsMessageWhileTheChainGoesOn(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().included[node.GroupTalk] = []string{"router", "auditor"}
	s := served(t, r)
	s.SendMessage("@router who should look?")
	chain := r.node.tk().committed[node.GroupTalk][0].MsgID
	said := fmt.Sprintf("%064x", 79)
	for _, ev := range []node.ChainEvent{
		{Type: serve.EventChainStart, Chain: chain},
		{Type: serve.EventAgentStart, Chain: chain, Handle: "router", Depth: 1},
		{Type: serve.EventAgentSummoned, Chain: chain, Handle: "auditor", By: "router"},
		{Type: serve.EventAgentCommit, Chain: chain, MsgID: said},
	} {
		chainEvent(r.node, node.GroupTalk, ev)
	}
	r.node.commit(node.GroupTalk, node.Message{MsgID: said, NodeID: r.node.NodeID(), Handle: "router", Depth: 1, Text: "over to auditor"})
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventAgentStart, Chain: chain, Handle: "auditor", Depth: 2})
	auditor := []shell.Party{{Party: node.AgentParty("auditor"), Handle: "auditor"}}
	eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return len(ts.Items) == 2 && ts.Items[1].MsgID == said && slices.Equal(ts.Items[1].Summoned, auditor) &&
			len(ts.Live) == 1 && len(ts.Live[0].Agents) == 1 && ts.Live[0].Agents[0].Handle == "auditor"
	})
}

// This device's agents' runs that posted nothing show as faded messages
// from that agent, read from their Transcripts, live or cleared, so they
// are there as the page opens: each placed by when it ended, and grouped
// with that agent's own messages. A failure's error is only in its
// details.
func TestRunsThatPostedNothingAreFadedMessagesFromTheirAgent(t *testing.T) {
	r := newRig(t, "alpha")
	t0 := time.Now().Add(-time.Hour).UTC()
	long := strings.Repeat("the Provider refused the request ", 20)
	r.node.mu.Lock()
	r.node.sg().runs["a1"] = []transcript.Run{{Chain: "a1", Step: 1, Handle: "auditor", Depth: 1, Outcome: transcript.OutcomeSilent, Ended: t0.Add(15 * time.Minute)}}
	r.node.sg().runs["b2"] = []transcript.Run{{Chain: "b2", Step: 1, Handle: "scribe", Depth: 1, Outcome: transcript.OutcomeError, Error: long, Ended: t0.Add(5 * time.Minute)}}
	r.node.sg().runs["c3"] = []transcript.Run{
		{Chain: "c3", Step: 1, Handle: "scribe", Depth: 1, Outcome: transcript.OutcomeSaid, MsgID: "ab", Ended: t0.Add(24 * time.Minute)},
		{Chain: "c3", Step: 2, Handle: "auditor", Depth: 2, Outcome: transcript.OutcomeCancelled, Ended: t0.Add(25 * time.Minute)},
	}
	r.node.sg().cleared["c3"] = []int{1, 2}
	r.node.mu.Unlock()
	bob := func(text string, at time.Duration) {
		r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: text, CreatedAt: t0.Add(at)})
	}
	bob("one", 0)
	bob("two", 10*time.Minute)
	r.node.commit(node.GroupTalk, node.Message{NodeID: r.node.NodeID(), Handle: "auditor", Depth: 1, Text: "three", CreatedAt: t0.Add(16 * time.Minute)})
	s := served(t, r)

	ts := eventuallyTalk(t, s, items(6))
	var got []string
	for _, it := range ts.Items {
		got = append(got, fmt.Sprintf("%s %s @%s %s", it.Kind, it.Side, it.Handle, it.Text))
	}
	want := []string{
		"message left @ one",
		"run left @scribe Failed.",
		"message left @ two",
		"run left @auditor Finished without replying.",
		"message left @auditor three",
		"run left @auditor Cancelled.",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("items:\n%s", strings.Join(got, "\n"))
	}
	failed, silent, three, cancelled := ts.Items[1], ts.Items[3], ts.Items[4], ts.Items[5]
	if failed.From != "This Device" || failed.Chain != "b2" || failed.Step != 1 || failed.Outcome != transcript.OutcomeError || failed.At.IsZero() {
		t.Fatalf("failed: %+v", failed)
	}
	if !silent.At.Equal(t0.Add(15*time.Minute)) || cancelled.Chain != "c3" || cancelled.Step != 2 {
		t.Fatalf("silent %+v, cancelled %+v", silent, cancelled)
	}
	if !silent.Author || silent.Avatar || three.Author || !three.Avatar {
		t.Fatalf("one run of @auditor's: %+v, %+v", silent, three)
	}

	d := s.RunDetails("b2", 1)
	if d.Problem != nil || d.Run == nil || d.Run.Outcome != transcript.OutcomeError || d.Run.Error != long || d.Run.Handle != "scribe" {
		t.Fatalf("details: %+v", d)
	}
	if d := s.RunDetails("c3", 2); d.Problem != nil || d.Run == nil || d.Run.Handle != "auditor" || !d.Run.Cleared {
		t.Fatalf("cleared run's details: %+v", d)
	}
}

// An agent whose Transcript was closed before its run, as when its
// Provider changed API Shape, leaves a line saying it starts a new one,
// and why (ADR 0036).
func TestANewTranscriptLeavesALine(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().included[node.GroupTalk] = []string{"auditor"}
	s := served(t, r)
	s.SendMessage("@auditor hello")
	chain := r.node.tk().committed[node.GroupTalk][0].MsgID
	for _, ev := range []node.ChainEvent{
		{Type: serve.EventChainStart, Chain: chain},
		{Type: serve.EventAgentTranscript, Chain: chain, Handle: "auditor", Reason: "its Provider now speaks another API Shape"},
		{Type: serve.EventChainEnd, Chain: chain},
	} {
		chainEvent(r.node, node.GroupTalk, ev)
	}
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Live) == 0 && len(ts.Items) == 2 })
	if line := ts.Items[1]; line.Kind != shell.KindSystem || line.Text != "@auditor starts a new Transcript: its Provider now speaks another API Shape." {
		t.Fatalf("line: %+v", line)
	}
}

func TestANewKeyIsAProvisionalRowUntilItsKeygenCompletes(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	if ts := s.NewKey("rsa", 2, "", ""); ts.Problem == nil || ts.Problem.Code != "bad-curve" {
		t.Fatalf("bad curve: %+v", ts.Problem)
	}
	ts := s.NewKey("ecdsa", 2, "treasury", "bitcoin")
	if ts.Problem != nil || len(ts.Keygens) != 1 {
		t.Fatalf("after NewKey: %+v", ts)
	}
	kg := ts.Keygens[0]
	if kg.Curve != "ecdsa" || kg.T != 2 || kg.Alias != "treasury" || kg.NetworkType != "bitcoin" || !kg.Accepted {
		t.Fatalf("keygen row: %+v", kg)
	}

	k := r.node.mint(kg.SessionID)
	// The row comes with a reload, the system line with the Session's
	// event: wait for both.
	ts = eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return len(ts.Keygens) == 0 && len(ts.Conversations) == 2 && slices.ContainsFunc(ts.Items, func(it shell.Item) bool {
			return it.Kind == shell.KindSystem && strings.Contains(it.Text, "New Key ready")
		})
	})
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

func TestARowCountsWhatWasSaidPastTheReadMarkAndTheOpenOneShowsNone(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	s := served(t, r)
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Conversations) == 2 })
	self := r.node.NodeID()
	row := func(ts shell.TalkState, id string) shell.Conversation {
		for _, c := range ts.Conversations {
			if c.ID == id {
				return c
			}
		}
		t.Fatalf("no row %q: %+v", id, ts.Conversations)
		return shell.Conversation{}
	}

	r.node.commit(k.ConversationID, node.Message{NodeID: self, Mine: true, Text: "mine, sent elsewhere"})
	r.node.commit(k.ConversationID, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "one"})
	r.node.commit(k.ConversationID, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "two"})
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool { return row(ts, k.ConversationID).Unread == 2 })
	if c := row(ts, k.ConversationID); c.UnreadAutomated {
		t.Fatalf("people alone marked automated: %+v", c)
	}
	r.node.commit(k.ConversationID, node.Message{NodeID: self, Depth: 1, Text: "job output"})
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return row(ts, k.ConversationID).UnreadAutomated })

	// The open Conversation shows none, read or not.
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "here"})
	ts = eventuallyTalk(t, s, items(1))
	if c := row(ts, node.GroupTalk); c.Unread != 0 || c.UnreadAutomated {
		t.Fatalf("open Conversation counted: %+v", c)
	}
	// Read to its end, it stays read once left; a Conversation not open
	// is not marked.
	if err := s.ReadTo(node.GroupTalk, ts.Items[0].Seq); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadTo(k.ConversationID, 99); err != nil {
		t.Fatal(err)
	}
	ts = s.OpenConversation(k.ConversationID)
	if c := row(ts, node.GroupTalk); c.Unread != 0 || c.UnreadAutomated {
		t.Fatalf("Group talk read but counted: %+v", c)
	}
	// Opened but never read to its end: it counts again once left.
	ts = s.OpenConversation(node.GroupTalk)
	if c := row(ts, k.ConversationID); c.Unread != 2 || !c.UnreadAutomated {
		t.Fatalf("left unread: %+v", c)
	}
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
	if got := strings.Join(ts.Key.NetworkTypes, ","); got != "bitcoin,ethereum" {
		t.Fatalf("an ECDSA Key's network types: %s", got)
	}
	if ts := s.SetKeyNetworkType("bitcoin"); ts.Problem != nil || ts.Key.NetworkType != "bitcoin" || ts.Conversations[1].NetworkType != "bitcoin" {
		t.Fatalf("set network type: %+v", ts)
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
	if ts := s.SetAgentIncluded("auditor", false); ts.Problem != nil || included(ts) != "scribe,signer" || !slices.Equal(ts.Handles, []shell.Handle{{Handle: "scribe"}, {Handle: "signer"}}) {
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

// Excluding an agent a Job answers with names the Job, and the Key it
// answers in as the page names it.
func TestExcludingAnAgentAJobAnswersWithNamesTheJob(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	r.node.tk().profiles = []string{"auditor"}
	r.node.tk().included[node.GroupTalk] = []string{"auditor"}
	r.node.tk().refuseInclusion = &node.AnswersError{Jobs: []node.JobAnswer{
		{Job: "review", Handle: "auditor", Target: "group"},
		{Job: "watch", Handle: "auditor", Target: k.Workspace},
	}}
	s := served(t, r)
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Conversations) == 2 })
	s.OpenConversation(node.GroupTalk)
	ts := s.SetAgentIncluded("auditor", false)
	want := "@auditor answers in Group talk for Job review; @auditor answers in vault for Job watch. Change where review and watch answer on Triggers first."
	if ts.Problem == nil || ts.Problem.Code != "job-answers" || ts.Problem.Message != want {
		t.Fatalf("refused: %+v", ts.Problem)
	}
	if included(ts) != "auditor" {
		t.Fatalf("still included: %s", included(ts))
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

// A pruned message is left out of the transcript; one that does not read
// is a placeholder.
func TestPrunedMessagesAreLeftOutAndUnreadableOnesArePlaceholders(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Pruned: true})
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Unreadable: true})
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "hi"})
	ts := eventuallyTalk(t, served(t, r), items(2))
	if it := ts.Items[0]; it.Kind != "unreadable" || it.From != "bob" || it.Side != "left" || it.Text != "" {
		t.Fatalf("unreadable: %+v", it)
	}
	if it := ts.Items[1]; it.Kind != "message" || it.Text != "hi" {
		t.Fatalf("readable: %+v", it)
	}
}

// A Trigger's firing and each Job it ran show in the Conversation of
// what it fired on, read from this device's Job Runs, so they are there
// as the page opens, placed by when each happened: the firing once, each
// Job once it ended, none for a Job never run. A trigger.fire or job.end
// reads them again.
func TestJobLinesAreReadFromJobRuns(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	t0 := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	r.node.commit(k.ConversationID, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "pay rent?", CreatedAt: t0})
	r.node.commit(k.ConversationID, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "thanks", CreatedAt: t0.Add(time.Hour)})
	ev := trigger.Event{Type: "sign_request.open", ConversationID: k.ConversationID}
	fired := t0.Add(time.Minute)
	r.node.addJobRuns(
		node.JobRun{Trigger: "review", Event: ev, Fired: fired, Job: "check", Kind: "exec", State: node.JobOK, Started: fired, Ended: fired.Add(time.Second)},
		node.JobRun{Trigger: "review", Event: ev, Fired: fired, Job: "notify", Kind: "webhook", State: node.JobFailed, Started: fired.Add(time.Second), Ended: fired.Add(2 * time.Second), Error: "HTTP 500"},
		node.JobRun{Trigger: "review", Event: ev, Fired: fired, Job: "after", Kind: "exec", State: node.JobNotRun},
	)
	r.node.addJobRuns(node.JobRun{Trigger: "nightly", Event: trigger.Event{Type: trigger.ScheduleEvent}, Fired: fired, Job: "sweep", Kind: "exec", State: node.JobOK, Ended: fired})
	s := served(t, r)
	s.OpenConversation(k.ConversationID)

	lines := func(ts shell.TalkState) string {
		var got []string
		for _, it := range ts.Items {
			got = append(got, fmt.Sprintf("%s %d: %s", it.Kind, it.JobRun, it.Text))
		}
		return strings.Join(got, "\n")
	}
	want := strings.Join([]string{
		"message 0: pay rent?",
		"job 1: Trigger review fired on sign_request.open.",
		"job 1: Job check finished.",
		"job 2: Job notify failed: HTTP 500",
		"message 0: thanks",
	}, "\n")
	ts := eventuallyTalk(t, s, items(5))
	if got := lines(ts); got != want {
		t.Fatalf("lines:\n%s", got)
	}

	// A firing under way: its line now, its Job's once it ended.
	now := time.Now()
	r.node.addJobRuns(node.JobRun{Trigger: "again", Event: ev, Fired: now, Job: "check", Kind: "exec", State: node.JobRunning, Started: now})
	chainEvent(r.node, k.ConversationID, node.ChainEvent{Type: serve.EventTriggerFire, Trigger: "again", Event: &ev})
	ts = eventuallyTalk(t, s, items(6))
	if it := ts.Items[5]; it.Kind != shell.KindJob || it.JobRun != 5 || it.Text != "Trigger again fired on sign_request.open." {
		t.Fatalf("firing: %+v", it)
	}
	r.node.endJobRun(5, node.JobOK, "")
	ok := true
	chainEvent(r.node, k.ConversationID, node.ChainEvent{Type: serve.EventJobEnd, Job: "check", OK: &ok})
	ts = eventuallyTalk(t, s, items(7))
	if it := ts.Items[6]; it.JobRun != 5 || it.Text != "Job check finished." {
		t.Fatalf("job end: %+v", it)
	}

	// Group talk has the Schedule's.
	s.OpenConversation(node.GroupTalk)
	ts = eventuallyTalk(t, s, items(2))
	if got := lines(ts); got != "job 4: Trigger nightly fired on its schedule.\njob 4: Job sweep finished." {
		t.Fatalf("Group talk:\n%s", got)
	}
}

// A Job run by hand shows "Job X run by hand." in its event's
// Conversation, Group talk for none, then how it ended, read from its Job
// Run; a job.run reads them again.
func TestAJobRunByHandShowsInTalk(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	t0 := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	r.node.commit(k.ConversationID, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "pay rent?", CreatedAt: t0})
	ev := trigger.Event{Type: "sign_request.open", ConversationID: k.ConversationID}
	fired := t0.Add(time.Minute)
	r.node.addJobRuns(node.JobRun{ByHand: true, Event: ev, Fired: fired, Job: "check", Kind: "exec", State: node.JobOK, Started: fired, Ended: fired.Add(time.Second)})
	s := served(t, r)
	s.OpenConversation(k.ConversationID)
	lines := func(ts shell.TalkState) string {
		var got []string
		for _, it := range ts.Items {
			got = append(got, fmt.Sprintf("%s %d: %s", it.Kind, it.JobRun, it.Text))
		}
		return strings.Join(got, "\n")
	}
	ts := eventuallyTalk(t, s, items(3))
	if got := lines(ts); got != "message 0: pay rent?\njob 1: Job check run by hand.\njob 1: Job check finished." {
		t.Fatalf("lines:\n%s", got)
	}

	s.OpenConversation(node.GroupTalk)
	eventuallyTalk(t, s, items(0))
	now := time.Now()
	r.node.addJobRuns(node.JobRun{ByHand: true, Fired: now, Job: "sweep", Kind: "exec", State: node.JobRunning, Started: now})
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventJobRun, Job: "sweep"})
	ts = eventuallyTalk(t, s, items(1))
	if got := lines(ts); got != "job 2: Job sweep run by hand." {
		t.Fatalf("Group talk:\n%s", got)
	}
}

func TestTalkReadsAgainOnceTheGroupsReplicaExists(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().noReplica = true
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "hi"})
	s := served(t, r)
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return ts.Unread != nil && ts.Unread.Code == "no-replica" })

	r.node.mu.Lock()
	r.node.tk().noReplica = false
	r.node.publish(node.Status{Group: "alpha", Serving: true, MajorityReachable: true})
	r.node.mu.Unlock()
	eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return ts.Unread == nil && len(ts.Conversations) == 1 && len(ts.Items) == 1
	})
}

// The page lists what the TalkState holds; JSON null for an empty list
// would break it. Only Key, Refused, Problem and Unread may be absent.
func TestTheTalkStateSendsEmptyListsNotNull(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	assertNoNullLists(t, eventuallyTalk(t, s, func(ts shell.TalkState) bool { return ts.Group != "" }))

	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventChainStart, Chain: "c1"})
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventAgentStart, Chain: "c1", Handle: "auditor", Depth: 1})
	assertNoNullLists(t, eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return len(ts.Live) == 1 && len(ts.Live[0].Agents) == 1
	}))
}

func assertNoNullLists(t *testing.T, ts shell.TalkState) {
	t.Helper()
	b, err := json.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if x == nil && k != "Key" && k != "Refused" && k != "Problem" && k != "Unread" {
					t.Errorf("%s.%s is null", path, k)
				}
				walk(path+"."+k, x)
			}
		case []any:
			for i, x := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), x)
			}
		}
	}
	walk("TalkState", v)
}

// Every device can be reachable while writes still can't commit, as when
// no leader settles; the header counts devices but isn't committable.
func TestEveryDeviceOnlineButNoCommitIsNotCommittable(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	r.node.mu.Lock()
	r.node.publish(node.Status{Group: "alpha", Serving: true, TailnetUp: true, MajorityReachable: false,
		Roster: []node.RosterNode{{NodeID: r.node.NodeID(), DisplayName: "laptop", Reachable: true}, {NodeID: "b0b", DisplayName: "bob", Reachable: true}}})
	r.node.mu.Unlock()
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Roster) == 2 })
	if ts.MajorityReachable || ts.Online != 2 {
		t.Fatalf("talk: online %d, majority %v", ts.Online, ts.MajorityReachable)
	}
	st := eventually(t, s, func(st shell.State) bool { return st.RosterSize == 2 })
	if st.MajorityReachable || st.Online != 2 {
		t.Fatalf("app: online %d, majority %v", st.Online, st.MajorityReachable)
	}
}

// A message is marked with its sender's Livery, and each mention in it
// with the Livery of the agent it names on the device that sent it.
func TestMessagesAndMentionsTakeTheLiveryOfThePartyTheyResolveTo(t *testing.T) {
	r := newRig(t, "alpha")
	self := r.node.NodeID()
	r.node.tk().liveries = map[string]string{
		node.AgentParty("auditor"):            "blue-stud",
		node.NodeParty("b0b"):                 "orange-arch",
		node.NodeAgentParty("b0b", "scribe"):  "cyan-round",
		node.NodeAgentParty("b0b", "auditor"): "violet-tile",
	}
	for _, m := range []node.Message{
		{NodeID: self, Mine: true, Text: "@Auditor and @scribe, not \\@auditor"},
		{NodeID: self, Handle: "auditor", Depth: 1, Text: "done"},
		{NodeID: self, Handle: "helper", Depth: 1, Text: "no livery"},
		{NodeID: self, Depth: 1, Text: "a Job"},
		{NodeID: "b0b", DisplayName: "bob", Text: "ask @auditor"},
		{NodeID: "b0b", DisplayName: "bob", Handle: "scribe", Depth: 1, Text: "noted"},
		{NodeID: "ca7", DisplayName: "cat", Text: "hi"},
		{NodeID: "b0b", DisplayName: "bob", Depth: 1, Text: "bob's Job"},
	} {
		r.node.commit(node.GroupTalk, m)
	}
	ts := eventuallyTalk(t, served(t, r), items(8))

	for i, want := range []string{"", "blue-stud", "", "", "orange-arch", "cyan-round", "", ""} {
		if got := ts.Items[i].Livery; got != want {
			t.Errorf("item %d %q: Livery %q, want %q", i, ts.Items[i].Text, got, want)
		}
	}
	wantParts := [][]shell.Part{
		{
			{Text: "@Auditor", Handle: "auditor", Livery: "blue-stud"},
			{Text: " and "},
			{Text: "@scribe", Handle: "scribe"},
			{Text: ", not \\@auditor"},
		},
		{{Text: "done"}},
		{{Text: "no livery"}},
		{{Text: "a Job"}},
		{{Text: "ask "}, {Text: "@auditor", Handle: "auditor", Livery: "violet-tile"}},
	}
	for i, want := range wantParts {
		if got := ts.Items[i].Parts; !slices.Equal(got, want) {
			t.Errorf("item %d parts %+v, want %+v", i, got, want)
		}
	}
}

// A run of left-side messages from one sender shows its author line on
// its first message and its avatar on its last; another sender, a message
// of this person's, a system line or a gap of over 5 minutes starts anew.
func TestARunOfOneSendersMessagesShowsTheAuthorFirstAndTheAvatarLast(t *testing.T) {
	r := newRig(t, "alpha")
	self := r.node.NodeID()
	t0 := time.Now().Add(-time.Hour)
	at := func(min float64) time.Time { return t0.Add(time.Duration(min * float64(time.Minute))) }
	for _, m := range []node.Message{
		{NodeID: "b0b", DisplayName: "bob", Text: "one", CreatedAt: at(0)},
		{NodeID: "b0b", DisplayName: "bob", Text: "two", CreatedAt: at(4)},
		{NodeID: "b0b", DisplayName: "bob", Text: "three", CreatedAt: at(8)},
		{NodeID: "b0b", DisplayName: "bob", Text: "late", CreatedAt: at(14)},
		{NodeID: "b0b", DisplayName: "bob", Handle: "scribe", Depth: 1, Text: "agent", CreatedAt: at(15)},
		{NodeID: self, Mine: true, Text: "mine", CreatedAt: at(16)},
		{NodeID: "b0b", DisplayName: "bob", Handle: "scribe", Depth: 1, Text: "again", CreatedAt: at(17)},
		{NodeID: self, Handle: "scribe", Depth: 1, Text: "ours", CreatedAt: at(18)},
		{NodeID: self, Depth: 1, Text: "job", CreatedAt: at(19)},
		{NodeID: self, Depth: 1, Text: "job 2", CreatedAt: at(20)},
	} {
		r.node.commit(node.GroupTalk, m)
	}
	ts := eventuallyTalk(t, served(t, r), items(10))

	want := []struct{ author, avatar bool }{
		{true, false}, {false, false}, {false, true}, // within 5 minutes of each other
		{true, true},                 // 6 minutes on
		{true, true},                 // bob's agent
		{false, false},               // this person's: no author line, no avatar
		{true, true},                 // after this person's
		{true, true},                 // this device's agent of the same Handle
		{true, false}, {false, true}, // a Job's
	}
	for i, w := range want {
		if it := ts.Items[i]; it.Author != w.author || it.Avatar != w.avatar {
			t.Errorf("item %d %q: author %v avatar %v, want %+v", i, it.Text, it.Author, it.Avatar, w)
		}
	}
}

func TestASystemLineEndsARun(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Conversations) == 1 })
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "one", CreatedAt: time.Now().Add(-time.Second)})
	eventuallyTalk(t, s, items(1))
	r.node.emit(node.Event{Chain: &node.ChainEventIn{Group: "alpha", Conversation: node.GroupTalk,
		Event: serve.ChainEvent{Type: serve.EventAgentSkipped, Chain: "c", Handle: "auditor", Reason: "gone"}}})
	eventuallyTalk(t, s, items(2))
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "two", CreatedAt: time.Now().Add(time.Second)})
	ts := eventuallyTalk(t, s, items(3))
	if a, b := ts.Items[0], ts.Items[2]; !a.Author || !a.Avatar || !b.Author || !b.Avatar || ts.Items[1].Kind != shell.KindSystem {
		t.Fatalf("items: %+v", ts.Items)
	}
}

// This device's agents answering, and the Handles the composer suggests,
// carry the Liveries of this device's agents.
func TestLiveAgentsAndHandlesTakeThisDevicesAgentsLiveries(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().included[node.GroupTalk] = []string{"auditor", "helper"}
	r.node.tk().liveries = map[string]string{
		node.AgentParty("auditor"):           "blue-stud",
		node.NodeAgentParty("b0b", "helper"): "cyan-round", // another device's helper
	}
	s := served(t, r)
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Handles) == 2 })
	want := []shell.Handle{{Handle: "auditor", Livery: "blue-stud"}, {Handle: "helper"}}
	if !slices.Equal(ts.Handles, want) {
		t.Errorf("Handles %+v, want %+v", ts.Handles, want)
	}

	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventAgentStart, Chain: "c1", Handle: "auditor", Depth: 1})
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventAgentStart, Chain: "c1", Handle: "helper", Depth: 2})
	ts = eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Live) == 1 && len(ts.Live[0].Agents) == 2 })
	if a := ts.Live[0].Agents; a[0].Livery != "blue-stud" || a[1].Livery != "" {
		t.Errorf("live agents %+v", a)
	}
}

// Devices here lists the Roster, this device first as This Device without
// a Livery, the others by name with their Liveries, and under each the
// agents of that device seen writing in the open Conversation.
func TestDevicesHereListsTheRosterAndTheAgentsSeenWritingHere(t *testing.T) {
	r := newRig(t, "alpha")
	self := r.node.NodeID()
	r.node.tk().liveries = map[string]string{
		node.AgentParty("auditor"):           "blue-stud",
		node.NodeParty("b0b"):                "orange-arch",
		node.NodeAgentParty("b0b", "scribe"): "cyan-round",
	}
	k := withKey(t, r)
	for _, m := range []node.Message{
		{NodeID: "b0b", DisplayName: "bob", Text: "hi"},
		{NodeID: "b0b", DisplayName: "bob", Handle: "scribe", Depth: 1, Text: "noted"},
		{NodeID: self, Handle: "auditor", Depth: 1, Text: "this device's own"},
		{NodeID: "b0b", DisplayName: "bob", Handle: "auditor", Depth: 1, Text: "bob's auditor"},
		{NodeID: "b0b", DisplayName: "bob", Handle: "scribe", Depth: 1, Text: "again"},
	} {
		r.node.commit(node.GroupTalk, m)
	}
	r.node.commit(k.ConversationID, node.Message{NodeID: "ca7", DisplayName: "cat", Handle: "elsewhere", Depth: 1, Text: "in the Key's"})
	s := served(t, r)
	r.node.mu.Lock()
	r.node.publish(node.Status{Group: "alpha", Serving: true, MajorityReachable: true, Roster: []node.RosterNode{
		{NodeID: "ca7", DisplayName: "cat"}, {NodeID: self, DisplayName: "laptop", Reachable: true}, {NodeID: "b0b", DisplayName: "bob"},
	}})
	r.node.mu.Unlock()
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Items) == 5 && len(ts.Devices) == 3 })

	want := []shell.Device{
		{Name: "This Device", Self: true, Agents: []shell.Party{}},
		{Party: node.NodeParty("b0b"), Name: "bob", Livery: "orange-arch", Agents: []shell.Party{
			{Party: node.NodeAgentParty("b0b", "scribe"), Handle: "scribe", Livery: "cyan-round"},
			{Party: node.NodeAgentParty("b0b", "auditor"), Handle: "auditor"},
		}},
		{Party: node.NodeParty("ca7"), Name: "cat", Agents: []shell.Party{}},
	}
	if fmt.Sprint(ts.Devices) != fmt.Sprint(want) {
		t.Fatalf("Devices\n %+v\nwant\n %+v", ts.Devices, want)
	}
}

// Any party's Livery can be set from Talk, by the party key each avatar
// carries, and shows at once on its messages and in the side panel; one
// another party holds is refused, random gives a free one and none clears
// it.
func TestSettingAPartysLiveryShowsAtOnceAcrossTheTranscriptAndThePanel(t *testing.T) {
	r := newRig(t, "alpha")
	self := r.node.NodeID()
	r.node.tk().profiles = []string{"auditor"}
	r.node.tk().liveries = map[string]string{
		node.AgentParty("auditor"): "blue-stud",
		node.NodeParty("b0b"):      "orange-arch",
	}
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Handle: "scribe", Depth: 1, Text: "noted"})
	r.node.commit(node.GroupTalk, node.Message{NodeID: self, Handle: "auditor", Depth: 1, Text: "ours"})
	r.node.commit(node.GroupTalk, node.Message{NodeID: self, Mine: true, Text: "mine"})
	s := served(t, r)
	r.node.mu.Lock()
	r.node.publish(node.Status{Group: "alpha", Serving: true, MajorityReachable: true, Roster: []node.RosterNode{
		{NodeID: self, DisplayName: "laptop", Reachable: true}, {NodeID: "b0b", DisplayName: "bob"},
	}})
	r.node.mu.Unlock()
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return len(ts.Items) == 3 && len(ts.Devices) == 2 && len(ts.Agents) == 1
	})

	scribe := node.NodeAgentParty("b0b", "scribe")
	if p := []string{ts.Items[0].Party, ts.Items[1].Party, ts.Items[2].Party}; !slices.Equal(p, []string{scribe, node.AgentParty("auditor"), ""}) {
		t.Fatalf("items' parties %q", p)
	}
	if a := ts.Agents[0]; a.Party != node.AgentParty("auditor") || a.Livery != "blue-stud" {
		t.Fatalf("Agents here: %+v", a)
	}
	// The picker greys out what other parties hold.
	if ts.Liveries[node.NodeParty("b0b")] != "orange-arch" || len(ts.Liveries) != 2 {
		t.Fatalf("Liveries %+v", ts.Liveries)
	}

	ts = s.SetLivery(scribe, "cyan-round")
	if ts.Problem != nil || ts.Items[0].Livery != "cyan-round" || ts.Devices[1].Agents[0].Livery != "cyan-round" || ts.Liveries[scribe] != "cyan-round" {
		t.Fatalf("set: %+v %+v %+v", ts.Problem, ts.Items[0], ts.Devices[1])
	}
	ts = s.SetLivery(scribe, "orange-arch")
	if ts.Problem == nil || ts.Problem.Code != "livery-taken" || ts.Items[0].Livery != "cyan-round" {
		t.Fatalf("taken: %+v %+v", ts.Problem, ts.Items[0])
	}
	ts = s.SetLivery(node.AgentParty("auditor"), node.NoLivery)
	if ts.Problem != nil || ts.Items[1].Livery != "" || ts.Agents[0].Livery != "" {
		t.Fatalf("none: %+v %+v", ts.Problem, ts.Items[1])
	}
	ts = s.SetLivery(node.NodeParty("b0b"), node.RandomLivery)
	if l := ts.Devices[1].Livery; ts.Problem != nil || l == "" || l == "orange-arch" || l == "cyan-round" {
		t.Fatalf("random: %+v %q", ts.Problem, l)
	}
}
