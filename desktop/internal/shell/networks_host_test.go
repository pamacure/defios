package shell_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"defios.com/desktop/internal/shell"
)

func networksOK(t *testing.T, what string, v shell.NetworksView) shell.NetworksView {
	t.Helper()
	if v.Problem != nil || !v.Saved {
		t.Fatalf("%s: %+v (where %q, field %q)", what, v.Problem, v.Where, v.Field)
	}
	return v
}

func networkNamed(v shell.NetworksView, alias string) *shell.NetworkView {
	for i := range v.Networks {
		if v.Networks[i].Alias == alias {
			return &v.Networks[i]
		}
	}
	return nil
}

// Networks added, edited and removed in the app are what the CLI lists;
// an RPC URL may be a Secret's.
func TestNetworksMadeInTheAppAreWhatTheCLILists(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _, ap, _ := formedAt(t)
	v := alice.Networks()
	if v.Group != "home" || !v.Served || v.ConfigProblem != nil || len(v.Networks) != 0 ||
		!slices.Equal(v.Types, []string{"ethereum", "bitcoin", "solana"}) || !slices.Contains(v.SolanaClusters, "devnet") ||
		!slices.Contains(v.BitcoinNetworks, "signet") {
		t.Fatalf("an empty page: %+v", v)
	}
	networksOK(t, "add eth", alice.SaveNetwork(shell.NetworkDraft{New: true, Alias: "eth", Type: "ethereum", ChainID: " 1 ",
		RPCURLs: []shell.RPCURL{{Secret: "ALCHEMY_URL"}, {URL: "https://rpc.example"}, {}}}))
	v = networksOK(t, "add btc", alice.SaveNetwork(shell.NetworkDraft{New: true, Alias: "btc", Type: "bitcoin", BitcoinNetwork: "signet"}))
	eth := networkNamed(v, "eth")
	if eth == nil || eth.Chain != "ethereum chain 1" || eth.ChainID != "1" || eth.Curve != "ecdsa" ||
		!slices.Equal(eth.RPCURLs, []shell.RPCURL{{Secret: "ALCHEMY_URL"}, {URL: "https://rpc.example"}}) {
		t.Fatalf("eth: %+v", eth)
	}

	// An edit may change the type: the old type's fields go.
	networksOK(t, "edit btc into solana", alice.SaveNetwork(shell.NetworkDraft{Alias: "btc", Type: "solana", Cluster: "devnet"}))
	// An edit that changes nothing is saved.
	networksOK(t, "edit, unchanged", alice.SaveNetwork(shell.NetworkDraft{Alias: "eth", Type: "ethereum", ChainID: "1",
		RPCURLs: []shell.RPCURL{{Secret: "ALCHEMY_URL"}, {URL: "https://rpc.example"}}}))

	var listed []map[string]any
	if err := json.Unmarshal([]byte(defiosCLI(t, ap.Config, "network", "list", "--json", "--group", "home")), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0]["alias"] != "btc" || listed[0]["chain"] != "solana devnet" ||
		listed[1]["alias"] != "eth" || !slices.Equal(listed[1]["rpc_urls"].([]any), []any{"$ALCHEMY_URL", "https://rpc.example"}) {
		t.Fatalf("network list: %v", listed)
	}

	v = networksOK(t, "remove", alice.RemoveNetwork("btc"))
	if len(v.Networks) != 1 || v.Where != "network:btc" {
		t.Fatalf("after remove: %+v", v)
	}
}

// A refused change names its field where it can and writes nothing; a
// config.toml that does not load is a config error, apart from a refusal;
// a Group not served lists its Networks.
func TestNetworkRefusalsAndAConfigThatDoesNotLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _, ap, _ := formedAt(t)
	networksOK(t, "add", alice.SaveNetwork(shell.NetworkDraft{New: true, Alias: "eth", Type: "ethereum", ChainID: "1"}))
	for name, c := range map[string]struct {
		d           shell.NetworkDraft
		code, field string
	}{
		"taken":        {shell.NetworkDraft{New: true, Alias: "eth", Type: "ethereum", ChainID: "10"}, "network-exists", "alias"},
		"bad alias":    {shell.NetworkDraft{New: true, Alias: "Main Net", Type: "ethereum", ChainID: "10"}, "invalid", "alias"},
		"no chain id":  {shell.NetworkDraft{New: true, Alias: "op", Type: "ethereum", ChainID: "ten"}, "invalid", "chain_id"},
		"no type":      {shell.NetworkDraft{New: true, Alias: "op"}, "invalid", "type"},
		"same chain":   {shell.NetworkDraft{New: true, Alias: "mainnet", Type: "ethereum", ChainID: "1"}, "invalid-network", ""},
		"a credential": {shell.NetworkDraft{New: true, Alias: "op", Type: "ethereum", ChainID: "10", RPCURLs: []shell.RPCURL{{URL: "https://u:p@rpc.example"}}}, "invalid-network", ""},
	} {
		v := alice.SaveNetwork(c.d)
		if v.Problem == nil || v.Problem.Code != c.code || v.Field != c.field || v.Saved || v.Where != "new-network" {
			t.Errorf("%s: %+v field %q where %q", name, v.Problem, v.Field, v.Where)
		}
	}
	if v := alice.Networks(); len(v.Networks) != 1 {
		t.Fatalf("a refusal wrote: %+v", v.Networks)
	}

	path := ap.GroupConfigFile("home")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	broken := string(raw) + "\n[networks.doge]\ntype = \"dogecoin\"\n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := alice.Networks(); v.ConfigProblem == nil || v.ConfigProblem.Code != "config-error" || v.Problem != nil {
		t.Fatalf("list a broken config: %+v", v)
	}
	v := alice.SaveNetwork(shell.NetworkDraft{New: true, Alias: "op", Type: "ethereum", ChainID: "10"})
	if v.Problem == nil || v.Problem.Code != "config-error" || v.Saved {
		t.Fatalf("add to a broken config: %+v", v.Problem)
	}
	if after, _ := os.ReadFile(path); string(after) != broken {
		t.Fatalf("config.toml rewritten:\n%s", after)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// A Group not served.
	if st := alice.AddGroup("work", "https://hs.example", "tskey-auth-work", ""); st.Problem != nil {
		t.Fatalf("add group: %+v", st.Problem)
	}
	if st := alice.Switch("home"); st.Served != "home" {
		t.Fatalf("back to home: %+v", st)
	}
	alice.SetEditGroup("work")
	v = networksOK(t, "add to work", alice.SaveNetwork(shell.NetworkDraft{New: true, Alias: "sol", Type: "solana", Cluster: "devnet"}))
	if v.Group != "work" || v.Served || len(v.Networks) != 1 {
		t.Fatalf("work's: %+v", v)
	}
}
