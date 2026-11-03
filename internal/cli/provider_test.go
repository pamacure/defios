package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/nodedir"
)

// groupConfig is the Group's config.toml under dir, as config reads it.
func groupConfig(t *testing.T, dir string) config.Config {
	t.Helper()
	c, err := config.ReadFile(nodedir.Resolve(nodedir.Lookup{Dir: dir}).GroupConfigFile(group.ImplicitAlias))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// provider add writes a complete [providers.<alias>] table to the Group's
// config.toml, with no serve running: an alias that names a preset uses
// it, --preset names one for any alias, and flags override its fields.
// Without a preset both fields are required, an api_path no shape matches
// is refused, and so is an alias already there; a refused add writes
// nothing.
func TestProviderAddWritesTheGroupsConfig(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir)
	for _, c := range []struct {
		args []string
		want config.Provider
	}{
		{[]string{"anthropic"}, config.Provider{BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages"}},
		{[]string{"local", "--base-url", "http://127.0.0.1:8080", "--api-path", "/v1/chat/completions"},
			config.Provider{BaseURL: "http://127.0.0.1:8080", APIPath: "/v1/chat/completions"}},
		{[]string{"openai"}, config.Provider{BaseURL: "https://api.openai.com", APIPath: "/v1/responses"}},
		{[]string{"openai-cc", "--preset", "openai", "--api-path", "/v1/chat/completions"},
			config.Provider{BaseURL: "https://api.openai.com", APIPath: "/v1/chat/completions"}},
		{[]string{"work", "--preset", "anthropic"}, config.Provider{BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages"}},
		{[]string{"proxied", "--preset", "anthropic", "--base-url", "https://proxy.example"},
			config.Provider{BaseURL: "https://proxy.example", APIPath: "/v1/messages"}},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "provider", "add"}, c.args...)...)
		if r.code != 0 {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", c.args, r.code, r.stdout, r.stderr)
		}
		if got := groupConfig(t, dir).Providers[c.args[0]]; !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%v: wrote %+v, want %+v", c.args, got, c.want)
		}
	}
	before := groupConfig(t, dir)
	for _, args := range [][]string{
		{"x", "--base-url", "https://x.example", "--api-path", "/v1/foo"},
		{"x", "--api-path", "/v1/foo"},
		{"x"},
		{"x", "--base-url", "https://x.example"},
		{"x", "--preset", "acme"},
		{"x", "--preset", "anthropic", "--base-url", "https://u:p@x.example"},
		{"anthropic", "--api-path", "/v1/decisions"},
		{"anthropic"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "provider", "add"}, args...)...)
		if r.code != 1 {
			t.Errorf("%v: exit %d, stderr %q; want 1", args, r.code, r.stderr)
		}
	}
	if after := groupConfig(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused adds changed the config:\n%+v\nwant\n%+v", after, before)
	}
	if c := groupConfig(t, dir); c.HeadscaleURL != "https://hs.example" || c.DisplayName == "" {
		t.Fatalf("add lost the rest of the config: %+v", c)
	}
}

// provider edit changes only the fields it is given, keeping the table's
// api_key and body; an edit of an absent alias, or one that changes
// nothing, is refused. provider remove drops the whole table.
func TestProviderEditAndRemoveTheTable(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir)
	path := nodedir.Resolve(nodedir.Lookup{Dir: dir}).GroupConfigFile(group.ImplicitAlias)
	c := groupConfig(t, dir)
	c.Providers = map[string]config.Provider{"claude": {
		BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages", APIKey: "$ANTHROPIC",
		Body: map[string]any{"stream": true, "thinking": map[string]any{"type": "adaptive"}},
	}}
	if err := config.WriteFile(path, c); err != nil {
		t.Fatal(err)
	}
	if r := run(t, nil, "", "--dir", dir, "provider", "edit", "claude", "--base-url", "http://127.0.0.1:8080"); r.code != 0 {
		t.Fatalf("edit: exit %d: %s", r.code, r.stderr)
	}
	want := config.Provider{
		BaseURL: "http://127.0.0.1:8080", APIPath: "/v1/messages", APIKey: "$ANTHROPIC",
		Body: map[string]any{"stream": true, "thinking": map[string]any{"type": "adaptive"}},
	}
	if got := groupConfig(t, dir).Providers["claude"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("edited %+v\nwant   %+v", got, want)
	}
	for _, args := range [][]string{
		{"edit", "ghost", "--base-url", "https://x.example"},
		{"edit", "claude", "--base-url", "http://127.0.0.1:8080"},
		{"edit", "claude", "--api-path", "/v1/foo"},
		{"remove", "ghost"},
	} {
		if r := run(t, nil, "", append([]string{"--dir", dir, "provider"}, args...)...); r.code != 1 {
			t.Errorf("%v: exit %d, stderr %q; want 1", args, r.code, r.stderr)
		}
	}
	if r := run(t, nil, "", "--dir", dir, "provider", "remove", "claude"); r.code != 0 {
		t.Fatalf("remove: exit %d: %s", r.code, r.stderr)
	}
	if _, ok := groupConfig(t, dir).Providers["claude"]; ok {
		t.Fatal("remove left the table")
	}
}

