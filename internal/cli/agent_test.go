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

// agentGroup is an initialised Node with one Group whose config.toml has
// the Provider "claude", and the path of the Node's config root.
func agentGroup(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir)
	if r := run(t, nil, "", "--dir", dir, "provider", "add", "claude", "--preset", "anthropic"); r.code != 0 {
		t.Fatalf("provider add: exit %d: %s", r.code, r.stderr)
	}
	return dir
}

// agent add writes [agents.<handle>] with the Provider, body and max_turns
// it is given, --model over --body, and scaffolds the profile's SYSTEM.md
// under the config root (ADR 0026), never overwriting one already there. It
// includes the agent nowhere, and says how to.
func TestAgentAddWritesTheProfile(t *testing.T) {
	dir := agentGroup(t)
	system := filepath.Join(dir, "agents", "scribe", "SYSTEM.md")
	if err := os.MkdirAll(filepath.Dir(system), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(system, []byte("You keep notes."), 0o600); err != nil {
		t.Fatal(err)
	}

	r := run(t, nil, "", "--dir", dir, "agent", "add", "auditor", "--provider", "claude",
		"--body", `{"model": "a", "max_tokens": 1024, "thinking": {"type": "adaptive"}}`, "--model", "claude-opus-5-5", "--max-turns", "4")
	if r.code != 0 {
		t.Fatalf("add: exit %d: %s", r.code, r.stderr)
	}
	auditor := filepath.Join(dir, "agents", "auditor", "SYSTEM.md")
	if !strings.Contains(r.stdout, auditor) || !strings.Contains(r.stdout, "include it with: defios agent include auditor") {
		t.Fatalf("add printed %q; want the SYSTEM.md path and the include hint", r.stdout)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "add", "scribe", "--provider", "claude"); r.code != 0 {
		t.Fatalf("add scribe: exit %d: %s", r.code, r.stderr)
	}

	c := groupConfig(t, dir)
	want := map[string]config.Profile{
		"auditor": {Provider: "claude", MaxTurns: 4, Body: map[string]any{
			"model": "claude-opus-5-5", "max_tokens": int64(1024), "thinking": map[string]any{"type": "adaptive"}}},
		"scribe": {Provider: "claude"},
	}
	if !reflect.DeepEqual(c.Agents, want) {
		t.Fatalf("agents = %#v\nwant %#v", c.Agents, want)
	}
	if c.Agent.Profiles != nil {
		t.Fatalf("add included the agent: %v", c.Agent.Profiles)
	}
	if got := readFile(t, auditor); got != "# @auditor\n\nYou are @auditor, an agent in a DeFiOS Group's talk.\nDescribe your purpose here.\n" {
		t.Fatalf("scaffolded SYSTEM.md = %q", got)
	}
	if got := readFile(t, system); got != "You keep notes." {
		t.Fatalf("add overwrote an existing SYSTEM.md: %q", got)
	}
}

// --body reads its object from a file with @path, or from stdin with -.
func TestAgentAddReadsTheBodyFromAFileOrStdin(t *testing.T) {
	dir := agentGroup(t)
	file := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(file, []byte(`{"model": "from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "add", "a", "--provider", "claude", "--body", "@"+file); r.code != 0 {
		t.Fatalf("add --body @file: exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, `{"model": "from-stdin", "temperature": 0.5}`, "--dir", dir, "agent", "add", "b", "--provider", "claude", "--body", "-"); r.code != 0 {
		t.Fatalf("add --body -: exit %d: %s", r.code, r.stderr)
	}
	c := groupConfig(t, dir)
	if got := c.Agents["a"].Body; !reflect.DeepEqual(got, map[string]any{"model": "from-file"}) {
		t.Fatalf("a's body = %v", got)
	}
	if got := c.Agents["b"].Body; !reflect.DeepEqual(got, map[string]any{"model": "from-stdin", "temperature": 0.5}) {
		t.Fatalf("b's body = %v", got)
	}
}

// agent add refuses an unknown Provider, an existing Handle, an invalid
// Handle or "user", a --body that is not a JSON object and a max_turns
// below 1, and a refused add writes nothing.
func TestAgentAddRefuses(t *testing.T) {
	dir := agentGroup(t)
	if r := run(t, nil, "", "--dir", dir, "agent", "add", "auditor", "--provider", "claude"); r.code != 0 {
		t.Fatalf("add: exit %d: %s", r.code, r.stderr)
	}
	before := groupConfig(t, dir)
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"x", "--provider", "gone"}, "no Provider gone; add it with `defios provider add`"},
		{[]string{"x"}, "--provider"},
		{[]string{"auditor", "--provider", "claude"}, "auditor"},
		{[]string{"Auditor", "--provider", "claude"}, "not a valid Handle"},
		{[]string{"user", "--provider", "claude"}, "reserved"},
		{[]string{"x", "--provider", "claude", "--body", `["model"]`}, "JSON object"},
		{[]string{"x", "--provider", "claude", "--body", `{"model": `}, "JSON object"},
		{[]string{"x", "--provider", "claude", "--body", "@/no/such/file"}, "no/such/file"},
		{[]string{"x", "--provider", "claude", "--max-turns", "0"}, "max-turns"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent", "add"}, c.args...)...)
		if r.code != 1 || !strings.Contains(r.stderr, c.says) {
			t.Errorf("%v: exit %d, stderr %q; want 1 saying %q", c.args, r.code, r.stderr, c.says)
		}
	}
	if after := groupConfig(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused adds changed the config:\n%+v\nwant\n%+v", after, before)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "x")); !os.IsNotExist(err) {
		t.Fatalf("a refused add scaffolded a profile directory: %v", err)
	}
}

// agent edit changes only the fields it is given: --body replaces the body
// whole, --model sets only body.model, over the new body when both are
// given. An edit that changes nothing, of an absent Handle, or onto an
// unknown Provider is refused.
func TestAgentEditChangesOnlyWhatItIsGiven(t *testing.T) {
	dir := agentGroup(t)
	if r := run(t, nil, "", "--dir", dir, "provider", "add", "openai"); r.code != 0 {
		t.Fatalf("provider add: exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "add", "auditor", "--provider", "claude",
		"--body", `{"model": "a", "stream": true}`, "--max-turns", "4"); r.code != 0 {
		t.Fatalf("add: exit %d: %s", r.code, r.stderr)
	}
	for _, c := range []struct {
		args []string
		want config.Profile
	}{
		{[]string{"--model", "b"}, config.Profile{Provider: "claude", MaxTurns: 4, Body: map[string]any{"model": "b", "stream": true}}},
		{[]string{"--provider", "openai"}, config.Profile{Provider: "openai", MaxTurns: 4, Body: map[string]any{"model": "b", "stream": true}}},
		{[]string{"--max-turns", "2"}, config.Profile{Provider: "openai", MaxTurns: 2, Body: map[string]any{"model": "b", "stream": true}}},
		{[]string{"--body", `{"store": false}`}, config.Profile{Provider: "openai", MaxTurns: 2, Body: map[string]any{"store": false}}},
		{[]string{"--body", `{"stream": true}`, "--model", "c"}, config.Profile{Provider: "openai", MaxTurns: 2, Body: map[string]any{"model": "c", "stream": true}}},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent", "edit", "auditor"}, c.args...)...)
		if r.code != 0 {
			t.Fatalf("%v: exit %d: %s", c.args, r.code, r.stderr)
		}
		if got := groupConfig(t, dir).Agents["auditor"]; !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%v: edited %#v\nwant %#v", c.args, got, c.want)
		}
	}
	before := groupConfig(t, dir)
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"auditor", "--model", "c"}, "unchanged"},
		{[]string{"auditor"}, "nothing to change"},
		{[]string{"ghost", "--model", "c"}, "no Agent Profile"},
		{[]string{"Ghost", "--model", "c"}, "not a valid Handle"},
		{[]string{"auditor", "--provider", "gone"}, "no Provider gone"},
		{[]string{"auditor", "--max-turns", "-1"}, "max-turns"},
		{[]string{"auditor", "--body", "7"}, "JSON object"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent", "edit"}, c.args...)...)
		if r.code != 1 || !strings.Contains(r.stderr, c.says) {
			t.Errorf("%v: exit %d, stderr %q; want 1 saying %q", c.args, r.code, r.stderr, c.says)
		}
	}
	if after := groupConfig(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused edits changed the config")
	}
}

// agentsWithKeys is a Group with two Keys whose config.toml defines the
// agents auditor and scribe and includes auditor; the ECDSA Key's
// Conversation (ab11) includes scribe instead. It returns the Node's paths.
func agentsWithKeys(t *testing.T) nodedir.Paths {
	t.Helper()
	dir := t.TempDir()
	groupWithKeys(t, dir)
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})
	writeFile(t, p.GroupConfigFile(group.ImplicitAlias), `
[providers.claude]
base_url = "https://api.anthropic.com"
api_path = "/v1/messages"

[agents.scribe]
provider = "claude"

[agents.auditor]
provider = "claude"
max_turns = 4

[agents.auditor.body]
model = "claude-opus-5-5"
stream = true

[agent]
profiles = ["auditor"]
`)
	writeFile(t, p.ConversationConfigFile(group.ImplicitAlias, "ecdsa-ab1111111111"), "[agent]\nprofiles = [\"scribe\"]\n")
	return p
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// agent list prints one line per agent, sorted: its Handle, Provider, model
// and whether it is included, in the Group's [agent] profiles or with --key
// in that Conversation's list merged over it.
func TestAgentListMarksTheIncluded(t *testing.T) {
	p := agentsWithKeys(t)
	r := run(t, nil, "", "--dir", p.Config, "agent", "list")
	want := "auditor  claude  claude-opus-5-5  [included]\nscribe  claude  -\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("list: exit %d, stdout %q, stderr %q\nwant %q", r.code, r.stdout, r.stderr, want)
	}
	r = run(t, nil, "", "--dir", p.Config, "agent", "list", "--key", "ab11")
	want = "auditor  claude  claude-opus-5-5\nscribe  claude  -  [included]\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("list --key: exit %d, stdout %q, stderr %q\nwant %q", r.code, r.stdout, r.stderr, want)
	}
	r = run(t, nil, "", "--dir", p.Config, "agent", "list", "--json")
	var got []map[string]any
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &got) != nil {
		t.Fatalf("list --json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if len(got) != 2 || got[0]["handle"] != "auditor" || got[0]["included"] != true || got[1]["handle"] != "scribe" || got[1]["included"] != false {
		t.Fatalf("list --json = %v", got)
	}
}

// agent show prints every field of one agent, and whether it is included,
// with --key in that Conversation.
func TestAgentShowPrintsEveryField(t *testing.T) {
	p := agentsWithKeys(t)
	dir := p.AgentProfileDir("auditor")
	r := run(t, nil, "", "--dir", p.Config, "agent", "show", "auditor")
	want := "handle auditor\nprovider claude\nmax_turns 4\nprofile_dir " + dir + "\n" +
		`body {"model":"claude-opus-5-5","stream":true}` + "\nincluded yes\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("show: exit %d, stdout %q, stderr %q\nwant %q", r.code, r.stdout, r.stderr, want)
	}
	r = run(t, nil, "", "--dir", p.Config, "agent", "show", "scribe", "--key", "ab11", "--json")
	var got map[string]any
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &got) != nil {
		t.Fatalf("show --json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	wantJSON := map[string]any{"handle": "scribe", "provider": "claude", "profile_dir": p.AgentProfileDir("scribe"), "included": true}
	if !reflect.DeepEqual(got, wantJSON) {
		t.Fatalf("show --key --json = %v\nwant %v", got, wantJSON)
	}
	if r := run(t, nil, "", "--dir", p.Config, "agent", "show", "ghost"); r.code != 1 || !strings.Contains(r.stderr, "ghost") {
		t.Fatalf("show ghost: exit %d, stderr %q", r.code, r.stderr)
	}
}

// agent remove deletes [agents.<handle>] and takes the Handle out of the
// Group's [agent] profiles and out of every Conversation config.toml in the
// Group that lists it, printing each file it changed. A Conversation that
// listed only it keeps an empty list, so it still includes no Group agent.
// The profile directory stays.
func TestAgentRemoveLeavesNoProfilesEntry(t *testing.T) {
	dir := agentGroup(t)
	for _, h := range []string{"auditor", "scribe"} {
		if r := run(t, nil, "", "--dir", dir, "agent", "add", h, "--provider", "claude"); r.code != 0 {
			t.Fatalf("add %s: exit %d: %s", h, r.code, r.stderr)
		}
	}
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})
	groupPath := p.GroupConfigFile(group.ImplicitAlias)
	c := groupConfig(t, dir)
	c.Agent.Profiles = []string{"auditor", "scribe"}
	if err := config.WriteFile(groupPath, c); err != nil {
		t.Fatal(err)
	}
	both := p.ConversationConfigFile(group.ImplicitAlias, "ecdsa-ab1111111111")
	only := p.ConversationConfigFile(group.ImplicitAlias, "eddsa-ab2222222222")
	other := p.ConversationConfigFile(group.ImplicitAlias, "ecdsa-cd3333333333")
	writeFile(t, both, "[agent]\nprofiles = [\"auditor\", \"scribe\"]\nmax_depth = 2\n")
	writeFile(t, only, "[agent]\nprofiles = [\"auditor\"]\n")
	writeFile(t, other, "[agent]\nprofiles = [\"scribe\"]\n")

	r := run(t, nil, "", "--dir", dir, "agent", "remove", "auditor")
	if r.code != 0 {
		t.Fatalf("remove: exit %d: %s", r.code, r.stderr)
	}
	for _, path := range []string{groupPath, both, only} {
		if !strings.Contains(r.stdout, path) {
			t.Errorf("remove did not print %s: %q", path, r.stdout)
		}
	}
	if strings.Contains(r.stdout, other) {
		t.Errorf("remove printed a file it did not change: %q", r.stdout)
	}

	c = groupConfig(t, dir)
	if _, ok := c.Agents["auditor"]; ok || !reflect.DeepEqual(c.Agent.Profiles, []string{"scribe"}) {
		t.Fatalf("Group config: agents %v, profiles %v", c.Agents, c.Agent.Profiles)
	}
	for path, want := range map[string]config.Agent{
		both:  {Profiles: []string{"scribe"}, MaxDepth: 2},
		only:  {Profiles: []string{}},
		other: {Profiles: []string{"scribe"}},
	} {
		w, err := config.ReadWorkspace(path)
		if err != nil || !reflect.DeepEqual(w.Agent, want) {
			t.Errorf("%s: [agent] %#v, %v; want %#v", path, w.Agent, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(p.AgentProfileDir("auditor"), "SYSTEM.md")); err != nil {
		t.Fatalf("remove touched the profile directory: %v", err)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "remove", "auditor"); r.code != 1 || !strings.Contains(r.stderr, "no Agent Profile") {
		t.Fatalf("second remove: exit %d, stderr %q", r.code, r.stderr)
	}
}

// agent delete removes the agent and then its profile directory, which is
// named by the Handle alone (ADR 0026): so it refuses, changing nothing,
// while another Group on this Node defines the Handle, naming that Group.
func TestAgentDeleteRefusesASharedHandle(t *testing.T) {
	dir := agentGroup(t)
	if r := run(t, nil, "", "--dir", dir, "group", "new", "other", "--headscale", "https://hs.example"); r.code != 0 {
		t.Fatalf("group new: exit %d: %s", r.code, r.stderr)
	}
	for _, args := range [][]string{
		{"agent", "add", "auditor", "--provider", "claude", "--group", group.ImplicitAlias},
		{"agent", "add", "scribe", "--provider", "claude", "--group", group.ImplicitAlias},
		{"provider", "add", "claude", "--preset", "anthropic", "--group", "other"},
		{"agent", "add", "auditor", "--provider", "claude", "--group", "other"},
	} {
		if r := run(t, nil, "", append([]string{"--dir", dir}, args...)...); r.code != 0 {
			t.Fatalf("%v: exit %d: %s", args, r.code, r.stderr)
		}
	}
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})

	before := groupConfig(t, dir)
	r := run(t, nil, "", "--dir", dir, "agent", "delete", "auditor", "--group", group.ImplicitAlias)
	if r.code != 1 || !strings.Contains(r.stderr, "other") {
		t.Fatalf("delete of a shared Handle: exit %d, stderr %q; want 1 naming the Group other", r.code, r.stderr)
	}
	if after := groupConfig(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatal("a refused delete changed the config")
	}
	if _, err := os.Stat(p.AgentProfileDir("auditor")); err != nil {
		t.Fatalf("a refused delete touched the profile directory: %v", err)
	}

	r = run(t, nil, "", "--dir", dir, "agent", "delete", "scribe", "--group", group.ImplicitAlias)
	if r.code != 0 || !strings.Contains(r.stdout, p.AgentProfileDir("scribe")) {
		t.Fatalf("delete scribe: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if _, ok := groupConfig(t, dir).Agents["scribe"]; ok {
		t.Fatal("delete left the table")
	}
	if _, err := os.Stat(p.AgentProfileDir("scribe")); !os.IsNotExist(err) {
		t.Fatalf("delete left the profile directory: %v", err)
	}

	if r := run(t, nil, "", "--dir", dir, "agent", "remove", "auditor", "--group", "other"); r.code != 0 {
		t.Fatalf("remove from other: exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "delete", "auditor", "--group", group.ImplicitAlias); r.code != 0 {
		t.Fatalf("delete once unshared: exit %d: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(p.AgentProfileDir("auditor")); !os.IsNotExist(err) {
		t.Fatalf("delete left the profile directory: %v", err)
	}
}
