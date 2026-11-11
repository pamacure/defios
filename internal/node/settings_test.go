package node_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/node"
)

// A Group's settings read as group new wrote them, are edited field by
// field with the Group not served, and what the config would refuse
// writes nothing.
func TestAGroupsSettingsAreReadAndEdited(t *testing.T) {
	h, err := node.Open(nodeDir(t), node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := h.GroupSettings(ctx, "alpha")
	want := node.GroupSettings{
		DisplayName:    "laptop",
		HeadscaleURL:   "https://hs.example",
		Hostname:       "defios-" + h.NodeID()[:8] + "-alpha",
		TailnetAuthKey: "$TS_AUTHKEY",
	}
	if err != nil || got != want {
		t.Fatalf("settings: %#v, %v\nwant %#v", got, err, want)
	}

	got, err = h.EditGroupSettings(ctx, "alpha", node.SettingsEdit{
		DisplayName: ptr("desk"),
		Prune:       ptr(config.Age(90 * 24 * time.Hour)),
	})
	want.DisplayName, want.Prune = "desk", config.Age(90*24*time.Hour)
	if err != nil || got != want {
		t.Fatalf("edit: %#v, %v\nwant %#v", got, err, want)
	}
	toml, err := h.GroupConfigTOML(ctx, "alpha")
	if err != nil || !strings.Contains(toml, `prune = "90d"`) || !strings.Contains(toml, `display_name = "desk"`) {
		t.Fatalf("config.toml after edit: %v\n%s", err, toml)
	}
	if beta, err := h.GroupSettings(ctx, "beta"); err != nil || beta.DisplayName != "laptop" {
		t.Fatalf("beta changed with alpha: %#v, %v", beta, err)
	}

	got, err = h.EditGroupSettings(ctx, "alpha", node.SettingsEdit{
		HeadscaleURL:   ptr("https://other.example"),
		Hostname:       ptr("desk-alpha"),
		TailnetAuthKey: ptr(""),
		Prune:          ptr(config.Age(0)),
	})
	want.HeadscaleURL, want.Hostname, want.TailnetAuthKey, want.Prune = "https://other.example", "desk-alpha", "", 0
	if err != nil || got != want {
		t.Fatalf("second edit: %#v, %v\nwant %#v", got, err, want)
	}

	var invalid *node.InvalidError
	for name, e := range map[string]node.SettingsEdit{
		"empty display name": {DisplayName: ptr("")},
		"long display name":  {DisplayName: ptr(strings.Repeat("x", 65))},
		"no headscale URL":   {HeadscaleURL: ptr("")},
		"no hostname":        {Hostname: ptr("")},
		"literal auth key":   {TailnetAuthKey: ptr("tskey-auth-abc")},
		"negative prune":     {Prune: ptr(config.Age(-time.Hour))},
	} {
		if _, err := h.EditGroupSettings(ctx, "alpha", e); !errors.As(err, &invalid) {
			t.Errorf("%s: %v; want an InvalidError", name, err)
		}
	}
	if after, err := h.GroupSettings(ctx, "alpha"); err != nil || after != want {
		t.Fatalf("a refused edit wrote: %#v, %v", after, err)
	}

	if _, err := h.GroupSettings(ctx, "ghost"); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("ghost: %v; want ErrUnknownGroup", err)
	}
	if _, err := h.EditGroupSettings(ctx, "ghost", node.SettingsEdit{}); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("edit ghost: %v; want ErrUnknownGroup", err)
	}
}
