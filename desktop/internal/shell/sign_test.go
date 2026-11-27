package shell_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/jobrun"
	"defios.com/internal/node"
	"defios.com/internal/signreq"
	"defios.com/internal/transcript"
	"defios.com/internal/trigger"
)

// keyServed is a Shell serving alpha over r's fake Node, with a 2-of-2
// Key whose Conversation is open, on a Roster of this device and bob's.
func keyServed(t *testing.T, r *rig) (*shell.Shell, node.Key) {
	t.Helper()
	return curveServed(t, r, "ecdsa")
}

// curveServed is keyServed with a Key on curve.
func curveServed(t *testing.T, r *rig, curve string) (*shell.Shell, node.Key) {
	t.Helper()
	k := withKey(t, r)
	if curve == "eddsa" {
		r.node.mu.Lock()
		kk := &r.node.tk().keys[len(r.node.tk().keys)-1]
		kk.Curve, kk.PublicKey = "eddsa", fmt.Sprintf("%064x", 7)
		kk.Key = "eddsa:" + kk.PublicKey
		k = *kk
		r.node.mu.Unlock()
	}
	s := served(t, r)
	r.node.mu.Lock()
	r.node.publish(node.Status{Group: "alpha", Serving: true, MajorityReachable: true, Roster: []node.RosterNode{
		{NodeID: r.node.NodeID(), DisplayName: "laptop", Reachable: true},
		{NodeID: "b0b", DisplayName: "bob", Reachable: true},
	}})
	r.node.mu.Unlock()
	s.OpenConversation(k.ConversationID)
	return s, k
}

// card is the Sign Request msgID's card, once ok holds of it.
func card(t *testing.T, s *shell.Shell, msgID string, ok func(*shell.SignCard) bool) (shell.TalkState, shell.Item) {
	t.Helper()
	var found shell.Item
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		for _, it := range ts.Items {
			if it.Kind == shell.KindSign && it.Sign != nil && it.Sign.MsgID == msgID && ok(it.Sign) {
				found = it
				return true
			}
		}
		return false
	})
	return ts, found
}

func anyCard(*shell.SignCard) bool { return true }

func TestAnotherDevicesSignRequestIsACardOnTheLeftAwaitingThisDevice(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)

	_, it := card(t, s, id, anyCard)
	c := it.Sign
	if it.Side != shell.Left || it.From != "bob" {
		t.Fatalf("placed %s from %q", it.Side, it.From)
	}
	if c.Reason != "pay rent" || c.Status != "open" || c.Needed != 2 || !c.CanAnswer || c.CanBroadcast {
		t.Fatalf("card: %+v", c)
	}
	if len(c.Transactions) != 1 || c.Transactions[0].Chain != "ethereum chain 1" || len(c.Transactions[0].Lines) != 2 {
		t.Fatalf("transactions: %+v", c.Transactions)
	}
	want := []shell.SignStance{
		{Device: "bob", Stance: "accepted"},
		{Device: "This Device", Self: true, Stance: "unaddressed"},
	}
	if len(c.Stances) != len(want) {
		t.Fatalf("stances: %+v", c.Stances)
	}
	for i, w := range want {
		if g := c.Stances[i]; g.Device != w.Device || g.Self != w.Self || g.Stance != w.Stance || g.Handle != "" {
			t.Errorf("stance %d: %+v; want %+v", i, g, w)
		}
	}
}

