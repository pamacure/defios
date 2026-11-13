package shell_test

import (
	"fmt"
	"testing"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
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

func TestAcceptingSignsOnceEnoughAcceptAndBroadcastListsAReceipt(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	card(t, s, id, anyCard)

	if ts := s.AcceptSign(id); ts.Problem != nil {
		t.Fatalf("accept: %+v", ts.Problem)
	}
	_, it := card(t, s, id, func(c *shell.SignCard) bool { return c.Status == "signed" })
	c := it.Sign
	if c.Mine != "accepted" || c.CanAnswer || !c.CanBroadcast || c.Queued {
		t.Fatalf("signed card: %+v", c)
	}
	if st := c.Stances[1]; !st.Self || st.Stance != "accepted" || st.Queued {
		t.Fatalf("this device's stance: %+v", st)
	}
	if ts := s.BroadcastSign(id); ts.Problem != nil {
		t.Fatalf("broadcast: %+v", ts.Problem)
	}
	_, it = card(t, s, id, func(c *shell.SignCard) bool { return len(c.Receipts) == 1 })
	if rc := it.Sign.Receipts[0]; rc.Device != "This Device" || len(rc.Transactions) != 1 || rc.Transactions[0].TxHash != "0xabc" {
		t.Fatalf("receipt: %+v", rc)
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
