package shell

import (
	"context"
	"errors"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/node"
	"defios.com/internal/secret"
)

// The Settings page's Group part, for the edit-target Group: its settings
// form, its Roster, Node Usage and Token Usage. Node Usage and Token
// Usage are separate sections, never mixed (GLOSSARY.md).

// SettingsNode is what the Settings page needs of a Node besides the
// rest of Node: NodeID, InspectGroup and SetSecret.
type SettingsNode interface {
	GroupSettings(ctx context.Context, group string) (node.GroupSettings, error)
	EditGroupSettings(ctx context.Context, group string, e node.SettingsEdit) (node.GroupSettings, error)
	Usage(ctx context.Context, group string) (node.Usage, error)
	TokenUsage(ctx context.Context, group string) (node.TokenUsage, error)
}

// GroupSettingsView is the Group part of the Settings page. Each section
// carries its own Problem, so one failing leaves the others shown.
type GroupSettingsView struct {
	// Group is the edit-target Group, and Served whether it is served
	// now: a change to it applies the next time it is served.
	Group  string
	Served bool

	Settings SettingsDraft
	// AuthKeySecret is the Secret the tailnet auth key reference names;
	// "" when none is named.
	AuthKeySecret string
	// Problem is why the settings could not be read or saved; Field, with
	// a save's Problem, the field it is about (a node.Field constant).
	Problem *Problem
	Field   string
	// Saved: the last save wrote the settings.
	Saved bool

	Roster        []RosterDevice
	RosterProblem *Problem

	Usage        *NodeUsage
	UsageProblem *Problem

	Tokens        []node.HandleTokens
	TokensProblem *Problem
}

// SettingsDraft is the settings form as a person fills it in. Prune is
// an age such as "90d", or "" to keep all talk. AuthKey is a new tailnet
// auth key to keep in the Vault; "" keeps the one there.
type SettingsDraft struct {
	DisplayName, HeadscaleURL, Hostname, Prune string
	AuthKey                                    string
}

// RosterDevice is one Node of the Group's Roster. Reachable means
// reachable now, and only while the Group is served.
type RosterDevice struct {
	Name, ID        string
	Self, Reachable bool
}

// NodeUsage is the Node Usage the page shows: disk for the Group and the
// whole Node, in bytes, and the serving process's use, nil when none
// serves.
type NodeUsage struct {
	GroupBytes, NodeBytes int64
	Process               *ProcessUse
}

// ProcessUse is the serving process's resident memory, in bytes, and CPU time
// since it started, in seconds; nil where the platform cannot tell.
type ProcessUse struct {
	Group      string // the Group it serves
	Memory     *uint64
	CPUSeconds *float64
	Uptime     float64 // seconds
}

// GroupSettings is the Settings page's Group part for the edit-target
// Group.
func (s *Shell) GroupSettings() GroupSettingsView {
	v, ok := s.settingsTarget()
	if !ok {
		return v
	}
	n := s.node()
	gs, err := n.GroupSettings(s.ctx, v.Group)
	if err != nil {
		v.Problem = problemOf(err)
	} else {
		v.Settings, v.AuthKeySecret = draftOf(gs), authKeySecret(gs.TailnetAuthKey)
	}
	s.fillRosterAndUsage(n, &v)
	return v
}

