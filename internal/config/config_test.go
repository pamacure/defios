package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/nodedir"
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
rpc_urls = ["$ALCHEMY_ETH", "$INFURA_ETH"]

[networks.btc]
rpc_urls = ["$BTC_RPC"]

[providers.anthropic]
api_key = "$DEFIOS_ANTHROPIC"
base_url = "https://proxy.example"

[agents.auditor]
dir = "agents/auditor"
model = "claude"

[agents.trader]
model = "gpt"

[agent]
profiles = ["auditor"]
max_depth = 2
`

// The Group config.toml holds this Node's tailnet presence, its Agent
// Profiles and the three Workspace sections, every secret as a reference.
func TestGroupConfigReadsTheWorkspaceSectionsAndAgentProfiles(t *testing.T) {
	c, err := config.ReadFile(write(t, groupConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Workspace{
		Networks: map[string]config.Network{
			"eth": {RPCURLs: []string{"$ALCHEMY_ETH", "$INFURA_ETH"}},
			"btc": {RPCURLs: []string{"$BTC_RPC"}},
		},
		Providers: map[string]config.Provider{"anthropic": {APIKey: "$DEFIOS_ANTHROPIC", BaseURL: "https://proxy.example"}},
		Agent:     config.Agent{Profiles: []string{"auditor"}, MaxDepth: 2},
	}
	if !reflect.DeepEqual(c.Workspace, want) || c.TailnetAuthKey != "$TS_AUTHKEY" {
		t.Fatalf("read %+v", c)
	}
	wantAgents := map[string]config.Profile{"auditor": {Dir: "agents/auditor", Model: "claude"}, "trader": {Model: "gpt"}}
	if !reflect.DeepEqual(c.Agents, wantAgents) {
		t.Fatalf("read %+v", c)
	}
}

// A plaintext secret in a config is an error when the file is read, in
// every secret-bearing field; so is a key the file does not define.
func TestConfigRefusesLiteralSecretsAndUnknownKeys(t *testing.T) {
	for name, body := range map[string]string{
		"literal tailnet_auth_key": `tailnet_auth_key = "tskey-auth-abc"`,
		"literal rpc url":          "[networks.eth]\nrpc_urls = [\"https://eth.example/v2/KEY\"]",
		"literal api_key":          "[providers.anthropic]\napi_key = \"sk-ant-abc\"",
		"interpolated reference":   "[networks.eth]\nrpc_urls = [\"https://eth.example/v2/$KEY\"]",
		"lowercase reference":      "[providers.openai]\napi_key = \"$openai\"",
		"old auth_key":             `auth_key = "tskey-auth-abc"`,
		"unknown provider":         "[providers.mistral]\napi_key = \"$KEY\"",
		"bad base_url":             "[providers.openai]\nbase_url = \"ftp://x\"",
	} {
		if _, err := config.ReadFile(write(t, body)); err == nil {
			t.Errorf("%s: read without error", name)
		} else if strings.Contains(err.Error(), "abc") {
			t.Errorf("%s: error echoes the secret: %v", name, err)
		}
	}
}

// A Conversation's config.toml holds only the Workspace sections; a missing
// one is an empty Workspace.
func TestConversationConfigTakesOnlyTheWorkspaceSections(t *testing.T) {
	if _, err := config.ReadWorkspace(write(t, "hostname = \"x\"\n[agent]\nprofiles = [\"auditor\"]")); err == nil {
		t.Fatal("a top-level key in a Conversation config must be an error")
	}
	if _, err := config.ReadWorkspace(write(t, "[networks.eth]\nrpc_urls = [\"https://literal.example\"]")); err == nil {
		t.Fatal("a literal rpc url in a Conversation config must be an error")
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
rpc_urls = ["$CONV_ETH"]

[providers.anthropic]
base_url = "https://other-proxy.example"

[providers.openai]
api_key = "$DEFIOS_OPENAI"

[agent]
profiles = ["trader"]
`))
	if err != nil {
		t.Fatal(err)
	}
	got := group.Workspace.Merge(conv)
	want := config.Workspace{
		Networks: map[string]config.Network{
			"eth": {RPCURLs: []string{"$CONV_ETH"}},
			"btc": {RPCURLs: []string{"$BTC_RPC"}},
		},
		Providers: map[string]config.Provider{
			"anthropic": {APIKey: "$DEFIOS_ANTHROPIC", BaseURL: "https://other-proxy.example"},
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

// prune is this Node's policy for the Group, off unless set, and only the
// Group's config.toml may set it.
func TestPrunePolicyDefaultsOffAndIsGroupOnly(t *testing.T) {
	c, err := config.ReadFile(write(t, groupConfig))
	if err != nil || c.Prune {
		t.Fatalf("prune unset = %v, %v; want off", c.Prune, err)
	}
	c, err = config.ReadFile(write(t, "prune = true\n"+groupConfig))
	if err != nil || !c.Prune {
		t.Fatalf("prune = true read as %v, %v", c.Prune, err)
	}
	if _, err := config.ReadFile(write(t, `prune = "on"`)); err == nil {
		t.Fatal(`prune = "on" must be an error: the policy is a boolean`)
	}
	if _, err := config.ReadWorkspace(write(t, "prune = true")); err == nil {
		t.Fatal("prune in a Conversation config must be an error")
	}
}

// An Agent Profile is defined only in the Group's config.toml, under a Handle
// that a mention can name (ADR 0016) and is not "user", and names the Model
// it runs on; [agent] includes only Handles the Group defines, at a depth of
// at least 1.
func TestConfigRefusesMalformedAgentProfiles(t *testing.T) {
	for name, body := range map[string]string{
		"bad handle":          "[agents.Auditor]\nmodel = \"claude\"",
		"reserved handle":     "[agents.user]\nmodel = \"claude\"",
		"handle ending in -":  "[agents.auditor-]\nmodel = \"claude\"",
		"handle ending in _":  "[agents.auditor_]\nmodel = \"claude\"",
		"handle with --":      "[agents.au--di]\nmodel = \"claude\"",
		"missing model":       "[agents.auditor]\ndir = \"agents/auditor\"",
		"bad model alias":     "[agents.auditor]\nmodel = \"Claude!\"",
		"undefined handle":    "[agents.auditor]\nmodel = \"claude\"\n[agent]\nprofiles = [\"trader\"]",
		"handle listed twice": "[agents.auditor]\nmodel = \"claude\"\n[agent]\nprofiles = [\"auditor\", \"auditor\"]",
		"zero max_depth":      "[agent]\nmax_depth = 0",
		"negative max_depth":  "[agent]\nmax_depth = -1",
		"old [agent] model":   "[agent]\nmodel = \"claude\"",
		"old [agent] window":  "[agent]\ntoken_window = 200000",
		"unknown profile key": "[agents.auditor]\nmodel = \"claude\"\nsoul = \"SOUL.md\"",
	} {
		if _, err := config.ReadFile(write(t, body)); err == nil {
			t.Errorf("%s: read without error", name)
		}
	}
	for name, body := range map[string]string{
		"profile in a Conversation": "[agents.auditor]\nmodel = \"claude\"",
		"bad handle in profiles":    "[agent]\nprofiles = [\"Auditor\"]",
		"zero max_depth":            "[agent]\nmax_depth = 0",
	} {
		if _, err := config.ReadWorkspace(write(t, body)); err == nil {
			t.Errorf("Conversation %s: read without error", name)
		}
	}
}

// Included is what may answer in one place: the Group's included profiles, or
// the Conversation's list in their place, each with its profile directory
// resolved, and the depth a chain may reach.
func TestIncludedResolvesTheProfiles(t *testing.T) {
	c, err := config.ReadFile(write(t, `
[agents.auditor]
model = "claude"

[agents.trader]
dir = "/srv/agents/trader"
model = "gpt"

[agents.scout]
dir = "~/agents/scout"
model = "claude"

[agents.quant]
dir = "shared/quant"
model = "gpt"

[agent]
profiles = ["auditor"]
max_depth = 3
`))
	if err != nil {
		t.Fatal(err)
	}
	const root, home = "/home/op/.config/defios", "/home/op"

	got, err := c.Included(config.Workspace{}, root, home)
	want := config.Included{
		Profiles: []config.ResolvedProfile{{Handle: "auditor", Dir: "/home/op/.config/defios/agents/auditor", Model: "claude"}},
		MaxDepth: 3,
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Group-level = %+v, %v\nwant %+v", got, err, want)
	}

	got, err = c.Included(config.Workspace{Agent: config.Agent{Profiles: []string{"quant", "trader", "scout"}}}, root, home)
	want = config.Included{
		Profiles: []config.ResolvedProfile{
			{Handle: "quant", Dir: "/home/op/.config/defios/shared/quant", Model: "gpt"},
			{Handle: "trader", Dir: "/srv/agents/trader", Model: "gpt"},
			{Handle: "scout", Dir: "/home/op/agents/scout", Model: "claude"},
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
	got, err = c.Included(cleared, root, home)
	if err != nil || len(got.Profiles) != 0 || got.MaxDepth != 2 {
		t.Fatalf("Conversation with profiles = [] = %+v, %v; want none, depth 2", got, err)
	}

	if _, err := c.Included(config.Workspace{Agent: config.Agent{Profiles: []string{"ghost"}}}, root, home); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("a Conversation including an undefined Handle = %v; want an error naming it", err)
	}

	none, err := config.Config{}.Included(config.Workspace{}, root, home)
	if err != nil || len(none.Profiles) != 0 || none.MaxDepth != 1 {
		t.Fatalf("no agents configured = %+v, %v; want none, depth 1", none, err)
	}
}