// provider add and edit set api_key, a Secret reference, and the base body,
// a JSON object read as agent add --body reads it. A literal key is
// refused and writes nothing. On edit, --body replaces the whole body,
// --body '{}' clears it and an empty --api-key removes the key; an edit to what
// is already there is refused as unchanged.
func TestProviderAddAndEditSetTheKeyAndBody(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir)
	r := run(t, nil, "", "--dir", dir, "provider", "add", "x", "--preset", "anthropic",
		"--api-key", "$ANTHROPIC_KEY", "--body", `{"stream":true, "max_tokens": 1024}`)
	if r.code != 0 {
		t.Fatalf("add: exit %d: %s", r.code, r.stderr)
	}
	want := config.Provider{BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages", APIKey: "$ANTHROPIC_KEY",
		Body: map[string]any{"stream": true, "max_tokens": int64(1024)}}
	if got := groupConfig(t, dir).Providers["x"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("added %#v\nwant  %#v", got, want)
	}

	before := groupConfig(t, dir)
	for _, args := range [][]string{
		{"add", "y", "--preset", "anthropic", "--api-key", "sk-ant-literal"},
		{"add", "y", "--preset", "anthropic", "--body", `["stream"]`},
		{"add", "y", "--preset", "anthropic", "--body", `{"": 1}`},
		{"edit", "x", "--body", `{"": 1}`},
		{"edit", "x", "--api-key", "sk-ant-literal"},
		{"edit", "x", "--api-key", "$lower"},
		{"edit", "x", "--body", "@/no/such/file"},
		{"edit", "x", "--api-key", "$ANTHROPIC_KEY"},
		{"edit", "x", "--body", `{"max_tokens": 1024, "stream": true}`},
	} {
		if r := run(t, nil, "", append([]string{"--dir", dir, "provider"}, args...)...); r.code != 1 {
			t.Errorf("%v: exit %d, stderr %q; want 1", args, r.code, r.stderr)
		}
	}
	if after := groupConfig(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused adds and edits changed the config:\n%+v\nwant\n%+v", after, before)
	}

	if r := run(t, nil, `{"temperature": 0.5}`, "--dir", dir, "provider", "edit", "x", "--body", "-", "--api-key", "$OTHER"); r.code != 0 {
		t.Fatalf("edit --body -: exit %d: %s", r.code, r.stderr)
	}
	want.APIKey, want.Body = "$OTHER", map[string]any{"temperature": 0.5}
	if got := groupConfig(t, dir).Providers["x"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("edited %#v\nwant   %#v", got, want)
	}
	if r := run(t, nil, "", "--dir", dir, "provider", "edit", "x", "--body", "{}", "--api-key", ""); r.code != 0 {
		t.Fatalf("edit clearing: exit %d: %s", r.code, r.stderr)
	}
	want.APIKey, want.Body = "", nil
	if got := groupConfig(t, dir).Providers["x"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleared %#v\nwant    %#v", got, want)
	}
	if r := run(t, nil, "", "--dir", dir, "provider", "edit", "x", "--body", "{}"); r.code != 1 || !strings.Contains(r.stderr, "nothing to change") {
		t.Fatalf("clearing a cleared body: exit %d, stderr %q; want 1, nothing to change", r.code, r.stderr)
	}
}

// provider list and show read the config: the Group's, or with --key that
// Key's Conversation config.toml merged over it. Neither shows the key's
// value, only its reference.
func TestProviderListAndShowReadTheMergedConfig(t *testing.T) {
	dir := t.TempDir()
	groupWithKeys(t, dir)
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})
	if err := os.MkdirAll(p.GroupConfigDir(group.ImplicitAlias), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.GroupConfigFile(group.ImplicitAlias), []byte(`
[providers.claude]
base_url = "https://api.anthropic.com"
api_path = "/v1/messages"
api_key = "$ANTHROPIC"

[providers.local]
base_url = "http://127.0.0.1:8080"
api_path = "/v1/chat/completions"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	conv := p.ConversationConfigFile(group.ImplicitAlias, "ecdsa-ab1111111111")
	if err := os.MkdirAll(filepath.Dir(conv), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conv, []byte("[providers.claude]\nbase_url = \"https://gateway.example\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := run(t, nil, "", "--dir", dir, "provider", "list")
	want := "claude  https://api.anthropic.com/v1/messages  messages\nlocal  http://127.0.0.1:8080/v1/chat/completions  chat_completions\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("list: exit %d, stdout %q, stderr %q\nwant %q", r.code, r.stdout, r.stderr, want)
	}

	r = run(t, nil, "", "--dir", dir, "provider", "show", "claude", "--key", "ab11", "--json")
	var got map[string]any
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &got) != nil {
		t.Fatalf("show --key: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	wantShow := map[string]any{"alias": "claude", "base_url": "https://gateway.example", "api_path": "/v1/messages", "shape": "messages", "api_key": "$ANTHROPIC"}
	if !reflect.DeepEqual(got, wantShow) {
		t.Fatalf("show --key = %v\nwant %v", got, wantShow)
	}

	if r := run(t, nil, "", "--dir", dir, "provider", "show", "ghost"); r.code != 1 || !strings.Contains(r.stderr, "no such provider") {
		t.Fatalf("show ghost: exit %d, stderr %q", r.code, r.stderr)
	}
}

// The model command is gone.
func TestThereIsNoModelCommand(t *testing.T) {
	if r := run(t, nil, "", "model", "list"); r.code == 0 {
		t.Fatalf("model list exit 0: %s", r.stdout)
	}
}
