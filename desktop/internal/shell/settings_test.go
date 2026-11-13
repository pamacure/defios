package shell_test

import (
	"reflect"
	"testing"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
	"defios.com/internal/vault"
)

// formedWith forms group on the fake with a peer that has accepted.
func (n *fakeNode) formedWith(group string, peer node.GroupNode) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f := n.form(group)
	f.groupID, f.initiator = "g1d", n.NodeID()
	f.roster = []node.GroupNode{{NodeID: n.NodeID(), DisplayName: n.name, Accepted: true, Reachable: true}, peer}
}

func TestSettingsShowTheServedGroupsSettingsRosterAndBothUsages(t *testing.T) {
	r := newRig(t, "alpha", "beta")
	r.node.formedWith("alpha", node.GroupNode{NodeID: "pe3r", DisplayName: "phone", Accepted: true})
	r.node.tokens = map[string][]node.HandleTokens{"alpha": {{Handle: "auditor", Input: 947, Output: 3}}}
	s := r.start(false)
	s.Unlock("alpha", pw)

	v := s.GroupSettings()
	if v.Group != "alpha" || !v.Served || v.Problem != nil {
		t.Fatalf("view: %+v", v)
	}
	want := shell.SettingsDraft{DisplayName: "laptop", HeadscaleURL: "https://hs.example", Hostname: "defios-n0de-alpha"}
	if v.Settings != want || v.AuthKeySecret != "TS_AUTHKEY" {
		t.Fatalf("settings %+v, auth key Secret %q", v.Settings, v.AuthKeySecret)
	}
	wantRoster := []shell.RosterDevice{
		{Name: "laptop", ID: "n0de", Self: true, Reachable: true},
		{Name: "phone", ID: "pe3r"},
	}
	if v.Roster != nil {
		t.Fatalf("the settings carry the Roster: %+v", v.Roster)
	}
	if r := s.GroupRoster(); r.Group != "alpha" || !reflect.DeepEqual(r.Roster, wantRoster) || r.RosterProblem != nil {
		t.Fatalf("roster %+v, %+v", r.Roster, r.RosterProblem)
	}
	u := v.Usage
	if u == nil || u.GroupBytes != 1<<10 || u.NodeBytes != 10<<10 || u.Process == nil ||
		*u.Process.Memory != 64<<20 || *u.Process.CPUSeconds != 2.5 || u.Process.Group != "alpha" {
		t.Fatalf("Node Usage %+v %+v", u, u.Process)
	}
	if want := []node.HandleTokens{{Handle: "auditor", Input: 947, Output: 3}}; !reflect.DeepEqual(v.Tokens, want) {
		t.Fatalf("Token Usage %+v", v.Tokens)
	}
}

func TestAGroupNotServedShowsNoDeviceReachableAndAnUnformedOneNoRoster(t *testing.T) {
	r := newRig(t, "alpha", "beta", "gamma")
	r.node.formedWith("beta", node.GroupNode{NodeID: "pe3r", DisplayName: "phone", Accepted: true, Reachable: true})
	r.node.form("gamma")
	s := r.start(false)
	s.Unlock("alpha", pw)

	s.SetEditGroup("beta")
	v := s.GroupRoster()
	if v.Group != "beta" || v.Served || len(v.Roster) != 2 {
		t.Fatalf("beta: %+v", v)
	}
	for _, d := range v.Roster {
		if d.Reachable {
			t.Fatalf("%s reachable in a Group not served", d.Name)
		}
	}
	s.SetEditGroup("gamma")
	if v := s.GroupRoster(); v.Roster != nil || v.RosterProblem == nil || v.RosterProblem.Code != "unformed" {
		t.Fatalf("gamma: %+v %+v", v.Roster, v.RosterProblem)
	}
}

func TestSettingsShowNothingWhileLocked(t *testing.T) {
	r := newRig(t, "alpha")
	s := r.start(false)
	if v := s.GroupSettings(); v.Group != "" || v.Roster != nil || v.Usage != nil {
		t.Fatalf("locked: %+v", v)
	}
	if v := s.GroupRoster(); v.Group != "" || v.Roster != nil {
		t.Fatalf("locked roster: %+v", v)
	}
	if v := s.SaveGroupSettings(shell.SettingsDraft{DisplayName: "x"}); v.Saved {
		t.Fatalf("saved while locked: %+v", v)
	}
}

