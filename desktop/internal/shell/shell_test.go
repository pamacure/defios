package shell_test

import (
	"testing"

	"defios.com/desktop/internal/prefs"
	"defios.com/desktop/internal/shell"
)

func TestNoNodeRoutesToOnboardingStepOne(t *testing.T) {
	r := newRig(t)
	r.noNode = true
	if st := r.start(false).State(); st.View != shell.ViewNoNode {
		t.Fatalf("view = %s; want %s", st.View, shell.ViewNoNode)
	}
}

func TestANodeWithNoGroupRoutesToStepTwoUntilOneExists(t *testing.T) {
	r := newRig(t)
	s := r.start(false)
	if st := s.State(); st.View != shell.ViewNoGroup {
		t.Fatalf("view = %s; want %s", st.View, shell.ViewNoGroup)
	}
	r.node.groups = []string{"alpha"} // `defios group new` ran meanwhile
	if st := s.Refresh(); st.View != shell.ViewLocked {
		t.Fatalf("after Refresh view = %s; want locked", st.View)
	}
}

func TestLockScreenShowsTheNodeNameAndPreselectsTheLastServedGroup(t *testing.T) {
	r := newRig(t, "alpha", "beta")
	r.prefs.p.LastGroup = "beta"
	st := r.start(false).State()
	if st.View != shell.ViewLocked || st.Node.Name != "laptop" || st.Selected != "beta" || len(st.Groups) != 2 {
		t.Fatalf("state = %+v", st)
	}
}

func TestLockScreenWithNoLastGroupPreselectsTheFirst(t *testing.T) {
	r := newRig(t, "alpha", "beta")
	if st := r.start(false).State(); st.Selected != "alpha" {
		t.Fatalf("Selected = %q; want alpha", st.Selected)
	}
}

func TestAWrongPassphraseStaysOnTheLockScreen(t *testing.T) {
	r := newRig(t, "alpha")
	st := r.start(false).Unlock("alpha", "nope")
	if st.View != shell.ViewLocked || problem(st) != "bad-passphrase" {
		t.Fatalf("state = %+v", st)
	}
}

func TestUnlockServesTheGroupAndRemembersIt(t *testing.T) {
	r := newRig(t, "alpha", "beta")
	st := r.start(false).Unlock("beta", pw)
	if st.View != shell.ViewServed || st.Served != "beta" || st.Problem != nil {
		t.Fatalf("state = %+v", st)
	}
	if r.prefs.get().LastGroup != "beta" {
		t.Fatalf("LastGroup = %q; want beta", r.prefs.get().LastGroup)
	}
}

func TestLockReturnsToTheLockScreen(t *testing.T) {
	r := newRig(t, "alpha")
	s := r.start(false)
	s.Unlock("alpha", pw)
	if st := s.Lock(); st.View != shell.ViewLocked || st.Served != "" {
		t.Fatalf("state = %+v", st)
	}
	if r.node.Status().Serving {
		t.Fatal("Node still serving after Lock")
	}
}

func TestServingThatStopsOnItsOwnShowsTheReasonOnTheLockScreen(t *testing.T) {
	r := newRig(t, "alpha")
	s := r.start(false)
	s.Unlock("alpha", pw)
	r.node.stop(errBoom)
	st := eventually(t, s, view(shell.ViewLocked))
	if st.Problem == nil || st.Problem.Message == "" {
		t.Fatalf("no reason shown: %+v", st)
	}
}

func TestServedElsewhereRetriesUntilTheOtherProcessStops(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.elsewhere = true
	s := r.start(false)
	if st := s.State(); st.View != shell.ViewServedElsewhere {
		t.Fatalf("view = %s; want served-elsewhere", st.View)
	}
	r.node.setElsewhere(false)
	eventually(t, s, view(shell.ViewLocked))
	if st := s.Unlock("alpha", pw); st.View != shell.ViewServed {
		t.Fatalf("unlock after the other process stopped: %+v", st)
	}
}

func TestUnlockThatFindsAnotherProcessServingShowsServedElsewhere(t *testing.T) {
	r := newRig(t, "alpha")
	s := r.start(false)
	r.node.setElsewhere(true)
	if st := s.Unlock("alpha", pw); st.View != shell.ViewServedElsewhere {
		t.Fatalf("view = %s; want served-elsewhere", st.View)
	}
}

func TestSwitchServesTheOtherGroupWithoutThePassphrase(t *testing.T) {
	r := newRig(t, "alpha", "beta")
	s := r.start(false)
	s.Unlock("alpha", pw)
	st := s.Switch("beta")
	if st.View != shell.ViewServed || st.Served != "beta" {
		t.Fatalf("state = %+v", st)
	}
	if r.prefs.get().LastGroup != "beta" {
		t.Fatalf("LastGroup = %q; want beta", r.prefs.get().LastGroup)
	}
}

func TestASwitchThatFailsLeavesTheLockScreenWithTheReason(t *testing.T) {
	r := newRig(t, "alpha")
	s := r.start(false)
	s.Unlock("alpha", pw)
	if st := s.Switch("ghost"); st.View != shell.ViewLocked || problem(st) != "unknown-group" {
		t.Fatalf("state = %+v", st)
	}
}

