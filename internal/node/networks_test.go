package node_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"defios.com/internal/network"
	"defios.com/internal/node"
)

// A Group's Networks are added, edited field by field and removed, with
// the Group not served; what the config would refuse writes nothing.
func TestAGroupsNetworksAreAddedEditedAndRemoved(t *testing.T) {
	h, err := node.Open(nodeDir(t), node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := h.AddNetwork(ctx, "alpha", node.GroupTalk, "eth", node.NetworkEdit{
		Type: ptr("ethereum"), ChainID: ptr(uint64(1)), RPCURLs: []string{"https://rpc.example", "$ALCHEMY"},
	})
	want := node.Network{Alias: "eth", Type: "ethereum", Chain: "ethereum chain 1", Curve: "ecdsa", ChainID: 1,
		RPCURLs: []string{"https://rpc.example", "$ALCHEMY"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("add eth: %#v, %v\nwant %#v", got, err, want)
	}
	if _, err := h.AddNetwork(ctx, "alpha", node.GroupTalk, "btc", node.NetworkEdit{Type: ptr("bitcoin"), BitcoinNetwork: ptr("testnet")}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		alias string
		e     node.NetworkEdit
		want  error
	}{
		"exists":     {"eth", node.NetworkEdit{Type: ptr("ethereum"), ChainID: ptr(uint64(10))}, network.ErrExists},
		"same chain": {"mainnet", node.NetworkEdit{Type: ptr("ethereum"), ChainID: ptr(uint64(1))}, network.ErrInvalid},
		"userinfo":   {"op", node.NetworkEdit{Type: ptr("ethereum"), ChainID: ptr(uint64(10)), RPCURLs: []string{"https://u:p@rpc.example"}}, network.ErrInvalid},
		"bad type":   {"doge", node.NetworkEdit{Type: ptr("dogecoin")}, network.ErrInvalid},
	} {
		if _, err := h.AddNetwork(ctx, "alpha", node.GroupTalk, c.alias, c.e); !errors.Is(err, c.want) {
			t.Errorf("add %s: %v; want %v", name, err, c.want)
		}
	}

	got, err = h.EditNetwork(ctx, "alpha", node.GroupTalk, "eth", node.NetworkEdit{RPCURLs: []string{"https://other.example"}})
	if err != nil || got.ChainID != 1 || !reflect.DeepEqual(got.RPCURLs, []string{"https://other.example"}) {
		t.Fatalf("edit eth: %#v, %v", got, err)
	}
	if _, err := h.EditNetwork(ctx, "alpha", node.GroupTalk, "eth", node.NetworkEdit{ChainID: ptr(uint64(1))}); !errors.Is(err, network.ErrUnchanged) {
		t.Fatalf("an edit changing nothing: %v; want network.ErrUnchanged", err)
	}
	if _, err := h.EditNetwork(ctx, "alpha", node.GroupTalk, "ghost", node.NetworkEdit{}); !errors.Is(err, network.ErrUnknown) {
		t.Fatalf("edit a ghost: %v; want network.ErrUnknown", err)
	}

	list, err := h.Networks(ctx, "alpha", node.GroupTalk)
	if err != nil || len(list) != 2 || list[0].Alias != "btc" || list[0].Chain != "bitcoin testnet" || list[1].Alias != "eth" {
		t.Fatalf("listed %#v, %v", list, err)
	}
	if list, err := h.Networks(ctx, "beta", node.GroupTalk); err != nil || len(list) != 0 {
		t.Fatalf("beta's: %#v, %v; want none", list, err)
	}
	if err := h.RemoveNetwork(ctx, "alpha", node.GroupTalk, "btc"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Network(ctx, "alpha", node.GroupTalk, "btc"); !errors.Is(err, network.ErrUnknown) {
		t.Fatalf("read once removed: %v; want network.ErrUnknown", err)
	}
	if err := h.RemoveNetwork(ctx, "alpha", node.GroupTalk, "btc"); !errors.Is(err, network.ErrUnknown) {
		t.Fatalf("remove again: %v; want network.ErrUnknown", err)
	}
	if _, err := h.Networks(ctx, "nope", node.GroupTalk); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("an unknown Group: %v; want ErrUnknownGroup", err)
	}
}

// A Conversation overrides its Group's Network table whole: an edit there
// starts from the Network in force and writes the whole table, and
// removing the override leaves the Group's in force. A Network added there
// is the Conversation's alone.
func TestAConversationOverridesItsGroupsNetwork(t *testing.T) {
	alice, _, k := keyed(t, "", "")
	ctx := context.Background()
	h := alice.h
	conv, err := h.ResolveConversation(ctx, "alpha", k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddNetwork(ctx, "alpha", node.GroupTalk, "eth", node.NetworkEdit{
		Type: ptr("ethereum"), ChainID: ptr(uint64(1)), RPCURLs: []string{"https://rpc.example"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := h.EditNetwork(ctx, "alpha", conv, "eth", node.NetworkEdit{RPCURLs: []string{"https://private.example"}})
	if err != nil || got.ChainID != 1 || !reflect.DeepEqual(got.RPCURLs, []string{"https://private.example"}) {
		t.Fatalf("override: %#v, %v", got, err)
	}
	if g, err := h.Network(ctx, "alpha", node.GroupTalk, "eth"); err != nil || g.RPCURLs[0] != "https://rpc.example" {
		t.Fatalf("the Group's after an override: %#v, %v", g, err)
	}
	if _, err := h.AddNetwork(ctx, "alpha", conv, "sol", node.NetworkEdit{Type: ptr("solana"), Cluster: ptr("devnet")}); err != nil {
		t.Fatal(err)
	}
	if list, err := h.Networks(ctx, "alpha", conv); err != nil || len(list) != 2 {
		t.Fatalf("the Conversation's: %#v, %v", list, err)
	}
	if list, err := h.Networks(ctx, "alpha", node.GroupTalk); err != nil || len(list) != 1 {
		t.Fatalf("the Group's: %#v, %v", list, err)
	}
	if _, err := h.AddNetwork(ctx, "alpha", conv, "eth", node.NetworkEdit{Type: ptr("ethereum"), ChainID: ptr(uint64(5))}); !errors.Is(err, network.ErrExists) {
		t.Fatalf("add in the Conversation what the Group has: %v; want network.ErrExists", err)
	}
	if _, err := h.AddNetwork(ctx, "alpha", conv, "mainnet", node.NetworkEdit{Type: ptr("ethereum"), ChainID: ptr(uint64(1))}); !errors.Is(err, network.ErrInvalid) {
		t.Fatalf("a second alias for the Group's chain: %v; want network.ErrInvalid", err)
	}

	if err := h.RemoveNetwork(ctx, "alpha", conv, "eth"); err != nil {
		t.Fatal(err)
	}
	if got, err := h.Network(ctx, "alpha", conv, "eth"); err != nil || got.RPCURLs[0] != "https://rpc.example" {
		t.Fatalf("once the override is removed: %#v, %v", got, err)
	}
	if err := h.RemoveNetwork(ctx, "alpha", conv, "eth"); !errors.Is(err, network.ErrUnknown) {
		t.Fatalf("removing an override not there: %v; want network.ErrUnknown", err)
	}
}

// A change to the Networks of a config.toml that does not load as it is
// is a ConfigError, told apart from a change refused, and writes nothing.
func TestANetworkChangeToABrokenConfigIsAConfigError(t *testing.T) {
	p := nodeDir(t)
	h, err := node.Open(p, node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := p.GroupConfigFile("alpha")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	broken := string(raw) + "\n[networks.doge]\ntype = \"dogecoin\"\n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfgErr *node.ConfigError
	_, err = h.AddNetwork(context.Background(), "alpha", node.GroupTalk, "eth", node.NetworkEdit{Type: ptr("ethereum"), ChainID: ptr(uint64(1))})
	if !errors.As(err, &cfgErr) {
		t.Fatalf("add to a broken config: %v; want a ConfigError", err)
	}
	if after, _ := os.ReadFile(path); string(after) != broken {
		t.Fatalf("config.toml rewritten:\n%s", after)
	}
}

// A config.toml that is not TOML at all is as much a ConfigError, for a
// list as for a change.
func TestNetworksOfAConfigThatDoesNotParseAreAConfigError(t *testing.T) {
	p := nodeDir(t)
	h, err := node.Open(p, node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.GroupConfigFile("alpha"), []byte("not = [toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var cfgErr *node.ConfigError
	if _, err := h.Networks(ctx, "alpha", node.GroupTalk); !errors.As(err, &cfgErr) {
		t.Fatalf("list: %v; want a ConfigError", err)
	}
	if _, err := h.AddNetwork(ctx, "alpha", node.GroupTalk, "eth", node.NetworkEdit{Type: ptr("ethereum"), ChainID: ptr(uint64(1))}); !errors.As(err, &cfgErr) {
		t.Fatalf("add: %v; want a ConfigError", err)
	}
	if _, err := h.Networks(ctx, "nope", node.GroupTalk); errors.As(err, &cfgErr) || !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("an unknown Group: %v; want ErrUnknownGroup, no ConfigError", err)
	}
}
