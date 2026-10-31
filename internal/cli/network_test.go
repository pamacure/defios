package cli_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/network"
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
