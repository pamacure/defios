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

// profile is the Agent Profile config.toml of handle on the Node at dir.
func profile(t *testing.T, dir, handle string) config.Profile {
	t.Helper()
	p, err := config.ReadProfile(nodedir.Resolve(nodedir.Lookup{Dir: dir}).AgentProfileFile(handle))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// agent add writes agents/<handle>/config.toml with the Provider, body and
// max_turns it is given, --model over --body, and scaffolds the profile's
// SYSTEM.md beside it (ADR 0026, ADR 0029), never overwriting one already
// there. It touches no Group's config.toml, so includes the agent nowhere,
// and says how to.
func TestAgentAddWritesTheProfile(t *testing.T) {
	dir := agentGroup(t)
	system := filepath.Join(dir, "agents", "scribe", "SYSTEM.md")
	if err := os.MkdirAll(filepath.Dir(system), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(system, []byte("You keep notes."), 0o600); err != nil {
		t.Fatal(err)
	}
	groupBefore := readFile(t, nodedir.Resolve(nodedir.Lookup{Dir: dir}).GroupConfigFile(group.ImplicitAlias))

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

	want := map[string]config.Profile{
		"auditor": {Provider: "claude", MaxTurns: 4, Body: map[string]any{
			"model": "claude-opus-5-5", "max_tokens": int64(1024), "thinking": map[string]any{"type": "adaptive"}}},
		"scribe": {Provider: "claude"},
	}
	for h, w := range want {
		if got := profile(t, dir, h); !reflect.DeepEqual(got, w) {
			t.Fatalf("%s = %#v\nwant %#v", h, got, w)
		}
	}
	if got := readFile(t, nodedir.Resolve(nodedir.Lookup{Dir: dir}).GroupConfigFile(group.ImplicitAlias)); got != groupBefore {
		t.Fatalf("add changed the Group's config.toml:\n%s", got)
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
	if got := profile(t, dir, "a").Body; !reflect.DeepEqual(got, map[string]any{"model": "from-file"}) {
		t.Fatalf("a's body = %v", got)
	}
	if got := profile(t, dir, "b").Body; !reflect.DeepEqual(got, map[string]any{"model": "from-stdin", "temperature": 0.5}) {
		t.Fatalf("b's body = %v", got)
	}
}

// agent add refuses no Provider, an existing Handle, an invalid Handle or
// "user", a --body that is not a JSON object and a max_turns below 1, and a
// refused add writes nothing. Whether a Group has the Provider is found out
// at summon, for the profile is the Node's, not one Group's.
func TestAgentAddRefuses(t *testing.T) {
	dir := agentGroup(t)
	if r := run(t, nil, "", "--dir", dir, "agent", "add", "auditor", "--provider", "claude"); r.code != 0 {
		t.Fatalf("add: exit %d: %s", r.code, r.stderr)
	}
	before := profile(t, dir, "auditor")
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"x"}, "--provider"},
		{[]string{"x", "--provider", "Bad!"}, "provider"},
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
	if after := profile(t, dir, "auditor"); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused adds changed the profile:\n%+v\nwant\n%+v", after, before)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "x")); !os.IsNotExist(err) {
		t.Fatalf("a refused add scaffolded a profile directory: %v", err)
	}
}

// agent edit changes only the fields it is given: --body replaces the body
// whole, --model sets only body.model, over the new body when both are
// given. An edit that changes nothing or of an absent Handle is refused.
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
		if got := profile(t, dir, "auditor"); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%v: edited %#v\nwant %#v", c.args, got, c.want)
		}
	}
	before := profile(t, dir, "auditor")
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"auditor", "--model", "c"}, "unchanged"},
		{[]string{"auditor"}, "nothing to change"},
		{[]string{"ghost", "--model", "c"}, "no Agent Profile"},
		{[]string{"Ghost", "--model", "c"}, "not a valid Handle"},
		{[]string{"auditor", "--provider", "Bad!"}, "provider"},
		{[]string{"auditor", "--max-turns", "-1"}, "max-turns"},
		{[]string{"auditor", "--body", "7"}, "JSON object"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent", "edit"}, c.args...)...)
		if r.code != 1 || !strings.Contains(r.stderr, c.says) {
			t.Errorf("%v: exit %d, stderr %q; want 1 saying %q", c.args, r.code, r.stderr, c.says)
		}
	}
	if after := profile(t, dir, "auditor"); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused edits changed the profile")
	}
}