// A signed request is broadcast on its own (ADR 0045): Broadcast is only
// offered again once a transaction was dropped or rejected.
func TestAcceptingSignsOnceEnoughAcceptAndBroadcastIsOnlyARetry(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	card(t, s, id, anyCard)

	if ts := s.AcceptSign(id); ts.Problem != nil {
		t.Fatalf("accept: %+v", ts.Problem)
	}
	_, it := card(t, s, id, func(c *shell.SignCard) bool { return c.Status == "signed" })
	c := it.Sign
	if c.Mine != "accepted" || c.CanAnswer || c.CanBroadcast || c.Queued {
		t.Fatalf("signed card: %+v", c)
	}
	if st := c.Stances[1]; !st.Self || st.Stance != "accepted" || st.Queued {
		t.Fatalf("this device's stance: %+v", st)
	}

	r.node.fail(id, signreq.ResultReverted, "out of gas")
	_, it = card(t, s, id, func(c *shell.SignCard) bool { return c.Status == "failed" })
	if c := it.Sign; c.CanBroadcast || c.Failure != "reverted" || len(c.Outcomes) != 1 {
		t.Fatalf("reverted card: %+v", c)
	}
	if h := it.Sign.History[len(it.Sign.History)-1]; !strings.Contains(h.Text, "out of gas") {
		t.Errorf("history ends %+v", h)
	}
	r.node.fail(id, signreq.ResultDropped, "its blockhash expired before it landed")
	_, it = card(t, s, id, func(c *shell.SignCard) bool { return c.CanBroadcast })
	if it.Sign.Failure != "dropped" {
		t.Fatalf("dropped card: %+v", it.Sign)
	}

	if ts := s.BroadcastSign(id); ts.Problem != nil {
		t.Fatalf("broadcast: %+v", ts.Problem)
	}
	_, it = card(t, s, id, func(c *shell.SignCard) bool { return len(c.Receipts) == 1 })
	if rc := it.Sign.Receipts[0]; rc.Device != "This Device" || len(rc.Transactions) != 1 || rc.Transactions[0].TxHash != "0xabc" {
		t.Fatalf("receipt: %+v", rc)
	}
	if it.Sign.Status != "broadcast" || it.Sign.CanBroadcast {
		t.Fatalf("rebroadcast card: %+v", it.Sign)
	}
}

func TestRefusingNeedsAReasonAndShowsTheRefusal(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	card(t, s, id, anyCard)

	if ts := s.RefuseSign(id, "  "); ts.Problem == nil || ts.Problem.Code != "invalid" {
		t.Fatalf("a blank reason: %+v", ts.Problem)
	}
	if ts := s.RefuseSign(id, "unknown payee"); ts.Problem != nil {
		t.Fatalf("refuse: %+v", ts.Problem)
	}
	_, it := card(t, s, id, func(c *shell.SignCard) bool { return c.Status == "refused" })
	st := it.Sign.Stances[1]
	if !st.Self || st.Stance != "refused" || st.Reason != "unknown payee" || it.Sign.CanAnswer {
		t.Fatalf("refusal: %+v", it.Sign)
	}
	// A stance is final.
	if ts := s.AcceptSign(id); ts.Problem == nil || ts.Problem.Code != "already-refused" {
		t.Fatalf("accept after refusing: %+v", ts.Problem)
	}
}

func TestWithoutAMajorityThisDevicesStanceShowsQueuedUntilItCommits(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	card(t, s, id, anyCard)
	r.node.mu.Lock()
	r.node.tk().noLeader = true
	r.node.mu.Unlock()

	if ts := s.AcceptSign(id); ts.Problem != nil {
		t.Fatalf("accept: %+v", ts.Problem)
	}
	_, it := card(t, s, id, func(c *shell.SignCard) bool { return c.Queued })
	if st := it.Sign.Stances[1]; !st.Self || st.Stance != "accepted" || !st.Queued || it.Sign.CanAnswer || it.Sign.Status != "open" {
		t.Fatalf("queued: %+v", it.Sign)
	}
	r.node.commitStances()
	card(t, s, id, func(c *shell.SignCard) bool { return !c.Queued && c.Status == "signed" && !c.Stances[1].Queued })
}

func TestTheRightColumnListsTheKeysSignRequests(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	a := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	b := r.node.propose(k.ConversationID, "b0b", "", "top up", 2)
	r.node.answer(b, "b0b", "", true, "no")
	ts := eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return len(ts.SignRequests) == 2 && ts.SignRequests[1].Status == "refused"
	})
	if g := ts.SignRequests[0]; g.MsgID != a || g.Reason != "pay rent" || g.Status != "open" {
		t.Fatalf("rows: %+v", ts.SignRequests)
	}
	if ts := s.OpenConversation(node.GroupTalk); len(ts.SignRequests) != 0 {
		t.Fatalf("Group talk lists %+v", ts.SignRequests)
	}
}

