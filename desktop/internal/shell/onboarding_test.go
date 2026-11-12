package shell_test

import (
	"testing"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
)

// Onboarding: step 1, create or restore the Node.

func TestCreatingANodeShowsProgressThenStepTwoWithThePassphraseHeld(t *testing.T) {
	r := newRig(t)
	r.noNode = true
	s := r.start(false)
	st := s.Create("work-laptop", pw)
	if st.View != shell.ViewNoGroup || st.Node.Name != "work-laptop" || !st.PassphraseHeld || st.Problem != nil {
		t.Fatalf("after Create: %+v", st)
	}
	if len(r.progress) != 2 || r.progress[0] != "generating ECDSA preparams" || r.progress[1] != "generating ECDSA preparams|done" {
		t.Fatalf("progress as Create ran = %q", r.progress)
	}
	if len(st.Progress) != 0 {
		t.Fatalf("progress kept after Create: %q", st.Progress)
	}
}

func TestCreateRefusesABadDisplayNameOnStepOne(t *testing.T) {
	r := newRig(t)
	r.noNode = true
	s := r.start(false)
	if p := shell.CheckDisplayName("two\nlines"); p == nil || p.Code != "invalid" {
		t.Fatalf("CheckDisplayName = %+v; want invalid", p)
	}
	if p := shell.CheckDisplayName("work-laptop"); p != nil {
		t.Fatalf("CheckDisplayName of a good name = %+v", p)
	}
	if st := s.Create("", pw); st.View != shell.ViewNoNode || problem(st) != "invalid" {
		t.Fatalf("empty name: %+v", st)
	}
	if st := s.Create("laptop", ""); st.View != shell.ViewNoNode || problem(st) != "empty-passphrase" {
		t.Fatalf("empty passphrase: %+v", st)
	}
}

func TestRestoringWithTheWrongPassphraseStaysOnStepOne(t *testing.T) {
	r := newRig(t)
	r.noNode, r.backupGroups = true, []string{"alpha"}
	s := r.start(false)
	if st := s.CheckBackup("/somewhere/else"); problem(st) != "not-a-file" {
		t.Fatalf("CheckBackup of a bad path: %+v", st)
	}
	if st := s.CheckBackup(backupFile); st.Problem != nil {
		t.Fatalf("CheckBackup: %+v", st)
	}
	st := s.Restore(backupFile, "nope")
	if st.View != shell.ViewNoNode || problem(st) != "bad-passphrase" || st.Problem.Message != "That passphrase does not open this Backup." {
		t.Fatalf("wrong passphrase: %+v", st)
	}
}

func TestARestoredNodeWithGroupsOpensOnTheLockScreen(t *testing.T) {
	r := newRig(t)
	r.noNode, r.backupGroups = true, []string{"alpha"}
	st := r.start(false).Restore(backupFile, pw)
	if st.View != shell.ViewLocked || st.Selected != "alpha" || st.PassphraseHeld || st.Restored == nil {
		t.Fatalf("after Restore: %+v", st)
	}
}

func TestARestoredNodeWithNoGroupOpensOnStepTwo(t *testing.T) {
	r := newRig(t)
	r.noNode = true
	st := r.start(false).Restore(backupFile, pw)
	if st.View != shell.ViewNoGroup || !st.PassphraseHeld || st.Restored == nil {
		t.Fatalf("after Restore: %+v", st)
	}
}

// Step 2, tailnet.

func TestStepTwoMakesTheGroupWithItsAuthKeyAsASecretAndServesItForStepThree(t *testing.T) {
	r := newRig(t)
	r.noNode = true
	s := r.start(false)
	s.Create("laptop", pw)
	st := s.AddGroup("alpha", "https://hs.example", "tskey-auth-1", "")
	if st.View != shell.ViewForming || st.Forming == nil || st.Forming.Group != "alpha" || st.Forming.Back != "" || st.Served != "alpha" {
		t.Fatalf("after step 2: %+v", st)
	}
	if got := r.node.secret("alpha", node.TailnetAuthKeySecret); got != "tskey-auth-1" {
		t.Fatalf("auth key Secret = %q", got)
	}
}

func TestStepTwoRefusesBadInputBeforeMakingAnything(t *testing.T) {
	r := newRig(t)
	s := r.start(false) // a Node with no Group, the passphrase not held
	if st := s.State(); st.View != shell.ViewNoGroup || st.PassphraseHeld {
		t.Fatalf("start: %+v", st)
	}
	for _, c := range []struct{ alias, url, key, pass, want string }{
		{"alpha", "https://hs.example", "tskey", "", "no-passphrase"},
		{"alpha", "https://hs.example", "tskey", "nope", "bad-passphrase"},
		{"alpha", "https://hs.example", "", pw, "no-auth-key"},
		{"Not An Alias", "https://hs.example", "tskey", pw, "invalid"},
		{"alpha", "hs.example", "tskey", pw, "invalid"},
	} {
		st := s.AddGroup(c.alias, c.url, c.key, c.pass)
		if st.View != shell.ViewNoGroup || problem(st) != c.want {
			t.Errorf("%+v: %+v; want %s", c, st, c.want)
		}
	}
	if len(r.node.groups) != 0 {
		t.Fatalf("Groups made: %v", r.node.groups)
	}
	if st := s.AddGroup("alpha", "https://hs.example", "tskey", pw); st.View != shell.ViewForming {
		t.Fatalf("with the passphrase typed: %+v", st)
	}
}