// agentsWithKeys is a Group with two Keys, and the Node's Agent Profiles
// auditor and scribe: the Group's config.toml includes auditor, the ECDSA
// Key's Conversation (ab11) scribe instead. It returns the Node's paths.
func agentsWithKeys(t *testing.T) nodedir.Paths {
	t.Helper()
	dir := t.TempDir()
	groupWithKeys(t, dir)
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})
	writeFile(t, p.GroupConfigFile(group.ImplicitAlias), `
[providers.claude]
base_url = "https://api.anthropic.com"
api_path = "/v1/messages"

[agent]
profiles = ["auditor"]
`)
	writeFile(t, p.AgentProfileFile("scribe"), "provider = \"claude\"\n")
	writeFile(t, p.AgentProfileFile("auditor"), "provider = \"claude\"\nmax_turns = 4\n\n[body]\nmodel = \"claude-opus-5-5\"\nstream = true\n")
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

// agent list prints one line per Agent Profile on the Node, sorted: its
// Handle, Provider, model and whether it is included, in the Current
// Group's [agent] profiles or with --key in that Conversation's list merged
// over it. A directory with no config.toml is not a profile.
func TestAgentListMarksTheIncluded(t *testing.T) {
	p := agentsWithKeys(t)
	writeFile(t, filepath.Join(p.AgentProfileDir("draft"), "SYSTEM.md"), "unfinished")
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

// agent show prints every field of one profile and where on the Node it is
// included: each Group whose [agent] profiles lists it, and each
// Conversation whose own list does, as <alias>/<workspace>.
func TestAgentShowPrintsEveryFieldAndWhereItIsIncluded(t *testing.T) {
	p := agentsWithKeys(t)
	dir := p.AgentProfileDir("auditor")
	r := run(t, nil, "", "--dir", p.Config, "agent", "show", "auditor")
	want := "handle auditor\nprovider claude\nmax_turns 4\ntools say\nprofile_dir " + dir + "\n" +
		`body {"model":"claude-opus-5-5","stream":true}` + "\nincluded_in default\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("show: exit %d, stdout %q, stderr %q\nwant %q", r.code, r.stdout, r.stderr, want)
	}
	r = run(t, nil, "", "--dir", p.Config, "agent", "show", "scribe", "--json")
	var got map[string]any
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &got) != nil {
		t.Fatalf("show --json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	wantJSON := map[string]any{"handle": "scribe", "provider": "claude", "profile_dir": p.AgentProfileDir("scribe"), "tools": []any{"say"},
		"included_in": []any{"default/ecdsa-ab1111111111"}}
	if !reflect.DeepEqual(got, wantJSON) {
		t.Fatalf("show --json = %v\nwant %v", got, wantJSON)
	}
	if r := run(t, nil, "", "--dir", p.Config, "agent", "show", "ghost"); r.code != 1 || !strings.Contains(r.stderr, "ghost") {
		t.Fatalf("show ghost: exit %d, stderr %q", r.code, r.stderr)
	}
}

