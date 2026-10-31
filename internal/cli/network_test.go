package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/group"
	"defios.com/internal/network"
	"defios.com/internal/nodedir"
)

// network add writes a whole [networks.<alias>] table to the Group's
// config.toml, with no serve running, and keeps the rest of the file
// (ADR 0023). list and show read it back.
func TestNetworkAddWritesTheLocalTable(t *testing.T) {
	dir := secretNode(t)
	before := groupConfig(t, dir)

	r := run(t, nil, "", "--dir", dir, "network", "add", "eth", "--type", "ethereum", "--chain-id", "1",
		"--rpc-url", "https://rpc.example", "--rpc-url", "$ALCHEMY")
	if r.code != 0 {
		t.Fatalf("add exit %d: %s", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "network", "add", "btc", "--type", "bitcoin", "--bitcoin-network", "testnet")
	if r.code != 0 {
		t.Fatalf("add btc exit %d: %s", r.code, r.stderr)
	}
	c := groupConfig(t, dir)
	want := map[string]network.Network{
		"eth": {Type: "ethereum", ChainID: 1, RPCURLs: []string{"https://rpc.example", "$ALCHEMY"}},
		"btc": {Type: "bitcoin", BitcoinNetwork: "testnet"},
	}
	if !reflect.DeepEqual(c.Networks, want) {
		t.Fatalf("networks = %+v, want %+v", c.Networks, want)
	}
	if c.Hostname != before.Hostname || c.HeadscaleURL != before.HeadscaleURL || c.TailnetAuthKey != before.TailnetAuthKey {
		t.Fatalf("add lost the rest of config.toml: %+v, was %+v", c, before)
	}

	r = run(t, nil, "", "--dir", dir, "network", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "eth  ethereum  ethereum chain 1") || !strings.Contains(r.stdout, "btc  bitcoin  bitcoin testnet") {
		t.Fatalf("list exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "--json", "network", "show", "eth")
	var v struct {
		Alias   string   `json:"alias"`
		Chain   string   `json:"chain"`
		ChainID uint64   `json:"chain_id"`
		Curve   string   `json:"curve"`
		RPCURLs []string `json:"rpc_urls"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || r.code != 0 {
		t.Fatalf("show exit %d: %v\n%s%s", r.code, err, r.stdout, r.stderr)
	}
	if v.Alias != "eth" || v.Chain != "ethereum chain 1" || v.ChainID != 1 || v.Curve != "ecdsa" || len(v.RPCURLs) != 2 || v.RPCURLs[1] != "$ALCHEMY" {
		t.Fatalf("show = %+v", v)
	}
}

// What config.toml would refuse, add refuses: a plain URL that carries a
// credential (without echoing it), a second alias for one chain, an alias
// that exists, a bad identity.
func TestNetworkAddRefusesWhatTheConfigWouldNot(t *testing.T) {
	dir := secretNode(t)
	if r := run(t, nil, "", "--dir", dir, "network", "add", "eth", "--type", "ethereum", "--chain-id", "1"); r.code != 0 {
		t.Fatalf("add exit %d: %s", r.code, r.stderr)
	}
	for name, args := range map[string][]string{
		"userinfo":     {"op", "--type", "ethereum", "--chain-id", "10", "--rpc-url", "https://u:hunter2@rpc.example"},
		"query":        {"op", "--type", "ethereum", "--chain-id", "10", "--rpc-url", "https://rpc.example/?key=hunter2"},
		"same chain":   {"mainnet", "--type", "ethereum", "--chain-id", "1"},
		"alias exists": {"eth", "--type", "ethereum", "--chain-id", "10"},
		"no chain id":  {"op", "--type", "ethereum"},
		"bad type":     {"doge", "--type", "dogecoin"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "network", "add"}, args...)...)
		if r.code != 1 {
			t.Errorf("%s: exit %d, want 1: %s", name, r.code, r.stderr)
		}
		if strings.Contains(r.stdout+r.stderr, "hunter2") {
			t.Errorf("%s: echoed the credential: %s", name, r.stderr)
		}
	}
	if c := groupConfig(t, dir); len(c.Networks) != 1 {
		t.Fatalf("a refused add wrote config.toml: %+v", c.Networks)
	}
}

// edit changes only the fields it is given; an edit that changes nothing, or
// of an alias that does not exist, is refused. remove deletes the table.
func TestNetworkEditAndRemove(t *testing.T) {
	dir := secretNode(t)
	if r := run(t, nil, "", "--dir", dir, "network", "add", "eth", "--type", "ethereum", "--chain-id", "1", "--rpc-url", "https://a.example"); r.code != 0 {
		t.Fatalf("add exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "network", "edit", "eth", "--rpc-url", "$RPC"); r.code != 0 {
		t.Fatalf("edit exit %d: %s", r.code, r.stderr)
	}
	if n := groupConfig(t, dir).Networks["eth"]; n.ChainID != 1 || len(n.RPCURLs) != 1 || n.RPCURLs[0] != "$RPC" {
		t.Fatalf("after edit = %+v", n)
	}
	if r := run(t, nil, "", "--dir", dir, "network", "edit", "eth", "--chain-id", "1"); r.code != 1 || !strings.Contains(r.stderr, "nothing to change") {
		t.Fatalf("unchanged edit exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "network", "edit", "nope", "--chain-id", "2"); r.code != 1 {
		t.Fatalf("edit of an unknown alias exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "network", "remove", "eth"); r.code != 0 {
		t.Fatalf("remove exit %d: %s", r.code, r.stderr)
	}
	if c := groupConfig(t, dir); len(c.Networks) != 0 {
		t.Fatalf("after remove = %+v", c.Networks)
	}
	if r := run(t, nil, "", "--dir", dir, "network", "remove", "eth"); r.code != 1 {
		t.Fatalf("remove of an unknown alias exit %d: %s", r.code, r.stderr)
	}
}

// --group points add, edit and remove at a Group other than the Current
// one, as it does provider's.
func TestNetworkWriteVerbsTakeGroup(t *testing.T) {
	dir := secretNode(t)
	if r := run(t, nil, "", "--dir", dir, "group", "new", "other", "--headscale", "https://hs.example"); r.code != 0 {
		t.Fatalf("group new other: %s", r.stderr)
	}
	for _, args := range [][]string{
		{"add", "eth", "--type", "ethereum", "--chain-id", "1"},
		{"add", "op", "--type", "ethereum", "--chain-id", "10"},
		{"edit", "eth", "--rpc-url", "https://a.example"},
		{"remove", "op"},
	} {
		if r := run(t, nil, "", append([]string{"--dir", dir, "network"}, append(args, "--group", group.ImplicitAlias)...)...); r.code != 0 {
			t.Fatalf("%v: exit %d: %s", args, r.code, r.stderr)
		}
	}
	want := map[string]network.Network{"eth": {Type: "ethereum", ChainID: 1, RPCURLs: []string{"https://a.example"}}}
	if got := groupConfig(t, dir).Networks; !reflect.DeepEqual(got, want) {
		t.Fatalf("%s networks = %+v, want %+v", group.ImplicitAlias, got, want)
	}
	if r := run(t, nil, "", "--dir", dir, "network", "list"); r.code != 0 || r.stdout != "" {
		t.Fatalf("the Current Group's list: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

// network list and show read the Group's config, or with --key that Key's
// Conversation config.toml merged over it, a table replacing the Group's
// whole.
func TestNetworkListAndShowReadTheMergedConfig(t *testing.T) {
	dir := t.TempDir()
	groupWithKeys(t, dir)
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})
	if err := os.MkdirAll(p.GroupConfigDir(group.ImplicitAlias), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.GroupConfigFile(group.ImplicitAlias), []byte(`
[networks.eth]
type = "ethereum"
chain_id = 1
rpc_urls = ["https://a.example"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	conv := p.ConversationConfigFile(group.ImplicitAlias, "ecdsa-ab1111111111")
	if err := os.MkdirAll(filepath.Dir(conv), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conv, []byte(`
[networks.eth]
type = "ethereum"
chain_id = 1
rpc_urls = ["$RPC"]

[networks.btc]
type = "bitcoin"
network = "testnet"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if r := run(t, nil, "", "--dir", dir, "network", "list"); r.code != 0 || r.stdout != "eth  ethereum  ethereum chain 1\n" {
		t.Fatalf("list: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	want := "btc  bitcoin  bitcoin testnet\neth  ethereum  ethereum chain 1\n"
	if r := run(t, nil, "", "--dir", dir, "network", "list", "--key", "ab11"); r.code != 0 || r.stdout != want {
		t.Fatalf("list --key: exit %d, stdout %q, stderr %q\nwant %q", r.code, r.stdout, r.stderr, want)
	}
	r := run(t, nil, "", "--dir", dir, "network", "show", "eth", "--key", "ab11")
	if r.code != 0 || !strings.Contains(r.stdout, "rpc_url $RPC\n") || strings.Contains(r.stdout, "a.example") {
		t.Fatalf("show --key: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "network", "show", "btc"); r.code != 1 {
		t.Fatalf("show btc without --key: exit %d, stdout %q", r.code, r.stdout)
	}
}

// A Group with no config.toml has no Networks: list shows none, and add
// writes the file, as provider add does.
func TestNetworkAddWithNoConfigFile(t *testing.T) {
	dir := t.TempDir()
	groupWithKeys(t, dir)
	if r := run(t, nil, "", "--dir", dir, "network", "list"); r.code != 0 || r.stdout != "" {
		t.Fatalf("list exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "network", "add", "eth", "--type", "ethereum", "--chain-id", "1"); r.code != 0 {
		t.Fatalf("add exit %d: %s", r.code, r.stderr)
	}
	if n := groupConfig(t, dir).Networks["eth"]; n.ChainID != 1 {
		t.Fatalf("after add = %+v", n)
	}
}
