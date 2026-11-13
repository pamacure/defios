package shell_test

import (
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
)

// reach publishes alpha served with laptop and bob, bob reachable or not.
func reach(r *rig, bob bool) {
	r.node.mu.Lock()
	defer r.node.mu.Unlock()
	r.node.publish(node.Status{Group: "alpha", Serving: true, TailnetUp: true, MajorityReachable: bob,
		Roster: []node.RosterNode{{NodeID: r.node.NodeID(), DisplayName: "laptop", Reachable: true}, {NodeID: "b0b", DisplayName: "bob", Reachable: bob}}})
}

func TestAGroupJustServedIsConnectingUntilEveryDeviceIsReachable(t *testing.T) {
	r := newRig(t, "alpha")
	s := r.start(false)
	if st := s.Unlock("alpha", pw); st.View != shell.ViewServed || !st.Connecting {
		t.Fatalf("unlocked: %+v", st)
	}
	reach(r, false)
	st := eventually(t, s, func(st shell.State) bool { return st.RosterSize == 2 })
	if !st.Connecting || st.Online != 1 {
		t.Fatalf("1 of 2: %+v", st)
	}
	reach(r, true)
	eventually(t, s, func(st shell.State) bool { return st.Online == 2 && !st.Connecting })

	// A device dropping later is shown as it is, not waited on again.
	reach(r, false)
	st = eventually(t, s, func(st shell.State) bool { return st.Online == 1 })
	if st.Connecting {
		t.Fatalf("connecting again: %+v", st)
	}
}

func TestConnectingEndsOnceTheWindowPassesWithADeviceStillOffline(t *testing.T) {
	r := newRig(t, "alpha")
	r.connectWindow = 30 * time.Millisecond
	s := r.start(false)
	s.Unlock("alpha", pw)
	reach(r, false)
	st := eventually(t, s, func(st shell.State) bool { return st.RosterSize == 2 && !st.Connecting })
	if st.Online != 1 || st.View != shell.ViewServed {
		t.Fatalf("after the window: %+v", st)
	}
}

func TestContinueNowAndLockEndConnecting(t *testing.T) {
	r := newRig(t, "alpha", "beta")
	s := r.start(false)
	s.Unlock("alpha", pw)
	if st := s.ContinueNow(); st.Connecting || st.View != shell.ViewServed {
		t.Fatalf("continue now: %+v", st)
	}
	if st := s.Switch("beta"); !st.Connecting {
		t.Fatalf("a Group switched to connects anew: %+v", st)
	}
	if st := s.Lock(); st.Connecting {
		t.Fatalf("locked: %+v", st)
	}
}