func TestSavingTheSettingsWritesThemAndKeepsAPruneAge(t *testing.T) {
	r := newRig(t, "alpha")
	s := r.start(false)
	s.Unlock("alpha", pw)
	d := shell.SettingsDraft{DisplayName: "desk", HeadscaleURL: "https://other.example", Hostname: "desk-alpha", Prune: "30d"}
	v := s.SaveGroupSettings(d)
	if !v.Saved || v.Problem != nil || v.Settings != d {
		t.Fatalf("save: %+v", v)
	}
	if v := s.GroupSettings(); v.Settings != d {
		t.Fatalf("read back %+v", v.Settings)
	}
	d.Prune = ""
	if v := s.SaveGroupSettings(d); v.Settings.Prune != "" {
		t.Fatalf("no prune: %+v", v.Settings)
	}
}

func TestARefusedFieldIsNamedAndNothingIsWritten(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(*shell.SettingsDraft)
		field string
	}{
		{"headscale URL", func(d *shell.SettingsDraft) { d.HeadscaleURL = "headscale.example" }, node.FieldHeadscaleURL},
		{"display name", func(d *shell.SettingsDraft) { d.DisplayName = "" }, node.FieldDisplayName},
		{"prune age", func(d *shell.SettingsDraft) { d.Prune = "soon" }, node.FieldPrune},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, "alpha")
			s := r.start(false)
			s.Unlock("alpha", pw)
			before := s.GroupSettings().Settings
			d := before
			d.Hostname = "changed"
			c.edit(&d)
			v := s.SaveGroupSettings(d)
			if v.Saved || v.Problem == nil || v.Problem.Code != "invalid" || v.Field != c.field {
				t.Fatalf("save: %+v %+v", v, v.Problem)
			}
			if v.Settings != d {
				t.Fatalf("the form lost what was typed: %+v", v.Settings)
			}
			if after := s.GroupSettings().Settings; after != before {
				t.Fatalf("a refused save wrote %+v", after)
			}
		})
	}
}

func TestANewAuthKeyIsKeptInTheVaultUnderTheSecretTheSettingsName(t *testing.T) {
	r := newRig(t, "alpha", "beta")
	r.node.settings = map[string]node.GroupSettings{"beta": {DisplayName: "laptop", HeadscaleURL: "https://hs.example", Hostname: "h"}}
	s := r.start(false)
	s.Unlock("alpha", pw)

	d := s.GroupSettings().Settings
	d.AuthKey = "tskey-auth-new"
	v := s.SaveGroupSettings(d)
	if !v.Saved || v.Settings.AuthKey != "" || r.node.secret("alpha", "TS_AUTHKEY") != "tskey-auth-new" {
		t.Fatalf("save: %+v; Secret %q", v, r.node.secret("alpha", "TS_AUTHKEY"))
	}

	// A Group naming no Secret gets the one a new Group names.
	s.SetEditGroup("beta")
	d = s.GroupSettings().Settings
	d.AuthKey = "tskey-auth-beta"
	if v := s.SaveGroupSettings(d); !v.Saved || v.AuthKeySecret != node.TailnetAuthKeySecret || r.node.secret("beta", node.TailnetAuthKeySecret) != "tskey-auth-beta" {
		t.Fatalf("beta: %+v", v)
	}
}

// An auth key the Vault does not take leaves the other settings saved,
// says so on the auth key's field, and names no Secret that is not there.
func TestAnAuthKeyNotKeptSaysSoAndNamesNoMissingSecret(t *testing.T) {
	r := newRig(t, "alpha", "beta")
	r.node.settings = map[string]node.GroupSettings{"beta": {DisplayName: "laptop", HeadscaleURL: "https://hs.example", Hostname: "h"}}
	r.node.secretErr = vault.ErrBusy
	s := r.start(false)
	s.Unlock("alpha", pw)
	s.SetEditGroup("beta")

	d := s.GroupSettings().Settings
	d.DisplayName, d.AuthKey = "desk", "tskey-auth-beta"
	v := s.SaveGroupSettings(d)
	if v.Field != node.FieldTailnetAuthKey || v.Problem == nil || v.Problem.Code != "auth-key-not-saved" {
		t.Fatalf("save: %+v %+v", v, v.Problem)
	}
	after := s.GroupSettings()
	if after.Settings.DisplayName != "desk" || after.AuthKeySecret != "" {
		t.Fatalf("after: %+v, auth key Secret %q", after.Settings, after.AuthKeySecret)
	}
}