// agent grant and revoke edit a profile's tools, the whole list of Tools it
// may call (ADR 0029), and print it: granting into an unset list first
// writes say with it, revoking every Tool leaves an empty list, and a Tool
// granted again or revoked when not granted changes nothing. An unknown Tool
// or Handle is refused, naming the Tools there are; show prints the list.
func TestAgentGrantRevokeEditTheTools(t *testing.T) {
	dir := agentGroup(t)
	if r := run(t, nil, "", "--dir", dir, "agent", "add", "auditor", "--provider", "claude"); r.code != 0 {
		t.Fatalf("add: exit %d: %s", r.code, r.stderr)
	}
	for _, tc := range []struct {
		args   []string
		stdout string
		tools  []string
	}{
		{[]string{"grant", "auditor", "sign_list"}, "say\nsign_list\n", []string{"say", "sign_list"}},
		{[]string{"grant", "auditor", "sign_list"}, "say\nsign_list\n", []string{"say", "sign_list"}},
		{[]string{"grant", "auditor", "sign_inspect", "--json"}, "[\n  \"say\",\n  \"sign_list\",\n  \"sign_inspect\"\n]\n", []string{"say", "sign_list", "sign_inspect"}},
		{[]string{"revoke", "auditor", "say"}, "sign_list\nsign_inspect\n", []string{"sign_list", "sign_inspect"}},
		{[]string{"revoke", "auditor", "say"}, "sign_list\nsign_inspect\n", []string{"sign_list", "sign_inspect"}},
		{[]string{"revoke", "auditor", "sign_list"}, "sign_inspect\n", []string{"sign_inspect"}},
		{[]string{"revoke", "auditor", "sign_inspect", "--json"}, "[]\n", []string{}},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent"}, tc.args...)...)
		if r.code != 0 || r.stdout != tc.stdout {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want %q", tc.args, r.code, r.stdout, r.stderr, tc.stdout)
		}
		if got := profile(t, dir, "auditor").Tools; !reflect.DeepEqual(got, tc.tools) {
			t.Fatalf("%v: tools = %#v, want %#v", tc.args, got, tc.tools)
		}
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "show", "auditor"); r.code != 0 || !strings.Contains(r.stdout, "\ntools none\n") {
		t.Fatalf("show: exit %d, stdout %q", r.code, r.stdout)
	}
	before := readFile(t, nodedir.Resolve(nodedir.Lookup{Dir: dir}).AgentProfileFile("auditor"))
	for _, args := range [][]string{
		{"grant", "auditor", "sign_everything"},
		{"revoke", "auditor", "sign_everything"},
		{"grant", "ghost", "say"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent"}, args...)...)
		if r.code != 1 || !strings.Contains(r.stderr, args[1]) && !strings.Contains(r.stderr, args[2]) {
			t.Errorf("%v: exit %d, stderr %q", args, r.code, r.stderr)
		}
		if args[2] == "sign_everything" && !strings.Contains(r.stderr, "sign_inspect") {
			t.Errorf("%v: stderr %q, want the Tools there are", args, r.stderr)
		}
	}
	if got := readFile(t, nodedir.Resolve(nodedir.Lookup{Dir: dir}).AgentProfileFile("auditor")); got != before {
		t.Fatalf("a refused grant changed the profile:\n%s", got)
	}
}

// agent delete deletes a profile's directory, its config.toml and SYSTEM.md
// with it. The profile is the whole Node's (ADR 0029), so delete refuses,
// changing nothing, while any Group or Conversation on the Node includes
// the Handle, naming each: exclude it there first.
func TestAgentDeleteRefusesWhileIncluded(t *testing.T) {
	dir := agentGroup(t)
	if r := run(t, nil, "", "--dir", dir, "group", "new", "other", "--headscale", "https://hs.example"); r.code != 0 {
		t.Fatalf("group new: exit %d: %s", r.code, r.stderr)
	}
	for _, args := range [][]string{
		{"agent", "add", "auditor", "--provider", "claude"},
		{"agent", "add", "scribe", "--provider", "claude"},
		{"agent", "include", "auditor", "--group", group.ImplicitAlias},
		{"agent", "include", "auditor", "--group", "other"},
	} {
		if r := run(t, nil, "", append([]string{"--dir", dir}, args...)...); r.code != 0 {
			t.Fatalf("%v: exit %d: %s", args, r.code, r.stderr)
		}
	}
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})
	conv := p.ConversationConfigFile(group.ImplicitAlias, "ecdsa-ab1111111111")
	writeFile(t, conv, "[agent]\nprofiles = [\"scribe\"]\n")

	for h, says := range map[string][]string{
		"auditor": {group.ImplicitAlias, "other"},
		"scribe":  {group.ImplicitAlias + "/ecdsa-ab1111111111"},
	} {
		r := run(t, nil, "", "--dir", dir, "agent", "delete", h)
		for _, s := range says {
			if r.code != 1 || !strings.Contains(r.stderr, s) {
				t.Errorf("delete %s while included: exit %d, stderr %q; want 1 naming %s", h, r.code, r.stderr, s)
			}
		}
		if _, err := os.Stat(p.AgentProfileFile(h)); err != nil {
			t.Fatalf("a refused delete touched %s's profile: %v", h, err)
		}
	}

	if r := run(t, nil, "", "--dir", dir, "agent", "exclude", "auditor", "--group", "other"); r.code != 0 {
		t.Fatalf("exclude from other: exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "delete", "auditor"); r.code != 1 || strings.Contains(r.stderr, "other") || !strings.Contains(r.stderr, group.ImplicitAlias) {
		t.Fatalf("delete included in one Group: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "exclude", "auditor", "--group", group.ImplicitAlias); r.code != 0 {
		t.Fatalf("exclude: exit %d: %s", r.code, r.stderr)
	}
	r := run(t, nil, "", "--dir", dir, "agent", "delete", "auditor")
	if r.code != 0 || !strings.Contains(r.stdout, p.AgentProfileDir("auditor")) {
		t.Fatalf("delete once excluded: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(p.AgentProfileDir("auditor")); !os.IsNotExist(err) {
		t.Fatalf("delete left the profile directory: %v", err)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "delete", "auditor"); r.code != 1 || !strings.Contains(r.stderr, "no Agent Profile") {
		t.Fatalf("second delete: exit %d, stderr %q", r.code, r.stderr)
	}
}

// agent include and exclude without --key edit the Group's [agent] profiles
// and print the Handles mentionable there, one per line or as a JSON array.
// Including an included Handle or excluding an absent one changes nothing
// and is not an error, even with no profile, so a list naming a profile
// deleted by hand can be fixed; excluding the last leaves profiles = [],
// never no key. Including a Handle with no Agent Profile is refused.
func TestAgentIncludeExcludeEditTheGroupsList(t *testing.T) {
	p := agentsWithKeys(t)
	path := p.GroupConfigFile(group.ImplicitAlias)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"include", "scribe"}, "auditor\nscribe\n"},
		{[]string{"include", "scribe"}, "auditor\nscribe\n"},
		{[]string{"exclude", "auditor"}, "scribe\n"},
		{[]string{"exclude", "auditor"}, "scribe\n"},
		{[]string{"exclude", "scribe"}, ""},
	} {
		r := run(t, nil, "", append([]string{"--dir", p.Config, "agent"}, c.args...)...)
		if r.code != 0 || r.stdout != c.want {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want %q", c.args, r.code, r.stdout, r.stderr, c.want)
		}
	}
	if got := readFile(t, path); !strings.Contains(got, "profiles = []") {
		t.Fatalf("excluding the last Handle dropped the key:\n%s", got)
	}
	c, err := config.ReadFile(path)
	if err != nil || c.Agent.Profiles == nil || len(c.Agent.Profiles) != 0 {
		t.Fatalf("Group config: profiles %#v, %v", c.Agent.Profiles, err)
	}

	r := run(t, nil, "", "--dir", p.Config, "agent", "include", "auditor", "--json")
	var got []string
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &got) != nil || !reflect.DeepEqual(got, []string{"auditor"}) {
		t.Fatalf("include --json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	r = run(t, nil, "", "--dir", p.Config, "agent", "exclude", "auditor", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "[]" {
		t.Fatalf("exclude --json of the last: exit %d, stdout %q; want []", r.code, r.stdout)
	}

	before := readFile(t, path)
	if r := run(t, nil, "", "--dir", p.Config, "agent", "include", "ghost"); r.code != 1 || !strings.Contains(r.stderr, "no Agent Profile \"ghost\"") {
		t.Errorf("include ghost: exit %d, stderr %q; want 1 naming ghost", r.code, r.stderr)
	}
	if after := readFile(t, path); after != before {
		t.Fatal("a refused include changed the config")
	}
}

