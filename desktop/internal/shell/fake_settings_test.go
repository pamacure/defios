package shell_test

import (
	"context"
	"errors"
	"net/url"
	"time"

	"defios.com/internal/node"
)

func (n *fakeNode) groupSettings(group string) (node.GroupSettings, error) {
	for _, g := range n.groups {
		if g == group {
			if gs, ok := n.settings[group]; ok {
				return gs, nil
			}
			return node.GroupSettings{
				DisplayName: n.name, HeadscaleURL: "https://hs.example",
				Hostname: "defios-n0de-" + group, TailnetAuthKey: "$TS_AUTHKEY",
			}, nil
		}
	}
	return node.GroupSettings{}, node.ErrUnknownGroup
}

func (n *fakeNode) GroupSettings(_ context.Context, group string) (node.GroupSettings, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.groupSettings(group)
}

// EditGroupSettings refuses, as internal/node does, a display name that
// is empty and a headscale URL that is not http(s), naming the field.
func (n *fakeNode) EditGroupSettings(_ context.Context, group string, e node.SettingsEdit) (node.GroupSettings, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	gs, err := n.groupSettings(group)
	if err != nil {
		return gs, err
	}
	if e.DisplayName != nil {
		if *e.DisplayName == "" {
			return node.GroupSettings{}, &node.InvalidError{Err: errors.New("display name must be 1–64 characters with no newlines"), Field: node.FieldDisplayName}
		}
		gs.DisplayName = *e.DisplayName
	}
	if e.HeadscaleURL != nil {
		if u, err := url.Parse(*e.HeadscaleURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return node.GroupSettings{}, &node.InvalidError{Err: errors.New("headscale URL must be an http:// or https:// URL"), Field: node.FieldHeadscaleURL}
		}
		gs.HeadscaleURL = *e.HeadscaleURL
	}
	if e.Hostname != nil {
		gs.Hostname = *e.Hostname
	}
	if e.TailnetAuthKey != nil {
		gs.TailnetAuthKey = *e.TailnetAuthKey
	}
	if e.Prune != nil {
		gs.Prune = *e.Prune
	}
	if n.settings == nil {
		n.settings = map[string]node.GroupSettings{}
	}
	n.settings[group] = gs
	return gs, nil
}

// Usage is 1 KiB per Group and 10 KiB for the Node, served by a process
// using 64 MiB and 2.5 s of CPU while a Group serves.
func (n *fakeNode) Usage(_ context.Context, group string) (node.Usage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	u := node.Usage{Total: 10 << 10, Groups: []node.GroupUsage{{Alias: group, Bytes: 1 << 10}}}
	if n.served != "" {
		rss, user, sys := uint64(64<<20), 2*time.Second, 500*time.Millisecond
		u.Serve = &node.ProcessUsage{Alias: n.served, RSS: &rss, UserCPU: &user, SystemCPU: &sys, Uptime: time.Minute}
	}
	return u, nil
}

func (n *fakeNode) TokenUsage(_ context.Context, group string) (node.TokenUsage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return node.TokenUsage{Handles: append([]node.HandleTokens{}, n.tokens[group]...)}, nil
}
