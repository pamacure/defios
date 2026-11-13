package shell_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
	"defios.com/internal/session"
	"defios.com/internal/trigger"
)

// notices waits for n notifications, and checks no more come.
func notices(t *testing.T, r *rig, n int) []shell.Notice {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for r.notifications() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d notifications; want %d: %+v", r.notifications(), n, r.noticesNow())
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // past the Hook grace
	got := r.noticesNow()
	if len(got) != n {
		t.Fatalf("%d notifications; want %d: %+v", len(got), n, got)
	}
	return got
}

func text(n shell.Notice) string { return n.Title + " | " + n.Body }

func TestASignRequestAwaitingThisDeviceNotifiesButOneItProposedDoesNot(t *testing.T) {
	r := newRig(t, "alpha")
	_, k := keyServed(t, r)
	r.node.propose(k.ConversationID, r.node.NodeID(), "", "my own", 2)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 3)
	r.node.answer(id, "c0c", "", false, "") // a stance is no new request
	n := notices(t, r, 1)[0]
	if n.Title != "New Sign Request · needs you" || n.Body != "bob asks: pay rent" {
		t.Fatalf("notice: %s", text(n))
	}
	if n.Group != "alpha" || n.Conversation != k.ConversationID || n.SignRequestID != id {
		t.Fatalf("it opens %+v", n)
	}
}

func hookRun(id uint64, req string, jobs ...node.JobRun) node.HookRun {
	return node.HookRun{ID: id, Trigger: "review", Event: trigger.Event{Type: "sign.request", SignRequestID: req}, Jobs: jobs}
}

func TestAHookOnANewSignRequestSaysWhoIsOnItInstead(t *testing.T) {
	for _, c := range []struct {
		jobs []node.JobRun
		want string
	}{
		{[]node.JobRun{{Job: "review", Kind: "agent", Handle: "reviewer", State: node.JobRunning}}, "New Sign Request · @reviewer is on it, no action needed"},
		{[]node.JobRun{
			{Job: "review", Kind: "agent", Handle: "reviewer", State: node.JobRunning},
			{Job: "note", Kind: "exec", State: node.JobPending},
			{Job: "sign", Kind: "agent", Handle: "signer", State: node.JobPending},
		}, "New Sign Request · @reviewer and @signer are on it, no action needed"},
		{[]node.JobRun{{Job: "make_sign_decision", Kind: "exec", State: node.JobRunning}}, "New Sign Request · Job make_sign_decision is on it, no action needed"},
	} {
		r := newRig(t, "alpha")
		_, k := keyServed(t, r)
		id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
		r.node.hookRun(hookRun(1, id, c.jobs...))
		if n := notices(t, r, 1)[0]; n.Title != c.want || n.SignRequestID != id {
			t.Errorf("notice: %s; want %q", text(n), c.want)
		}
	}
}

func TestAFailingJobNotifiesThatTheRequestNeedsThePerson(t *testing.T) {
	r := newRig(t, "alpha")
	_, k := keyServed(t, r)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	r.node.hookRun(hookRun(1, id, node.JobRun{Job: "make_sign_decision", Kind: "exec", State: node.JobRunning}))
	notices(t, r, 1)
	r.node.hookRun(hookRun(1, id, node.JobRun{Job: "make_sign_decision", Kind: "exec", State: node.JobFailed, Error: "exit status 1"}))
	r.node.hookRun(hookRun(1, id, node.JobRun{Job: "make_sign_decision", Kind: "exec", State: node.JobFailed, Error: "exit status 1"}))
	n := notices(t, r, 2)[1]
	if n.Title != "Sign Request needs you" || !strings.Contains(n.Body, "Job make_sign_decision failed: exit status 1") || n.SignRequestID != id {
		t.Fatalf("notice: %s", text(n))
	}
}

func TestAJobThatFailsAfterTheStanceWasTakenNeedsNobody(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	r.node.hookRun(hookRun(1, id, node.JobRun{Job: "review", Kind: "agent", Handle: "reviewer", State: node.JobRunning}))
	notices(t, r, 1)
	s.AcceptSign(id)
	card(t, s, id, func(c *shell.SignCard) bool { return c.Mine == "accepted" })
	r.node.hookRun(hookRun(1, id, node.JobRun{Job: "review", Kind: "agent", Handle: "reviewer", State: node.JobFailed, Error: "boom"}))
	notices(t, r, 1)
}