// With --key, agent include and exclude edit that Conversation's
// config.toml, keeping its other sections, and print its list.
func TestAgentIncludeExcludeEditAConversationsList(t *testing.T) {
	p := agentsWithKeys(t)
	path := p.ConversationConfigFile(group.ImplicitAlias, "ecdsa-ab1111111111")
	writeFile(t, path, "[providers.claude]\nbase_url = \"https://gateway.example\"\n\n[agent]\nprofiles = [\"scribe\"]\nmax_depth = 2\n")
	groupBefore := readFile(t, p.GroupConfigFile(group.ImplicitAlias))

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"include", "auditor"}, "scribe\nauditor\n"},
		{[]string{"include", "auditor"}, "scribe\nauditor\n"},
		{[]string{"exclude", "scribe"}, "auditor\n"},
		{[]string{"exclude", "scribe"}, "auditor\n"},
		{[]string{"exclude", "auditor"}, ""},
	} {
		r := run(t, nil, "", append([]string{"--dir", p.Config, "agent"}, append(c.args, "--key", "ab11")...)...)
		if r.code != 0 || r.stdout != c.want || r.stderr != "" {
			t.Fatalf("%v --key: exit %d, stdout %q, stderr %q; want %q", c.args, r.code, r.stdout, r.stderr, c.want)
		}
	}
	w, err := config.ReadWorkspace(path)
	want := config.Workspace{
		Providers: map[string]config.Provider{"claude": {BaseURL: "https://gateway.example"}},
		Agent:     config.Agent{Profiles: []string{}, MaxDepth: 2},
	}
	if err != nil || !reflect.DeepEqual(w, want) {
		t.Fatalf("Conversation config = %#v, %v\nwant %#v", w, err, want)
	}
	if readFile(t, p.GroupConfigFile(group.ImplicitAlias)) != groupBefore {
		t.Fatal("--key changed the Group's config.toml")
	}
}

