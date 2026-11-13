package shell_test

import (
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/nodedir"
)

// formed is two apps, alice and bob, whose Nodes formed Group home on m
// through onboarding, each landed on Talk.
func formed(t *testing.T) (alice, bob *shell.Shell) {
	t.Helper()
	alice, bob, _, _ = formedAt(t)
	return alice, bob
}

// formedAt is formed, with alice's and bob's Node directories.
func formedAt(t *testing.T) (alice, bob *shell.Shell, ap, bp nodedir.Paths) {
	t.Helper()
	m := &mesh{addrs: map[string]string{}}
	alice, ap = onboard(t, m, "alice")
	bob, bp = onboard(t, m, "bob")
	var st shell.State
	eventually(t, alice, func(shell.State) bool {
		st = alice.Discover()
		return st.Forming != nil && len(st.Forming.Discovered) == 1
	})
	alice.Propose([]string{st.Forming.Discovered[0].ID})
	eventually(t, bob, func(st shell.State) bool { return st.Forming != nil && st.Forming.Proposal != nil })
	bob.Accept()
	eventually(t, alice, view(shell.ViewServed))
	eventually(t, bob, view(shell.ViewServed))
	return alice, bob, ap, bp
}

func hasText(text string) func(shell.TalkState) bool {
	return func(ts shell.TalkState) bool {
		for _, it := range ts.Items {
			if it.Text == text {
				return true
			}
		}
		return false
	}
}

func TestTwoDevicesTalkMakeAKeyAndQueueWithoutAMajority(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group and makes a Key")
	}
	alice, bob := formed(t)
	ready := func(ts shell.TalkState) bool { return ts.MajorityReachable && len(ts.Conversations) == 1 }
	talkWithin(t, alice, 20*time.Second, ready)
	talkWithin(t, bob, 20*time.Second, ready)

	// Group talk: on the right for alice, on the left for bob.
	if ts := alice.SendMessage("hello bob"); ts.Refused != nil || ts.Problem != nil {
		t.Fatalf("send: %+v %+v", ts.Refused, ts.Problem)
	}
	ts := talkWithin(t, alice, 10*time.Second, hasText("hello bob"))
	if it := ts.Items[len(ts.Items)-1]; it.Side != "right" || it.Queued {
		t.Fatalf("alice's own: %+v", it)
	}
	ts = talkWithin(t, bob, 10*time.Second, hasText("hello bob"))
	if it := ts.Items[len(ts.Items)-1]; it.Side != "left" || it.From != "alice" {
		t.Fatalf("on bob: %+v", it)
	}
	if ts := bob.SendMessage("@nobody hi"); ts.Refused == nil || ts.Refused.Code != "not-included" {
		t.Fatalf("mention refused: %+v", ts.Refused)
	}

	// A new Key: a provisional row on both devices, then a Key on both.
	if ts := alice.NewKey("eddsa", 2, "treasury"); ts.Problem != nil {
		t.Fatalf("new key: %+v", ts.Problem)
	}
	ts = talkWithin(t, alice, 10*time.Second, func(ts shell.TalkState) bool { return len(ts.Keygens) == 1 })
	if !ts.Keygens[0].Accepted {
		t.Fatalf("alice started it, so accepted it: %+v", ts.Keygens[0])
	}
	ts = talkWithin(t, bob, 10*time.Second, func(ts shell.TalkState) bool { return len(ts.Keygens) == 1 })
	if kg := ts.Keygens[0]; kg.Alias != "treasury" || kg.Accepted {
		t.Fatalf("bob's provisional row: %+v", kg)
	}
	if ts := bob.AcceptKeygen(ts.Keygens[0].SessionID, ""); ts.Problem != nil {
		t.Fatalf("accept: %+v", ts.Problem)
	}
	minted := func(ts shell.TalkState) bool { return len(ts.Keygens) == 0 && len(ts.Conversations) == 2 }
	ts = talkWithin(t, alice, 60*time.Second, minted)
	talkWithin(t, bob, 60*time.Second, minted)
	key := ts.Conversations[1]
	if key.Alias != "treasury" || key.Curve != "eddsa" || key.T != 2 || key.N != 2 {
		t.Fatalf("key row: %+v", key)
	}
	ts = alice.OpenConversation(key.ID)
	if ts.Key == nil || !ts.Key.Share {
		t.Fatalf("key detail: %+v", ts.Key)
	}

	// bob goes offline: alice's message queues, and Drop removes it.
	bob.Lock()
	talkWithin(t, alice, 20*time.Second, func(ts shell.TalkState) bool { return !ts.MajorityReachable })
	ts = alice.SendMessage("anyone?")
	if ts.Refused != nil {
		t.Fatalf("send: %+v", ts.Refused)
	}
	ts = talkWithin(t, alice, 10*time.Second, hasText("anyone?"))
	q := ts.Items[len(ts.Items)-1]
	if !q.Queued || !q.Droppable {
		t.Fatalf("queued: %+v", q)
	}
	ts = alice.DropQueued(q.MsgID)
	if ts.Problem != nil || hasText("anyone?")(ts) {
		t.Fatalf("drop: %+v %+v", ts.Problem, ts.Items)
	}
}