func TestTheEditTargetDefaultsToTheServedGroupAndIsKept(t *testing.T) {
	r := newRig(t, "alpha", "beta")
	s := r.start(false)
	if st := s.Unlock("alpha", pw); st.EditGroup != "alpha" || st.EditGroupServed != true {
		t.Fatalf("default edit target: %+v", st)
	}
	st := s.SetEditGroup("beta")
	if st.EditGroup != "beta" || st.EditGroupServed || st.Served != "alpha" {
		t.Fatalf("after choosing beta: %+v", st)
	}
	if st := s.State(); st.EditGroup != "beta" {
		t.Fatalf("edit target not kept: %+v", st)
	}
}

func TestStartAtLoginRegistersWithTheOS(t *testing.T) {
	r := newRig(t, "alpha")
	s := r.start(false)
	if st := s.SetStartAtLogin(true); !st.Prefs.StartAtLogin || !r.autostart.enabled() || !r.prefs.get().StartAtLogin {
		t.Fatalf("on: %+v", st)
	}
	if st := s.SetStartAtLogin(false); st.Prefs.StartAtLogin || r.autostart.enabled() {
		t.Fatalf("off: %+v", st)
	}
}

func TestTheKeychainKeepsThePassphraseUntilTurnedOff(t *testing.T) {
	r := newRig(t, "alpha")
	s := r.start(false)
	s.Unlock("alpha", pw)
	if st := s.SetKeychain(true); !st.Prefs.Keychain || !r.keychain.has("n0de") {
		t.Fatalf("on: %+v", st)
	}
	if st := s.SetKeychain(false); st.Prefs.Keychain || r.keychain.has("n0de") {
		t.Fatalf("off: %+v; stored = %v", st, r.keychain.has("n0de"))
	}
}

func TestTheKeychainForgetsAfterLockOnlyWhenTurnedOff(t *testing.T) {
	r := newRig(t, "alpha")
	s := r.start(false)
	s.Unlock("alpha", pw)
	s.SetKeychain(true)
	s.Lock()
	if !r.keychain.has("n0de") {
		t.Fatal("Lock removed the stored passphrase")
	}
	if st := s.SetKeychain(false); r.keychain.has("n0de") || st.Prefs.Keychain {
		t.Fatal("turning the keychain off while locked kept the passphrase")
	}
}

// The four start-at-login / keychain combinations, launched at login.

func TestAtLoginWithStartAtLoginOffDoesNothingSpecial(t *testing.T) {
	r := newRig(t, "alpha")
	r.prefs.p = prefs.Prefs{LastGroup: "alpha"}
	if st := r.start(true).State(); st.View != shell.ViewLocked || r.notifications() != 0 {
		t.Fatalf("state = %+v, %d notifications", st, r.notifications())
	}
}

func TestAtLoginWithoutTheKeychainNotifiesThatTheNodeIsLocked(t *testing.T) {
	r := newRig(t, "alpha")
	r.prefs.p = prefs.Prefs{StartAtLogin: true, LastGroup: "alpha"}
	if st := r.start(true).State(); st.View != shell.ViewLocked || r.notifications() != 1 {
		t.Fatalf("state = %+v, %d notifications", st, r.notifications())
	}
}

func TestAtLoginWithTheKeychainUnlocksTheLastServedGroup(t *testing.T) {
	r := newRig(t, "alpha", "beta")
	r.prefs.p = prefs.Prefs{StartAtLogin: true, Keychain: true, LastGroup: "beta"}
	r.keychain.Set("n0de", pw)
	st := r.start(true).State()
	if st.View != shell.ViewServed || st.Served != "beta" || r.notifications() != 0 {
		t.Fatalf("state = %+v, %d notifications", st, r.notifications())
	}
}

func TestAtLoginWithAStaleKeychainPassphraseNotifiesThatTheNodeIsLocked(t *testing.T) {
	r := newRig(t, "alpha")
	r.prefs.p = prefs.Prefs{StartAtLogin: true, Keychain: true, LastGroup: "alpha"}
	r.keychain.Set("n0de", "old passphrase")
	st := r.start(true).State()
	if st.View != shell.ViewLocked || r.notifications() != 1 {
		t.Fatalf("state = %+v, %d notifications", st, r.notifications())
	}
}

func TestAtLoginWithTheKeychainWaitsOutAnotherProcessThenUnlocks(t *testing.T) {
	r := newRig(t, "alpha")
	r.prefs.p = prefs.Prefs{StartAtLogin: true, Keychain: true, LastGroup: "alpha"}
	r.keychain.Set("n0de", pw)
	r.node.elsewhere = true
	s := r.start(true)
	r.node.setElsewhere(false)
	eventually(t, s, view(shell.ViewServed))
}

func TestMutedNotificationsPostNothingAtLogin(t *testing.T) {
	r := newRig(t, "alpha")
	r.prefs.p = prefs.Prefs{StartAtLogin: true, MuteNotifications: true, LastGroup: "alpha"}
	r.start(true)
	if r.notifications() != 0 {
		t.Fatalf("%d notifications; want none", r.notifications())
	}
}