// On a Conversation with no list of its own, include --key writes only
// that Handle, not the Group's list, and warns that it no longer follows
// the Group's; exclude --key refuses and writes nothing.
func TestAgentIncludeExcludeOnAConversationFollowingTheGroup(t *testing.T) {
	p := agentsWithKeys(t)
	path := p.ConversationConfigFile(group.ImplicitAlias, "eddsa-ab2222222222")
	writeFile(t, path, "[agent]\nmax_depth = 3\n")

	r := run(t, nil, "", "--dir", p.Config, "agent", "exclude", "auditor", "--key", "ab22")
	if r.code != 1 || !strings.Contains(r.stderr, "ab22 follows the Group's list; exclude it there, or give ab22 its own list with include --key") {
		t.Fatalf("exclude --key on a list-less Conversation: exit %d, stderr %q", r.code, r.stderr)
	}
	if got := readFile(t, path); got != "[agent]\nmax_depth = 3\n" {
		t.Fatalf("a refused exclude wrote %q", got)
	}

	r = run(t, nil, "", "--dir", p.Config, "agent", "include", "scribe", "--key", "ab22")
	if r.code != 0 || r.stdout != "scribe\n" || !strings.Contains(r.stderr, "no longer follows the Group's") {
		t.Fatalf("include --key on a list-less Conversation: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	w, err := config.ReadWorkspace(path)
	if err != nil || !reflect.DeepEqual(w.Agent, config.Agent{Profiles: []string{"scribe"}, MaxDepth: 3}) {
		t.Fatalf("Conversation [agent] = %#v, %v", w.Agent, err)
	}

	fresh := p.ConversationConfigFile(group.ImplicitAlias, "ecdsa-ab1111111111")
	if err := os.Remove(fresh); err != nil {
		t.Fatal(err)
	}
	if r := run(t, nil, "", "--dir", p.Config, "agent", "include", "auditor", "--key", "ab11"); r.code != 0 || r.stdout != "auditor\n" {
		t.Fatalf("include --key with no config.toml: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if w, err := config.ReadWorkspace(fresh); err != nil || !reflect.DeepEqual(w.Agent.Profiles, []string{"auditor"}) {
		t.Fatalf("new Conversation config: %#v, %v", w, err)
	}
	if r := run(t, nil, "", "--dir", p.Config, "agent", "include", "ghost", "--key", "ab11"); r.code != 1 || !strings.Contains(r.stderr, "ghost") {
		t.Fatalf("include ghost --key: exit %d, stderr %q", r.code, r.stderr)
	}
}

// agent include and exclude --key edit a Conversation's list without
// merging its config.toml over the Group's, so a Conversation whose
// Networks clash with the Group's can still have its list fixed.
func TestAgentIncludeExcludeKeyDoNotMergeTheConfig(t *testing.T) {
	p := agentsWithKeys(t)
	gf := p.GroupConfigFile(group.ImplicitAlias)
	writeFile(t, gf, readFile(t, gf)+"\n[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\n")
	path := p.ConversationConfigFile(group.ImplicitAlias, "ecdsa-ab1111111111")
	writeFile(t, path, "[networks.mainnet]\ntype = \"ethereum\"\nchain_id = 1\n\n[agent]\nprofiles = [\"scribe\"]\n")

	if r := run(t, nil, "", "--dir", p.Config, "agent", "include", "auditor", "--key", "ab11"); r.code != 0 || r.stdout != "scribe\nauditor\n" {
		t.Fatalf("include --key: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := run(t, nil, "", "--dir", p.Config, "agent", "exclude", "scribe", "--key", "ab11"); r.code != 0 || r.stdout != "auditor\n" {
		t.Fatalf("exclude --key: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}