func TestAKeygenFromAnotherDeviceNotifiesButOneStartedHereDoesNot(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	if ts := s.NewKey("ecdsa", 2, "mine"); ts.Problem != nil {
		t.Fatal(ts.Problem)
	}
	r.node.emit(node.Event{Session: &node.SessionEvent{Group: "alpha", Session: node.Session{
		SessionID: "07he7", Status: session.Open, Curve: "eddsa", T: 2, N: 2, ProposedAlias: "treasury"}}})
	n := notices(t, r, 1)[0]
	if n.Title != "New Key · needs you" || !strings.Contains(n.Body, "treasury") || n.Conversation != node.GroupTalk {
		t.Fatalf("notice: %s", text(n))
	}
}

func TestAGroupProposalNotifies(t *testing.T) {
	r := newRig(t, "alpha")
	served(t, r)
	r.node.emit(node.Event{Proposal: &node.Proposal{Group: "alpha", GroupID: "g1", Initiator: "b0b", Roster: []node.ProposedNode{
		{NodeID: "b0b", DisplayName: "bob"}, {NodeID: r.node.NodeID(), DisplayName: "laptop"}}}})
	if n := notices(t, r, 1)[0]; n.Title != "Group proposal · needs you" || !strings.Contains(n.Body, "bob") {
		t.Fatalf("notice: %s", text(n))
	}
}

func TestNewMessagesNotifyOnlyWithThePreferenceOn(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "first"})
	notices(t, r, 0)

	if st := s.SetMessageNotifications(true); !st.Prefs.MessageNotifications {
		t.Fatalf("prefs: %+v", st.Prefs)
	}
	if p, _ := r.prefs.Load(); !p.MessageNotifications {
		t.Fatalf("saved: %+v", p)
	}
	r.node.commit(node.GroupTalk, node.Message{NodeID: r.node.NodeID(), Mine: true, Text: "my own"})
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Handle: "scribe", Depth: 1, Text: "second"})
	n := notices(t, r, 1)[0]
	if n.Title != "bob · @scribe in Group" || n.Body != "second" || n.Conversation != node.GroupTalk {
		t.Fatalf("notice: %s", text(n))
	}
}

func TestMutedNotificationsPostNothing(t *testing.T) {
	r := newRig(t, "alpha")
	_, k := keyServed(t, r)
	r.shell.SetMessageNotifications(true)
	r.shell.SetNotifications(false)
	r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "hi"})
	notices(t, r, 0)
}

func TestAJobThatFailsBeforeTheFirstNotificationMakesItSayTheRequestNeedsThePerson(t *testing.T) {
	r := newRig(t, "alpha")
	_, k := keyServed(t, r)
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	r.node.hookRun(hookRun(1, id, node.JobRun{Job: "make_sign_decision", Kind: "exec", State: node.JobFailed, Error: "exit status 1"}))
	n := notices(t, r, 1)[0]
	if n.Title != "Sign Request needs you" || !strings.Contains(n.Body, "Job make_sign_decision failed") || n.Conversation != k.ConversationID {
		t.Fatalf("notice: %s", text(n))
	}
}

func TestAHookRunSeenBeforeItsRequestStillOpensTheRequestsCard(t *testing.T) {
	r := newRig(t, "alpha")
	_, k := keyServed(t, r)
	// The run names the request the fake is about to propose.
	r.node.mu.Lock()
	next := fmt.Sprintf("5e%062x", r.node.tk().seq+1)
	r.node.mu.Unlock()
	r.node.hookRun(hookRun(1, next, node.JobRun{Job: "review", Kind: "agent", Handle: "reviewer", State: node.JobFailed, Error: "boom"}))
	id := r.node.propose(k.ConversationID, "b0b", "", "pay rent", 2)
	if id != next {
		t.Fatalf("proposed %s, not %s", id, next)
	}
	n := notices(t, r, 1)[0]
	if n.Title != "Sign Request needs you" || n.Conversation != k.ConversationID || n.SignRequestID != id || !strings.Contains(n.Body, "pay rent") {
		t.Fatalf("notice: %s %+v", text(n), n)
	}
}