func TestAddGroupFromAServedGroupNeedsNoPassphraseAndCanGoBack(t *testing.T) {
	r := newRig(t, "beta")
	s := r.start(false)
	s.Unlock("beta", pw)
	st := s.AddGroup("alpha", "https://hs.example", "tskey", "")
	if st.View != shell.ViewForming || st.Served != "alpha" || st.Forming.Back != "beta" {
		t.Fatalf("after adding: %+v", st)
	}
	// Going back serves beta; alpha stays, unformed, in the Group list.
	st = s.Switch("beta")
	if st.View != shell.ViewServed || st.Served != "beta" || len(st.Groups) != 2 || st.Groups[1].Status != "unformed" {
		t.Fatalf("after going back: %+v", st)
	}
}

// Step 3, Group connection.

// forming is a Shell on step 3 for a new Group alpha.
func forming(t *testing.T) (*rig, *shell.Shell) {
	r := newRig(t)
	r.noNode = true
	s := r.start(false)
	s.Create("laptop", pw)
	if st := s.AddGroup("alpha", "https://hs.example", "tskey", ""); st.View != shell.ViewForming {
		t.Fatalf("not forming: %+v", st)
	}
	return r, s
}

func TestProposingToAnUnreachedDeviceOffersARetryThatThenWaitsForItToAccept(t *testing.T) {
	r, s := forming(t)
	r.node.mu.Lock()
	r.node.discovered = []node.DiscoveredNode{{NodeID: "n0de"}, {NodeID: "b0b", DisplayName: "bob", Hostname: "defios-b0b-alpha"}}
	r.node.unreached = map[string]bool{"b0b": true}
	r.node.mu.Unlock()

	st := s.Discover()
	if f := st.Forming; len(f.Discovered) != 1 || f.Discovered[0] != (shell.NodeInfo{ID: "b0b", Name: "bob"}) {
		t.Fatalf("discovered (this device left out): %+v", f.Discovered)
	}
	st = s.Propose([]string{"b0b"})
	if f := st.Forming; st.View != shell.ViewForming || problem(st) != "not-reached" || len(f.NotReached) != 1 || f.NotReached[0] != "b0b" ||
		len(f.Proposed) != 1 || f.Proposed[0].Name != "bob" || f.Proposal != nil {
		t.Fatalf("unreached: %+v %+v", st, st.Forming)
	}

	r.node.mu.Lock()
	r.node.unreached = nil
	r.node.mu.Unlock()
	st = s.Propose([]string{"b0b"})
	if st.Problem != nil || st.Forming.Proposal == nil || !st.Forming.Proposal.Mine || len(st.Forming.NotReached) != 0 {
		t.Fatalf("retried: %+v %+v", st, st.Forming)
	}
	if got := r.node.proposed[1]; len(got) != 2 || got[0] != "n0de" || got[1] != "b0b" {
		t.Fatalf("proposed %v; want this device and bob", got)
	}
	r.node.acceptBy("alpha", "b0b")
	eventually(t, s, view(shell.ViewServed))
}

func TestAProposalFromAnotherNodeShowsItsRosterAndAcceptingItFormsTheGroup(t *testing.T) {
	r, s := forming(t)
	r.node.receive("alpha", "b0b", "b0b", "n0de")
	st := eventually(t, s, func(st shell.State) bool { return st.Forming != nil && st.Forming.Proposal != nil })
	p := st.Forming.Proposal
	if p.Mine || p.Accepted || p.Initiator != "b0b" || len(p.Roster) != 2 || p.Roster[0] != (shell.ProposedNode{ID: "b0b", Name: "name-b0b", Accepted: true}) || !p.Roster[1].Self {
		t.Fatalf("invite: %+v", p)
	}
	if st := s.Accept(); st.View != shell.ViewServed || st.Served != "alpha" {
		t.Fatalf("after Accept: %+v", st)
	}
}

func TestAnAcceptThatDidNotReachEveryoneOffersARetry(t *testing.T) {
	r, s := forming(t)
	r.node.undelivered = []string{"b0b"}
	r.node.receive("alpha", "b0b", "b0b", "n0de")
	eventually(t, s, func(st shell.State) bool { return st.Forming != nil && st.Forming.Proposal != nil })
	st := s.Accept()
	if st.View != shell.ViewForming || len(st.Forming.Undelivered) != 1 || !st.Forming.Proposal.Accepted {
		t.Fatalf("undelivered: %+v %+v", st, st.Forming)
	}
	r.node.mu.Lock()
	r.node.undelivered = nil
	r.node.mu.Unlock()
	if st := s.Accept(); st.View != shell.ViewServed {
		t.Fatalf("retried: %+v", st)
	}
}

func TestDecliningAProposalFreesTheGroupForAnother(t *testing.T) {
	r, s := forming(t)
	r.node.receive("alpha", "b0b", "b0b", "n0de")
	eventually(t, s, func(st shell.State) bool { return st.Forming != nil && st.Forming.Proposal != nil })
	st := s.Decline("wrong tailnet")
	if st.View != shell.ViewForming || st.Forming.Proposal != nil || st.Problem != nil {
		t.Fatalf("after Decline: %+v %+v", st, st.Forming)
	}
}