func TestFocusingASignRequestOpensItsConversationOnItsCard(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	s.OpenConversation(node.GroupTalk)
	if ts := s.FocusSign("beta", k.ConversationID, id); ts.Open != node.GroupTalk || ts.Focus != "" {
		t.Fatalf("another Group's notice moved the page: %+v", ts)
	}
	ts := s.FocusSign("alpha", k.ConversationID, id)
	if ts.Open != k.ConversationID || ts.Focus != id {
		t.Fatalf("focus: open %q focus %q", ts.Open, ts.Focus)
	}
	card(t, s, id, anyCard)
	if ts := s.OpenConversation(node.GroupTalk); ts.Focus != "" {
		t.Fatalf("focus kept: %q", ts.Focus)
	}
}

// A Sign Request's card says how many transactions it has and on which
// chains, and keeps its History; a change of its status is no line of
// the transcript.
func TestASignRequestsHistoryIsOnItsCardNotTheTranscript(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	r.node.answer(id, r.node.NodeID(), "", false, "")

	ts, it := card(t, s, id, func(c *shell.SignCard) bool { return c.Status == "signed" })
	c := it.Sign
	if c.Summary != "1 transaction · ethereum chain 1" {
		t.Fatalf("summary %q", c.Summary)
	}
	var got []string
	for _, ev := range c.History {
		got = append(got, ev.Kind+": "+ev.Text)
	}
	want := []string{"proposed: bob proposed it.", "accepted: This Device accepted.", "signed: Signed."}
	if !slices.Equal(got, want) {
		t.Fatalf("history:\n%s", strings.Join(got, "\n"))
	}
	for _, it := range ts.Items {
		if it.Kind == shell.KindSystem {
			t.Fatalf("system line: %q", it.Text)
		}
	}
}

// A Sign Request proposed by a run of this device's agent names that run,
// for its card to lead to.
func TestASignRequestNamesTheRunOfThisDevicesAgentThatProposedIt(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	r.node.mu.Lock()
	id := fmt.Sprintf("5e%062x", r.node.tk().seq+1) // the next propose's
	r.node.sg().runs["c4a1"] = []transcript.Run{{Chain: "c4a1", Step: 2, Handle: "treasurer", Depth: 1, Outcome: transcript.OutcomeSilent,
		Acted: []transcript.Acted{{Tool: "sign_propose", MsgID: id}}}}
	r.node.mu.Unlock()
	s := served(t, r)
	s.OpenConversation(k.ConversationID)
	if got := r.node.propose(k.ConversationID, r.node.NodeID(), "treasurer", "pay rent", 2); got != id {
		t.Fatalf("proposed %s, want %s", got, id)
	}
	_, it := card(t, s, id, func(c *shell.SignCard) bool { return c.Cause != nil })
	if *it.Sign.Cause != (shell.SignCause{Chain: "c4a1", Step: 2}) {
		t.Fatalf("cause %+v", *it.Sign.Cause)
	}
}

// A Sign Request this device's exec Job proposed names that Job's Job
// Run, whose details list the requests it proposed, accepted or refused.
func TestASignRequestNamesTheJobRunOfThisDevicesExecJobThatProposedIt(t *testing.T) {
	r := newRig(t, "alpha")
	k := withKey(t, r)
	r.node.mu.Lock()
	id := fmt.Sprintf("5e%062x", r.node.tk().seq+1) // the next propose's
	r.node.mu.Unlock()
	now := time.Now()
	r.node.addJobRuns(node.JobRun{ByHand: true, Fired: now, Job: "pay", Kind: "exec", State: node.JobOK, Started: now, Ended: now,
		Event: trigger.Event{ConversationID: k.ConversationID},
		Signs: []jobrun.Sign{{Act: jobrun.Propose, MsgID: id}, {Act: jobrun.Refuse, MsgID: "5eff"}}})
	s := served(t, r)
	s.OpenConversation(k.ConversationID)
	if got := r.node.propose(k.ConversationID, r.node.NodeID(), "", "pay rent", 2); got != id {
		t.Fatalf("proposed %s, want %s", got, id)
	}
	_, it := card(t, s, id, func(c *shell.SignCard) bool { return c.Cause != nil })
	if *it.Sign.Cause != (shell.SignCause{JobRun: 1}) {
		t.Fatalf("cause %+v", *it.Sign.Cause)
	}
	want := []shell.RunSign{{Tool: "sign_propose", MsgID: id, Reason: "pay rent"}, {Tool: "sign_refuse", MsgID: "5eff"}}
	if j := s.JobRunDetails(1).Job; j == nil || !slices.Equal(j.Signs, want) {
		t.Fatalf("Job Run details: %+v", j)
	}
}
