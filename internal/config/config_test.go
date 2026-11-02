package config_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/network"
	"defios.com/internal/nodedir"
	"defios.com/internal/provider"
)

func TestWriteUsesSpecKeysAndReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := config.Config{HeadscaleURL: "https://headscale.example", DisplayName: "laptop", Hostname: "defios-a1b2c3d4", TailnetAuthKey: "$TS_AUTHKEY"}
	if err := config.WriteFile(path, in); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`headscale_url = "https://headscale.example"`,
		`display_name = "laptop"`,
		`hostname = "defios-a1b2c3d4"`,
		`tailnet_auth_key = "$TS_AUTHKEY"`,
	} {
		if !strings.Contains(string(raw), line) {
			t.Errorf("config.toml missing %q:\n%s", line, raw)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 0600", info.Mode().Perm())
	}
	out, err := config.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("read %+v, want %+v", out, in)
	}
}

// config.toml is now per-Group (ADR 0007): it round-trips at the
// alias-parameterized path nodedir resolves, independent of any other Group's
// config.
func TestPerGroupConfigRoundTrips(t *testing.T) {
	p := nodedir.Resolve(nodedir.Lookup{Dir: t.TempDir()})
	for _, tc := range []struct {
		alias string
		cfg   config.Config
	}{
		{"home", config.Config{HeadscaleURL: "https://home.example", DisplayName: "laptop", Hostname: "defios-home"}},
		{"work", config.Config{HeadscaleURL: "https://work.example", DisplayName: "laptop", Hostname: "defios-work"}},
	} {
		if err := os.MkdirAll(p.GroupConfigDir(tc.alias), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := config.WriteFile(p.GroupConfigFile(tc.alias), tc.cfg); err != nil {
			t.Fatal(err)
		}
	}
	got, err := config.ReadFile(p.GroupConfigFile("home"))
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadscaleURL != "https://home.example" || got.Hostname != "defios-home" {
		t.Fatalf("home config = %+v; a Group's config.toml must be keyed by its alias", got)
	}
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const groupConfig = `
headscale_url = "https://hs.example"
display_name = "laptop"
hostname = "defios-home"
tailnet_auth_key = "$TS_AUTHKEY"

[networks.eth]
type = "ethereum"
chain_id = 1
rpc_urls = ["$ALCHEMY_ETH", "https://eth.example"]

[networks.btc]
type = "bitcoin"
network = "testnet"
rpc_urls = ["$BTC_RPC"]

[providers.anthropic]
base_url = "https://api.anthropic.com"
api_path = "/v1/messages"
api_key = "$DEFIOS_ANTHROPIC"

[agents.auditor]
provider = "claude"

[agents.trader]
provider = "gpt"

[agent]
profiles = ["auditor"]
max_depth = 2
`

// endpoint is a valid base_url and api_path, for a [providers.<alias>]
// table under test for something else.
const endpoint = "base_url = \"https://x.example\"\napi_path = \"/v1/messages\"\n"

// The Group config.toml holds this Node's tailnet presence, its Agent
// Profiles and the three Workspace sections, every secret as a reference.
func TestGroupConfigReadsTheWorkspaceSectionsAndAgentProfiles(t *testing.T) {
	c, err := config.ReadFile(write(t, groupConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Workspace{
		Networks: map[string]network.Network{
			"eth": {Type: "ethereum", ChainID: 1, RPCURLs: []string{"$ALCHEMY_ETH", "https://eth.example"}},
			"btc": {Type: "bitcoin", BitcoinNetwork: "testnet", RPCURLs: []string{"$BTC_RPC"}},
		},
		Providers: map[string]config.Provider{"anthropic": {BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages", APIKey: "$DEFIOS_ANTHROPIC"}},
		Agent:     config.Agent{Profiles: []string{"auditor"}, MaxDepth: 2},
	}
	if !reflect.DeepEqual(c.Workspace, want) || c.TailnetAuthKey != "$TS_AUTHKEY" {
		t.Fatalf("read %+v", c)
	}
	wantAgents := map[string]config.Profile{"auditor": {Provider: "claude"}, "trader": {Provider: "gpt"}}
	if !reflect.DeepEqual(c.Agents, wantAgents) {
		t.Fatalf("read %+v", c)
	}
}

// A plaintext secret in a config is an error when the file is read, in
// every secret-bearing field; so is a key the file does not define.
func TestConfigRefusesLiteralSecretsAndUnknownKeys(t *testing.T) {
	for name, body := range map[string]string{
		"literal tailnet_auth_key": `tailnet_auth_key = "tskey-auth-abc"`,
		"rpc url with userinfo":    "[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\nrpc_urls = [\"https://u:abc@eth.example\"]",
		"rpc url with a query":     "[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\nrpc_urls = [\"https://eth.example/?key=abc\"]",
		"literal api_key":          "[providers.anthropic]\n" + endpoint + "api_key = \"sk-ant-abc\"",
		"interpolated reference":   "[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\nrpc_urls = [\"$KEY/v2\"]",
		"lowercase reference":      "[providers.openai]\n" + endpoint + "api_key = \"$openai\"",
		"old auth_key":             `auth_key = "tskey-auth-abc"`,
		"bad provider alias":       "[providers.Mistral]\n" + endpoint + "api_key = \"$KEY\"",
	} {
		if _, err := config.ReadFile(write(t, body)); err == nil {
			t.Errorf("%s: read without error", name)
		} else if strings.Contains(err.Error(), "abc") {
			t.Errorf("%s: error echoes the secret: %v", name, err)
		}
	}
}

// A [networks.<alias>] table is the whole Network. An rpc_urls entry is a
// plain http(s) URL that carries no credential, or a Secret reference; a
// plain URL with userinfo or a query, a bad identity, or two aliases for
// one chain are refused when the file is read (ADR 0023).
func TestNetworkTablesAreWholeNetworks(t *testing.T) {
	const eth1 = "[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\n"
	for _, urls := range []string{`["https://rpc.example"]`, `["$RPC"]`, `[]`, `["http://127.0.0.1:8545", "$RPC"]`} {
		if _, err := config.ReadFile(write(t, eth1+"rpc_urls = "+urls)); err != nil {
			t.Errorf("rpc_urls = %s: %v", urls, err)
		}
	}
	for name, body := range map[string]string{
		"userinfo":         eth1 + `rpc_urls = ["https://u:p@rpc.example"]`,
		"query":            eth1 + `rpc_urls = ["https://rpc.example/?key=x"]`,
		"websocket":        eth1 + `rpc_urls = ["wss://rpc.example"]`,
		"no type":          "[networks.eth]\nchain_id = 1",
		"no chain id":      "[networks.eth]\ntype = \"ethereum\"",
		"bitcoin chain_id": "[networks.btc]\ntype = \"bitcoin\"\nnetwork = \"mainnet\"\nchain_id = 1",
		"unknown key":      eth1 + `endpoint = "https://rpc.example"`,
		"two aliases":      eth1 + "[networks.mainnet]\ntype = \"ethereum\"\nchain_id = 1",
		"bad solana":       "[networks.sol]\ntype = \"solana\"\ncluster = \"moon\"",
	} {
		if _, err := config.ReadFile(write(t, body)); err == nil {
			t.Errorf("%s: read without error", name)
		} else if strings.Contains(err.Error(), "u:p") || strings.Contains(err.Error(), "key=x") {
			t.Errorf("%s: error echoes the credential: %v", name, err)
		}
	}
	if _, err := config.ReadWorkspace(write(t, eth1+`rpc_urls = ["https://rpc.example/?key=x"]`)); err == nil {
		t.Error("a Conversation's config.toml must refuse a plain URL with a query too")
	}
}

// WriteFile writes a Network as its table, and it reads back the same.
func TestNetworkTablesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := config.Config{Hostname: "h", Workspace: config.Workspace{Networks: map[string]network.Network{
		"eth": {Type: "ethereum", ChainID: 1, RPCURLs: []string{"$RPC"}},
		"sol": {Type: "solana", Cluster: "devnet", GenesisHash: "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"},
	}}}
	if err := config.WriteFile(path, in); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "chain_id = 0") || strings.Contains(string(raw), `network = ""`) {
		t.Errorf("a Network writes another type's fields:\n%s", raw)
	}
	out, err := config.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("read %+v, want %+v", out, in)
	}
}

// A Conversation's config.toml holds only the Workspace sections; a missing
// one is an empty Workspace.
func TestConversationConfigTakesOnlyTheWorkspaceSections(t *testing.T) {
	if _, err := config.ReadWorkspace(write(t, "hostname = \"x\"\n[agent]\nprofiles = [\"auditor\"]")); err == nil {
		t.Fatal("a top-level key in a Conversation config must be an error")
	}
	if _, err := config.ReadWorkspace(write(t, "[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\nrpc_urls = [\"https://k@literal.example\"]")); err == nil {
		t.Fatal("an rpc url with userinfo in a Conversation config must be an error")
	}
	w, err := config.ReadWorkspace(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil || !reflect.DeepEqual(w, config.Workspace{}) {
		t.Fatalf("missing file = %+v, %v", w, err)
	}
}

// A Conversation's Workspace merges over the Group's table by table: what it
// sets wins, and what it leaves alone the Group's supplies.
func TestConversationMergesOverTheGroupTableByTable(t *testing.T) {
	group, err := config.ReadFile(write(t, groupConfig))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := config.ReadWorkspace(write(t, `
[networks.eth]
type = "ethereum"
chain_id = 1
rpc_urls = ["$CONV_ETH"]

[providers.anthropic]

[providers.openai]
api_key = "$DEFIOS_OPENAI"

[agent]
profiles = ["trader"]
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := group.Workspace.Merge(conv)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Workspace{
		Networks: map[string]network.Network{
			"eth": {Type: "ethereum", ChainID: 1, RPCURLs: []string{"$CONV_ETH"}},
			"btc": {Type: "bitcoin", BitcoinNetwork: "testnet", RPCURLs: []string{"$BTC_RPC"}},
		},
		Providers: map[string]config.Provider{
			"anthropic": {BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages", APIKey: "$DEFIOS_ANTHROPIC"},
			"openai":    {APIKey: "$DEFIOS_OPENAI"},
		},
		Agent: config.Agent{Profiles: []string{"trader"}, MaxDepth: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %+v\nwant     %+v", got, want)
	}
	if len(group.Workspace.Networks["eth"].RPCURLs) != 2 {
		t.Fatal("Merge changed the Group's Workspace")
	}
}

// Two aliases for one chain are refused in the merged Workspace too, though
// each file alone passes: one in the Group's config, one in a
// Conversation's (ADR 0023).
func TestAMergeRefusesTwoAliasesForOneChain(t *testing.T) {
	group, err := config.ReadFile(write(t, groupConfig))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := config.ReadWorkspace(write(t, "[networks.mainnet]\ntype = \"ethereum\"\nchain_id = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := group.Workspace.Merge(conv); !errors.Is(err, network.ErrInvalid) || !strings.Contains(err.Error(), "eth and mainnet both name ethereum chain 1") {
		t.Fatalf("merge = %v; want the two aliases refused", err)
	}
}

// A Conversation's [providers.<alias>] overrides the Group's key by key: one
// that sets only base_url keeps the Group's api_path, api_key and body. Its
// own keys are checked when it is read; whether the merged Provider is
// whole is checked where it is used.
func TestAConversationOverridesAProviderKeyByKey(t *testing.T) {
	group, err := config.ReadFile(write(t, `
[providers.x]
base_url = "https://api.anthropic.com"
api_path = "/v1/messages"
api_key = "$X_KEY"

[providers.x.body]
model = "m"
stream = true
`))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := config.ReadWorkspace(write(t, "[providers.x]\nbase_url = \"http://127.0.0.1:8080\"\n\n[providers.y]\napi_path = \"/v1/responses\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := group.Workspace.Merge(conv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ws.Provider("x")
	want := config.Provider{BaseURL: "http://127.0.0.1:8080", APIPath: "/v1/messages", APIKey: "$X_KEY", Body: map[string]any{"model": "m", "stream": true}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("x = %+v, %v\nwant %+v", got, err, want)
	}
	if got.Endpoint("x").URL() != "http://127.0.0.1:8080/v1/messages" {
		t.Fatalf("x is called at %s", got.Endpoint("x").URL())
	}
	if group.Providers["x"].BaseURL != "https://api.anthropic.com" {
		t.Fatal("Merge changed the Group's Provider")
	}
	if _, err := ws.Provider("y"); !errors.Is(err, provider.ErrInvalid) || !strings.Contains(err.Error(), "[providers.y]") {
		t.Fatalf("y, a Conversation's table the Group lacks, with no base_url = %v; want it invalid", err)
	}
	if _, err := ws.Provider("z"); !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("z = %v; want it unknown", err)
	}
	for name, body := range map[string]string{
		"base_url userinfo": "[providers.x]\nbase_url = \"https://u:SECRET@x.example\"",
		"unknown shape":     "[providers.x]\napi_path = \"/v1/foo\"",
	} {
		if _, err := config.ReadWorkspace(write(t, body)); err == nil {
			t.Errorf("Conversation %s: read without error", name)
		}
	}
}

// prune is this Node's policy for the Group, an age: whole days or a Go
// duration (ADR 0024). Unset is an archival Node, a boolean or anything
// that is not a positive age is an error, it survives a write, and only the
// Group's config.toml may set it.
func TestPrunePolicyIsAnAge(t *testing.T) {
	c, err := config.ReadFile(write(t, groupConfig))
	if err != nil || c.Prune != 0 {
		t.Fatalf("prune unset = %v, %v; want 0, an archival Node", c.Prune, err)
	}
	for body, want := range map[string]time.Duration{
		`prune = "90d"`: 90 * 24 * time.Hour,
		`prune = "36h"`: 36 * time.Hour,
	} {
		c, err := config.ReadFile(write(t, body+"\n"+groupConfig))
		if err != nil || time.Duration(c.Prune) != want {
			t.Errorf("%s read as %v, %v; want %v", body, time.Duration(c.Prune), err, want)
		}
	}
	for _, body := range []string{`prune = true`, `prune = false`, `prune = "on"`, `prune = 90`, `prune = "0d"`, `prune = "-1h"`, `prune = "1.5d"`, `prune = "d"`} {
		if _, err := config.ReadFile(write(t, body)); err == nil || !strings.Contains(err.Error(), "prune") {
			t.Errorf("%s: err = %v, want an error naming prune", body, err)
		}
	}
	if _, err := config.ReadWorkspace(write(t, `prune = "1d"`)); err == nil {
		t.Fatal("prune in a Conversation config must be an error")
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	in := config.Config{DisplayName: "laptop", Hostname: "defios-home", Prune: config.Age(90 * 24 * time.Hour)}
	if err := config.WriteFile(path, in); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `prune = "90d"`) {
		t.Errorf("written config.toml:\n%s\nwant prune = \"90d\"", b)
	}
	if out, err := config.ReadFile(path); err != nil || out.Prune != in.Prune {
		t.Fatalf("round trip = %v, %v; want %v", out.Prune, err, in.Prune)
	}
	for age, want := range map[time.Duration]string{36 * time.Hour: "36h", 90 * time.Minute: "1h30m", 90 * time.Second: "1m30s"} {
		if got := config.Age(age).String(); got != want {
			t.Errorf("Age(%v) = %q, want %q", age, got, want)
		}
	}
}

// An Agent Profile is defined only in the Group's config.toml, under a Handle
// that a mention can name (ADR 0016) and is not "user", and names the
// Provider it runs on; [agent] includes only Handles the Group defines, at a depth of
// at least 1.
func TestConfigRefusesMalformedAgentProfiles(t *testing.T) {
	for name, body := range map[string]string{
		"bad handle":                 "[agents.Auditor]\nprovider = \"claude\"",
		"reserved handle":            "[agents.user]\nprovider = \"claude\"",
		"handle ending in -":         "[agents.auditor-]\nprovider = \"claude\"",
		"handle ending in _":         "[agents.auditor_]\nprovider = \"claude\"",
		"handle with --":             "[agents.au--di]\nprovider = \"claude\"",
		"missing provider":           "[agents.auditor]\nmax_turns = 4",
		"bad provider alias":         "[agents.auditor]\nprovider = \"Claude!\"",
		"undefined handle":           "[agents.auditor]\nprovider = \"claude\"\n[agent]\nprofiles = [\"trader\"]",
		"handle listed twice":        "[agents.auditor]\nprovider = \"claude\"\n[agent]\nprofiles = [\"auditor\", \"auditor\"]",
		"zero max_depth":             "[agent]\nmax_depth = 0",
		"negative max_depth":         "[agent]\nmax_depth = -1",
		"old [agent] model":          "[agent]\nmodel = \"claude\"",
		"old [agent] window":         "[agent]\ntoken_window = 200000",
		"unknown profile key":        "[agents.auditor]\nprovider = \"claude\"\nsoul = \"SOUL.md\"",
		"old model key":              "[agents.auditor]\nprovider = \"claude\"\nmodel = \"claude-opus-5-5\"",
		"old max_tokens key":         "[agents.auditor]\nprovider = \"claude\"\nmax_tokens = 4000",
		"old temperature key":        "[agents.auditor]\nprovider = \"claude\"\ntemperature = 0.2",
		"zero max_turns":             "[agents.auditor]\nprovider = \"claude\"\nmax_turns = 0",
		"negative max_turns":         "[agents.auditor]\nprovider = \"claude\"\nmax_turns = -3",
		"body not a table":           "[agents.auditor]\nprovider = \"claude\"\nbody = \"x\"",
		"date-time in body":          "[agents.auditor]\nprovider = \"claude\"\n[agents.auditor.body]\nsince = 2026-01-01T00:00:00Z",
		"local date in body":         "[agents.auditor]\nprovider = \"claude\"\n[agents.auditor.body.metadata]\nday = 2026-01-01",
		"time in an array":           "[agents.auditor]\nprovider = \"claude\"\n[agents.auditor.body]\nat = [07:32:00]",
		"nan in body":                "[agents.auditor]\nprovider = \"claude\"\n[agents.auditor.body]\ntemperature = nan",
		"provider body not a table":  "[providers.claude]\n" + endpoint + "body = \"x\"",
		"date-time in provider body": "[providers.claude]\n" + endpoint + "[providers.claude.body]\nsince = 2026-01-01T00:00:00Z",
		"unknown provider key":       "[providers.claude]\n" + endpoint + "model = \"claude-opus-5-5\"",
	} {
		_, err := config.ReadFile(write(t, body))
		if err == nil {
			t.Errorf("%s: read without error", name)
		} else if strings.Contains(name, "date") || strings.Contains(name, "time in") {
			if !strings.Contains(err.Error(), "date or time") {
				t.Errorf("%s: %v, want it to say a date or time has no JSON form", name, err)
			}
		}
	}
	for name, body := range map[string]string{
		"profile in a Conversation":  "[agents.auditor]\nprovider = \"claude\"",
		"body in a Conversation":     "[agents.auditor.body]\nmodel = \"claude-opus-5-5\"",
		"bad handle in profiles":     "[agent]\nprofiles = [\"Auditor\"]",
		"zero max_depth":             "[agent]\nmax_depth = 0",
		"date-time in provider body": "[providers.claude.body.metadata]\nday = 2026-01-01",
	} {
		if _, err := config.ReadWorkspace(write(t, body)); err == nil {
			t.Errorf("Conversation %s: read without error", name)
		}
	}
}

// A profile's body is its request's other fields, as TOML nests them,
// converted to JSON values: tables are objects, an array of tables an array
// of objects, integers and floats numbers. max_turns resolves with it;
// unset, it is 8, and an unset body is empty.
func TestIncludedResolvesTheProfileBody(t *testing.T) {
	c, err := config.ReadFile(write(t, `
[agents.auditor]
provider = "claude"
max_turns = 3

[agents.auditor.body]
model = "claude-opus-5-5"
stream = true
max_tokens = 4096
temperature = 1
stop_sequences = ["END"]

[agents.auditor.body.thinking]
type = "adaptive"

[[agents.auditor.body.metadata.tags]]
name = "a"

[agents.scribe]
provider = "local"

[agent]
profiles = ["auditor", "scribe"]
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Included(config.Workspace{}, "/r")
	if err != nil {
		t.Fatal(err)
	}
	auditor, scribe := got.Profiles[0], got.Profiles[1]
	raw, err := json.Marshal(auditor.Body)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"max_tokens":4096,"metadata":{"tags":[{"name":"a"}]},"model":"claude-opus-5-5","stop_sequences":["END"],"stream":true,"temperature":1,"thinking":{"type":"adaptive"}}`
	if string(raw) != want || auditor.Provider != "claude" || auditor.MaxTurns != 3 {
		t.Fatalf("auditor = %+v\nbody %s\nwant %s", auditor, raw, want)
	}
	if scribe.Provider != "local" || scribe.MaxTurns != 8 || len(scribe.Body) != 0 {
		t.Fatalf("scribe = %+v", scribe)
	}
}

// A Provider's body is the base for every profile that runs on it: the
// profile's body merges over it, objects key by key and anything else
// replacing, and a Conversation's body for that Provider merges over the
// Group's the same way.
func TestAProfileBodyMergesOverItsProvidersBody(t *testing.T) {
	c, err := config.ReadFile(write(t, `
[providers.gpt]
base_url = "https://api.openai.com"
api_path = "/v1/responses"
api_key = "$OPENAI"

[providers.gpt.body]
model = "gpt-base"
stream = true
store = false
include = ["reasoning.encrypted_content"]

[providers.gpt.body.reasoning]
effort = "low"
summary = "auto"

[agents.trader]
provider = "gpt"

[agents.auditor]
provider = "gpt"

[agents.auditor.body]
model = "gpt-big"
include = []

[agents.auditor.body.reasoning]
effort = "high"

[agents.scribe]
provider = "claude"

[agents.scribe.body]
model = "claude-opus-5-5"

[agent]
profiles = ["trader", "auditor", "scribe"]
`))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := config.ReadWorkspace(write(t, `
[providers.gpt.body]
stream = false
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		conv config.Workspace
		want []string
	}{
		{config.Workspace{}, []string{
			`{"include":["reasoning.encrypted_content"],"model":"gpt-base","reasoning":{"effort":"low","summary":"auto"},"store":false,"stream":true}`,
			`{"include":[],"model":"gpt-big","reasoning":{"effort":"high","summary":"auto"},"store":false,"stream":true}`,
			`{"model":"claude-opus-5-5"}`,
		}},
		{conv, []string{
			`{"include":["reasoning.encrypted_content"],"model":"gpt-base","reasoning":{"effort":"low","summary":"auto"},"store":false,"stream":false}`,
			`{"include":[],"model":"gpt-big","reasoning":{"effort":"high","summary":"auto"},"store":false,"stream":false}`,
			`{"model":"claude-opus-5-5"}`,
		}},
	} {
		got, err := c.Included(tc.conv, "/r")
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range got.Profiles {
			raw, err := json.Marshal(p.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.want[i] {
				t.Errorf("%s body %s\nwant        %s", p.Handle, raw, tc.want[i])
			}
		}
	}
	if c.Workspace.Providers["gpt"].Body["stream"] != true || c.Workspace.Providers["gpt"].Body["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("Included changed the Group's Provider body: %v", c.Workspace.Providers["gpt"].Body)
	}
	if c.Agents["auditor"].Body["reasoning"].(map[string]any)["summary"] != nil {
		t.Fatalf("Included changed the profile's body: %v", c.Agents["auditor"].Body)
	}
}

// A profile's directory is named by its Handle (ADR 0026): a Group config
// that still sets [agents.<handle>] dir is refused as an unknown key.
func TestConfigRefusesAProfileDir(t *testing.T) {
	_, err := config.ReadFile(write(t, "[agents.x]\ndir = \"agents/y\"\nprovider = \"claude\""))
	if err == nil || !strings.Contains(err.Error(), "agents.x.dir") {
		t.Fatalf("read = %v; want an unknown-key error naming agents.x.dir", err)
	}
}

// Included is what may answer in one place: the Group's included profiles, or
// the Conversation's list in their place, each with its profile directory
// named by its Handle under the config root (ADR 0026), and the depth a chain
// may reach.
func TestIncludedResolvesTheProfiles(t *testing.T) {
	c, err := config.ReadFile(write(t, `
[agents.auditor]
provider = "claude"

[agents.trader]
provider = "gpt"

[agents.scout]
provider = "claude"

[agents.quant]
provider = "gpt"

[agent]
profiles = ["auditor"]
max_depth = 3
`))
	if err != nil {
		t.Fatal(err)
	}
	const root = "/home/op/.config/defios"

	got, err := c.Included(config.Workspace{}, root)
	want := config.Included{
		Profiles: []config.ResolvedProfile{{Handle: "auditor", Dir: "/home/op/.config/defios/agents/auditor", Provider: "claude", MaxTurns: 8}},
		MaxDepth: 3,
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Group-level = %+v, %v\nwant %+v", got, err, want)
	}

	got, err = c.Included(config.Workspace{Agent: config.Agent{Profiles: []string{"quant", "trader", "scout"}}}, root)
	want = config.Included{
		Profiles: []config.ResolvedProfile{
			{Handle: "quant", Dir: "/home/op/.config/defios/agents/quant", Provider: "gpt", MaxTurns: 8},
			{Handle: "trader", Dir: "/home/op/.config/defios/agents/trader", Provider: "gpt", MaxTurns: 8},
			{Handle: "scout", Dir: "/home/op/.config/defios/agents/scout", Provider: "claude", MaxTurns: 8},
		},
		MaxDepth: 3,
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Conversation = %+v, %v\nwant %+v", got, err, want)
	}

	cleared, err := config.ReadWorkspace(write(t, "[agent]\nprofiles = []\nmax_depth = 2"))
	if err != nil {
		t.Fatal(err)
	}
	got, err = c.Included(cleared, root)
	if err != nil || len(got.Profiles) != 0 || got.MaxDepth != 2 {
		t.Fatalf("Conversation with profiles = [] = %+v, %v; want none, depth 2", got, err)
	}

	if _, err := c.Included(config.Workspace{Agent: config.Agent{Profiles: []string{"ghost"}}}, root); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("a Conversation including an undefined Handle = %v; want an error naming it", err)
	}

	none, err := config.Config{}.Included(config.Workspace{}, root)
	if err != nil || len(none.Profiles) != 0 || none.MaxDepth != 1 {
		t.Fatalf("no agents configured = %+v, %v; want none, depth 1", none, err)
	}
}

// A Provider is wholly this Node's config (ADR 0022): [providers.<alias>]
// holds its base_url and api_path, both required with ADR 0019's rules, and
// a bad one is refused when the config is read.
func TestAProviderIsItsLocalTable(t *testing.T) {
	c, err := config.ReadFile(write(t, `
[providers.claude]
base_url = "https://api.anthropic.com"
api_path = "/v1/messages"
api_key = "$ANTHROPIC"
`))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Provider{BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages", APIKey: "$ANTHROPIC"}
	if got := c.Providers["claude"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("read %+v, want %+v", got, want)
	}
	for name, body := range map[string]string{
		"missing api_path":   "[providers.x]\nbase_url = \"https://x.example\"",
		"missing base_url":   "[providers.x]\napi_path = \"/v1/messages\"",
		"only a key":         "[providers.x]\napi_key = \"$KEY\"",
		"unknown shape":      "[providers.x]\nbase_url = \"https://x.example\"\napi_path = \"/v1/foo\"",
		"base_url userinfo":  "[providers.x]\nbase_url = \"https://user:SECRET@x.example\"\napi_path = \"/v1/messages\"",
		"base_url query":     "[providers.x]\nbase_url = \"https://x.example?key=SECRET\"\napi_path = \"/v1/messages\"",
		"base_url fragment":  "[providers.x]\nbase_url = \"https://x.example#SECRET\"\napi_path = \"/v1/messages\"",
		"base_url scheme":    "[providers.x]\nbase_url = \"ws://x.example\"\napi_path = \"/v1/messages\"",
		"relative api_path":  "[providers.x]\nbase_url = \"https://x.example\"\napi_path = \"v1/messages\"",
		"api_path query":     "[providers.x]\nbase_url = \"https://x.example\"\napi_path = \"/v1/messages?key=SECRET\"",
		"api_path fragment":  "[providers.x]\nbase_url = \"https://x.example\"\napi_path = \"/v1/messages#x\"",
		"decisions api_path": "[providers.x]\nbase_url = \"https://x.example\"\napi_path = \"/v1/decisions\"",
	} {
		_, err := config.ReadFile(write(t, body))
		if err == nil {
			t.Errorf("%s: read without error", name)
			continue
		}
		if !strings.Contains(err.Error(), "[providers.x]") || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: %v, want it to name [providers.x] and not echo a secret", name, err)
		}
	}
}
