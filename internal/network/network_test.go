package network

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

func eth(alias string, chain uint64, urls ...string) *coslv1.NetworkConfig {
	return &coslv1.NetworkConfig{Alias: alias, Type: "ethereum", ChainId: chain, RpcUrls: urls}
}

// rowOf wraps a payload as the COSL row the FSM would have stored.
func rowOf(t *testing.T, seq uint64, typ uint32, msg proto.Message) cosl.Row {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Marshal(&coslv1.Envelope{Version: 1, MsgType: typ, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return cosl.Row{Seq: seq, MsgType: typ, Envelope: env}
}

func TestValidate(t *testing.T) {
	good := []*coslv1.NetworkConfig{
		eth("ethereum-mainnet", 1, "https://rpc.example", "wss://rpc2.example"),
		eth("eth-no-rpc", 11155111),
		{Alias: "btc", Type: "bitcoin", Network: "signet"},
		{Alias: "sol", Type: "solana", Cluster: "devnet"},
		{Alias: "sol2", Type: "solana", Cluster: "mainnet-beta", GenesisHash: "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"},
	}
	for _, c := range good {
		if err := Validate(c); err != nil {
			t.Errorf("%s: %v", c.Alias, err)
		}
	}
	bad := map[string]*coslv1.NetworkConfig{
		"bad alias":        eth("Bad Alias", 1),
		"empty alias":      eth("", 1),
		"unknown type":     {Alias: "x", Type: "dogecoin"},
		"eth no chain":     {Alias: "x", Type: "ethereum"},
		"eth with cluster": {Alias: "x", Type: "ethereum", ChainId: 1, Cluster: "devnet"},
		"btc bad network":  {Alias: "x", Type: "bitcoin", Network: "mainnet2"},
		"btc with chain":   {Alias: "x", Type: "bitcoin", Network: "mainnet", ChainId: 1},
		"sol bad cluster":  {Alias: "x", Type: "solana", Cluster: "moon"},
		"sol bad genesis":  {Alias: "x", Type: "solana", Cluster: "devnet", GenesisHash: "not base58!"},
		"bad rpc scheme":   eth("x", 1, "ftp://rpc.example"),
		"rpc no host":      eth("x", 1, "https://"),
	}
	for name, c := range bad {
		if err := Validate(c); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestCurveByType(t *testing.T) {
	for typ, want := range map[string]string{"bitcoin": "ecdsa", "ethereum": "ecdsa", "solana": "eddsa"} {
		if got := (Network{Type: typ}).Curve(); got != want {
			t.Errorf("%s curve = %q, want %q", typ, got, want)
		}
	}
}

func TestFoldSupersedesAndTombstones(t *testing.T) {
	rows := []cosl.Row{
		rowOf(t, 1, cosl.TypeNetworkCreate, eth("b", 10)),
		rowOf(t, 2, cosl.TypeNetworkCreate, eth("a", 1, "https://old.example")),
		rowOf(t, 3, cosl.TypeNetworkUpdate, eth("a", 1, "https://new.example")),
		rowOf(t, 4, cosl.TypeNetworkRemove, &coslv1.NetworkRemove{Alias: "b"}),
		rowOf(t, 5, cosl.TypeAppFreeform, &coslv1.AppFreeform{Text: "noise"}),
	}
	got, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Alias != "a" || !reflect.DeepEqual(got[0].RPCURLs, []string{"https://new.example"}) {
		t.Fatalf("fold = %+v", got)
	}
}

func TestFoldIgnoresRacesAndInvalidMessages(t *testing.T) {
	rows := []cosl.Row{
		rowOf(t, 1, cosl.TypeNetworkCreate, eth("a", 1)),
		rowOf(t, 2, cosl.TypeNetworkCreate, eth("a", 99)),                                                           // lost the create race
		rowOf(t, 3, cosl.TypeNetworkUpdate, eth("ghost", 5)),                                                        // nothing to supersede
		rowOf(t, 4, cosl.TypeNetworkUpdate, &coslv1.NetworkConfig{Alias: "a", Type: "bitcoin", Network: "mainnet"}), // type change
		rowOf(t, 5, cosl.TypeNetworkUpdate, eth("a", 0)),                                                            // invalid
		rowOf(t, 6, cosl.TypeNetworkRemove, &coslv1.NetworkRemove{Alias: "ghost"}),
	}
	got, err := Fold(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ChainID != 1 {
		t.Fatalf("fold = %+v", got)
	}
}

func TestRemovedAliasCanBeCreatedAgain(t *testing.T) {
	got, err := Fold([]cosl.Row{
		rowOf(t, 1, cosl.TypeNetworkCreate, eth("a", 1)),
		rowOf(t, 2, cosl.TypeNetworkRemove, &coslv1.NetworkRemove{Alias: "a"}),
		rowOf(t, 3, cosl.TypeNetworkCreate, eth("a", 2)),
	})
	if err != nil || len(got) != 1 || got[0].ChainID != 2 {
		t.Fatalf("fold = %+v, %v", got, err)
	}
}

func TestOverrideBeatsLogRPC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "networks.toml")
	if err := os.WriteFile(path, []byte("[ethereum-mainnet]\nrpc_urls = [\"https://key.example/SECRET\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ov, err := LoadOverrides(path)
	if err != nil {
		t.Fatal(err)
	}
	n := Network{Alias: "ethereum-mainnet", RPCURLs: []string{"https://public.example"}}
	if urls, overridden := ov.Effective(n); !overridden || !reflect.DeepEqual(urls, []string{"https://key.example/SECRET"}) {
		t.Fatalf("override: %v %v", urls, overridden)
	}
	other := Network{Alias: "other", RPCURLs: []string{"https://public.example"}}
	if urls, overridden := ov.Effective(other); overridden || !reflect.DeepEqual(urls, other.RPCURLs) {
		t.Fatalf("no override: %v %v", urls, overridden)
	}
}

func TestMissingOverrideFileIsEmpty(t *testing.T) {
	ov, err := LoadOverrides(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, overridden := ov.Effective(Network{Alias: "x"}); overridden {
		t.Fatal("empty overrides overrode")
	}
}

func TestOverrideRejectsBadURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "networks.toml")
	os.WriteFile(path, []byte("[x]\nrpc_urls = [\"gopher://nope\"]\n"), 0o600)
	if _, err := LoadOverrides(path); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}
