package node_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/vault"
)

func TestNewGroupScaffoldsAnUnformedGroup(t *testing.T) {
	h := open(t, create(t, "laptop"))
	g, err := h.NewGroup(node.GroupSpec{Alias: "alpha", HeadscaleURL: "https://hs.example"})
	if err != nil {
		t.Fatal(err)
	}
	// Display name and hostname default from the Node.
	if g.DisplayName != "laptop" || g.Hostname != "defios-"+h.NodeID()[:8]+"-alpha" || g.HeadscaleURL != "https://hs.example" {
		t.Fatalf("group = %+v", g)
	}
	groups, err := h.Groups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0] != (node.GroupSummary{Alias: "alpha", Status: "unformed"}) {
		t.Fatalf("groups = %+v", groups)
	}
	if err := h.Check("alpha"); err != nil {
		t.Fatalf("a scaffolded Group cannot be served: %v", err)
	}
}

func TestNewGroupRefusals(t *testing.T) {
	h := open(t, create(t, "laptop"))
	if _, err := h.NewGroup(node.GroupSpec{Alias: "alpha", HeadscaleURL: "https://hs.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.NewGroup(node.GroupSpec{Alias: "alpha", HeadscaleURL: "https://hs.example"}); !errors.Is(err, node.ErrGroupExists) {
		t.Fatalf("same alias: %v; want ErrGroupExists", err)
	}
	var invalid *node.InvalidError
	for _, spec := range []node.GroupSpec{
		{Alias: "Not An Alias", HeadscaleURL: "https://hs.example"},
		{Alias: "beta"},
		{Alias: "beta", HeadscaleURL: "https://hs.example", DisplayName: "two\nlines"},
	} {
		if _, err := h.NewGroup(spec); !errors.As(err, &invalid) {
			t.Errorf("%+v: %v; want InvalidError", spec, err)
		}
	}
}

func TestTheTailnetAuthKeySecretReachesTheTailnet(t *testing.T) {
	p := create(t, "laptop")
	var authKey string
	h, err := node.Open(p, node.Options{
		Getenv: func(string) string { return "" },
		NewTailnet: func(o serve.TsnetOptions) serve.Tailnet {
			authKey = o.AuthKey
			return loopTailnet{}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Lock() })
	if _, err := h.NewGroup(node.GroupSpec{Alias: "alpha", HeadscaleURL: "https://hs.example"}); err != nil {
		t.Fatal(err)
	}
	if err := h.SetSecret("alpha", node.TailnetAuthKeySecret, "tskey-auth-1", "wrong passphrase"); !errors.Is(err, vault.ErrBadPassphrase) {
		t.Fatalf("SetSecret with the wrong passphrase: %v", err)
	}
	if err := h.SetSecret("gamma", node.TailnetAuthKeySecret, "tskey-auth-1", passphrase); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("SetSecret on an unknown Group: %v", err)
	}
	if err := h.SetSecret("alpha", node.TailnetAuthKeySecret, "tskey-auth-1", passphrase); err != nil {
		t.Fatal(err)
	}
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	if authKey != "tskey-auth-1" {
		t.Fatalf("tailnet joined with auth key %q", authKey)
	}
}

func TestGroupsSaysWhichIsServed(t *testing.T) {
	p := nodeDir(t)
	h := open(t, p)
	if err := h.Unlock(passphrase, "beta"); err != nil {
		t.Fatal(err)
	}
	// Served by this host, and as another process on the same Node sees it.
	for _, viewer := range []*node.Host{h, open(t, p)} {
		groups, err := viewer.Groups(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var served []string
		for _, g := range groups {
			if g.Served {
				served = append(served, g.Alias)
			}
		}
		if len(groups) != 2 || strings.Join(served, ",") != "beta" {
			t.Fatalf("groups = %+v; want alpha and beta, beta served", groups)
		}
	}
}