// SaveGroupSettings writes d as the edit-target Group's settings. A
// refused field writes nothing and names it in Field. A new auth key the
// Vault does not take leaves the rest saved, and says so on its field.
func (s *Shell) SaveGroupSettings(d SettingsDraft) GroupSettingsView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.settingsTarget()
	if !ok {
		return v
	}
	n := s.node()
	v.Settings = d
	fail := func(field string, p *Problem) GroupSettingsView {
		v.Field, v.Problem = field, p
		v.Settings.AuthKey = ""
		s.fillRosterAndUsage(n, &v)
		return v
	}
	var prune config.Age
	if d.Prune != "" {
		age, err := config.ParseAge(d.Prune)
		if err != nil {
			return fail(node.FieldPrune, &Problem{"invalid", sentence(err.Error())})
		}
		prune = age
	}
	e := node.SettingsEdit{
		DisplayName: &d.DisplayName, HeadscaleURL: &d.HeadscaleURL, Hostname: &d.Hostname, Prune: &prune,
	}
	s.mu.Lock()
	pass := s.pass
	s.mu.Unlock()
	if d.AuthKey != "" && pass == "" {
		return fail(node.FieldTailnetAuthKey, problemOf(node.ErrLocked))
	}
	gs, err := n.EditGroupSettings(s.ctx, v.Group, e)
	if err != nil {
		field := ""
		if inv := (*node.InvalidError)(nil); errors.As(err, &inv) {
			field = inv.Field
		}
		return fail(field, problemOf(err))
	}
	v.Settings, v.AuthKeySecret, v.Saved = draftOf(gs), authKeySecret(gs.TailnetAuthKey), true
	if d.AuthKey != "" {
		// The key goes in the Vault before the config names it, so the
		// config never names a Secret that is not there.
		name := v.AuthKeySecret
		if name == "" {
			name = node.TailnetAuthKeySecret // the one a new Group's names
		}
		held := func() (string, bool, error) { return pass, true, nil }
		err := n.SetSecret(s.ctx, v.Group, name, d.AuthKey, held)
		if err == nil && v.AuthKeySecret == "" {
			ref := "$" + name
			if _, err = n.EditGroupSettings(s.ctx, v.Group, node.SettingsEdit{TailnetAuthKey: &ref}); err == nil {
				v.AuthKeySecret = name
			}
		}
		if err != nil {
			v.Field = node.FieldTailnetAuthKey
			v.Problem = &Problem{"auth-key-not-saved", "The other settings were saved, but the tailnet auth key was not: " + problemOf(err).Message}
		}
	}
	s.fillRosterAndUsage(n, &v)
	return v
}

// settingsTarget is the view's Group and whether it is served; ok false,
// with nothing to show, while no Group is served in this app.
func (s *Shell) settingsTarget() (GroupSettingsView, bool) {
	st := s.State()
	v := GroupSettingsView{Group: st.EditGroup, Served: st.EditGroupServed}
	return v, s.node() != nil && st.View == ViewServed && v.Group != ""
}

// fillRosterAndUsage adds v.Group's Roster, Node Usage and Token Usage.
func (s *Shell) fillRosterAndUsage(n Node, v *GroupSettingsView) {
	if d, err := n.InspectGroup(s.ctx, v.Group); err != nil {
		v.RosterProblem = rosterProblem(err)
	} else {
		v.Roster = make([]RosterDevice, 0, len(d.Roster))
		for _, m := range d.Roster {
			v.Roster = append(v.Roster, RosterDevice{
				Name: m.DisplayName, ID: m.NodeID, Self: m.NodeID == n.NodeID(),
				Reachable: v.Served && m.Reachable,
			})
		}
	}
	if u, err := n.Usage(s.ctx, v.Group); err != nil {
		v.UsageProblem = problemOf(err)
	} else {
		v.Usage = nodeUsageOf(u)
	}
	if t, err := n.TokenUsage(s.ctx, v.Group); err != nil {
		v.TokensProblem = problemOf(err)
	} else {
		v.Tokens = t.Handles
	}
}

func rosterProblem(err error) *Problem {
	if errors.Is(err, group.ErrNoGroup) {
		return &Problem{"unformed", "This Group isn't formed yet, so it has no devices besides this one."}
	}
	return problemOf(err)
}

func draftOf(gs node.GroupSettings) SettingsDraft {
	d := SettingsDraft{DisplayName: gs.DisplayName, HeadscaleURL: gs.HeadscaleURL, Hostname: gs.Hostname}
	if gs.Prune != 0 {
		d.Prune = gs.Prune.String()
	}
	return d
}

// authKeySecret is the Secret ref names, "" for none.
func authKeySecret(ref string) string {
	name, err := secret.ParseRef(ref)
	if err != nil {
		return ""
	}
	return name
}

func nodeUsageOf(u node.Usage) *NodeUsage {
	out := &NodeUsage{NodeBytes: u.Total}
	for _, g := range u.Groups {
		out.GroupBytes += g.Bytes
	}
	if p := u.Serve; p != nil {
		use := &ProcessUse{Group: p.Alias, Memory: p.RSS, Uptime: p.Uptime.Seconds()}
		if p.UserCPU != nil || p.SystemCPU != nil {
			var cpu time.Duration
			for _, d := range []*time.Duration{p.UserCPU, p.SystemCPU} {
				if d != nil {
					cpu += *d
				}
			}
			secs := cpu.Seconds()
			use.CPUSeconds = &secs
		}
		out.Process = use
	}
	return out
}
