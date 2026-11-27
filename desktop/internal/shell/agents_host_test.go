package shell_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
	"defios.com/internal/secret"
)

// postingAPI is an Anthropic Messages API whose agent says "checked" once
// a chain reaches it, then ends its turn. It keeps the system prompt of
// every call.
type postingAPI struct {
	mu      sync.Mutex
	systems []string
}

func (a *postingAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		System   any               `json:"system"`
		Messages []json.RawMessage `json:"messages"`
	}
	json.Unmarshal(raw, &req)
	sys, _ := json.Marshal(req.System)
	a.mu.Lock()
	a.systems = append(a.systems, string(sys))
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	// A run's own tool result is answered by ending its turn. Earlier runs
	// replayed before its turn hold tool results too (ADR 0036): only the
	// last message says where this call is.
	if n := len(req.Messages); n > 0 && !strings.Contains(string(req.Messages[n-1]), `"type":"text"`) {
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`)
		return
	}
	io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[`+
		`{"type":"tool_use","id":"toolu_1","name":"talk_write","input":{"text":"checked"}}],`+
		`"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":3}}`)
}

// lastSystem is the system prompt of the last call.
func (a *postingAPI) lastSystem() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.systems) == 0 {
		return ""
	}
	return a.systems[len(a.systems)-1]
}

func wroteBy(handle string, n int) func(shell.TalkState) bool {
	return func(ts shell.TalkState) bool {
		wrote := 0
		for _, it := range ts.Items {
			if it.Handle == handle && it.Text == "checked" {
				wrote++
			}
		}
		return wrote >= n
	}
}

func agentsOK(t *testing.T, what string, v shell.AgentsView) shell.AgentsView {
	t.Helper()
	if v.Problem != nil || !v.Saved {
		t.Fatalf("%s: %+v (field %q)", what, v.Problem, v.Field)
	}
	return v
}

// A Provider and an Agent Profile made in the app answer in Talk, and a
// changed BRIEF.md is the next chain's system prompt.
func TestAProfileMadeInTheAppAnswersInTalkWithItsBrief(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group and runs an agent")
	}
	api := &postingAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	alice, _ := formed(t)

	if sv := alice.NewSecret("ANTHROPIC_KEY", "sk-test"); sv.Problem != nil || !slices.Contains(sv.Secrets, shell.SecretName{Name: "ANTHROPIC_KEY", Source: secret.SourceVault}) {
		t.Fatalf("new Secret: %+v", sv)
	}
	v := agentsOK(t, "add provider", alice.SaveProvider(shell.ProviderDraft{
		New: true, Alias: "claude", BaseURL: srv.URL, APIPath: "/v1/messages", APIKey: "ANTHROPIC_KEY",
	}))
	if len(v.Providers) != 1 || v.Providers[0].Alias != "claude" || v.Providers[0].APIKey != "ANTHROPIC_KEY" {
		t.Fatalf("providers: %+v", v.Providers)
	}
	ready := func(ts shell.TalkState) bool { return ts.MajorityReachable && len(ts.Conversations) == 1 }
	talkWithin(t, alice, 20*time.Second, ready)
	v = agentsOK(t, "add profile", alice.SaveAgentProfile(shell.ProfileDraft{
		New: true, Handle: "auditor", Provider: "claude", Model: "claude-test",
		Body: `{"max_tokens": 1024}`, MaxTurns: "4", Brief: "You audit, v1.",
	}))
	if o := v.Open; o == nil || o.Handle != "auditor" || o.Model != "claude-test" || o.MaxTurns != "4" ||
		o.Brief != "You audit, v1." || o.Body != "{\n  \"max_tokens\": 1024\n}" {
		t.Fatalf("open: %+v", v.Open)
	}
	// Talk, already open, lists it to include.
	if agents := alice.Talk().Agents; !slices.ContainsFunc(agents, func(a shell.Agent) bool { return a.Handle == "auditor" && !a.Included }) {
		t.Fatalf("Talk's agents after adding one: %+v", agents)
	}

	if ts := alice.SetAgentIncluded("auditor", true); ts.Problem != nil {
		t.Fatalf("include: %+v", ts.Problem)
	}
	if ts := alice.SendMessage("@auditor look"); ts.Refused != nil || ts.Problem != nil {
		t.Fatalf("send: %+v %+v", ts.Refused, ts.Problem)
	}
	talkWithin(t, alice, 20*time.Second, wroteBy("auditor", 1))
	if !strings.Contains(api.lastSystem(), "You audit, v1.") {
		t.Fatalf("system prompt %s", api.lastSystem())
	}

	d := draftOf(*alice.Agents("auditor").Open)
	d.Brief = "You audit, v2."
	if v := agentsOK(t, "edit the Brief", alice.SaveAgentProfile(d)); v.Open.Brief != "You audit, v2." {
		t.Fatalf("Brief after save: %q", v.Open.Brief)
	}
	alice.SendMessage("@auditor again")
	talkWithin(t, alice, 20*time.Second, wroteBy("auditor", 2))
	if s := api.lastSystem(); !strings.Contains(s, "You audit, v2.") || strings.Contains(s, "v1") {
		t.Fatalf("system prompt after the edit %s", s)
	}
	// Included in Group talk now, so Delete warns where and deletes nothing.
	v = alice.DeleteAgentProfile("auditor")
	if v.Problem == nil || v.Problem.Code != "profile-included" || !strings.Contains(v.Problem.Message, "home") || v.Open == nil {
		t.Fatalf("delete while included: %+v %+v", v.Problem, v.Open)
	}
	if places := v.Open.IncludedIn; len(places) != 1 || places[0].Group != "home" || places[0].Where != "Group talk" {
		t.Fatalf("included in %+v", places)
	}
}

func draftOf(o shell.ProfileView) shell.ProfileDraft {
	return shell.ProfileDraft{Handle: o.Handle, Provider: o.Provider, Model: o.Model, Body: o.Body, MaxTurns: o.MaxTurns, Brief: o.Brief, Livery: o.Livery}
}

// Grants round-trip as the CLI's agent verbs see them; Providers edit a
// Group not served; and an invalid edit writes nothing and names its
// field.
func TestGrantsProvidersAndRefusedEditsOnTheAgentsPage(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _, ap, _ := formedAt(t)
	plugin := filepath.Join(ap.PluginsDir(), "audit")
	if err := os.MkdirAll(plugin, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "name = \"audit\"\nversion = \"1.0.0\"\ndescription = \"Audits.\"\n\n[mcp.scanner]\ncommand = \"node\"\nenv = [\"ETHERSCAN_KEY\", \"OTHER\"]\n"
	if err := os.WriteFile(filepath.Join(plugin, "plugin.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	agentsOK(t, "add provider", alice.SaveProvider(shell.ProviderDraft{New: true, Alias: "anthropic", Preset: "anthropic"}))
	v := agentsOK(t, "add profile", alice.SaveAgentProfile(shell.ProfileDraft{New: true, Handle: "auditor", Provider: "anthropic"}))
	if o := v.Open; o.Brief == "" || o.Body != "" || o.MaxTurns != "" || !slices.Equal(o.Tools, []string{"talk_write", "talk_read"}) {
		t.Fatalf("a new profile: %+v", o)
	}
	if !slices.Contains(v.Catalog.Tools, "sign_list") || !slices.ContainsFunc(v.Catalog.Servers, func(s shell.CatalogServer) bool {
		return s.Ref == "audit/scanner" && slices.Equal(s.Env, []string{"ETHERSCAN_KEY", "OTHER"})
	}) {
		t.Fatalf("catalog: %+v", v.Catalog)
	}
	// A server asking for nothing gives Env [], not null, which the page
	// would fail to draw.
	if !slices.ContainsFunc(v.Catalog.Servers, func(s shell.CatalogServer) bool { return s.Ref == "bitcoin/tx" && s.Env != nil }) {
		t.Fatalf("catalog servers: %+v", v.Catalog.Servers)
	}
	// So is one granted with no Secrets.
	v = agentsOK(t, "grant tx", alice.GrantAgent("auditor", "bitcoin/tx", nil))
	if g, ok := v.Open.MCP["bitcoin/tx"]; !ok || g == nil {
		t.Fatalf("granted bitcoin/tx: %+v", v.Open.MCP)
	}
	agentsOK(t, "revoke tx", alice.RevokeAgent("auditor", "bitcoin/tx"))

	// An MCP Server with a Secret, granted then revoked.
	if sv := alice.NewSecret("ETHERSCAN_KEY", "ek"); sv.Problem != nil {
		t.Fatalf("new Secret: %+v", sv.Problem)
	}
	v = agentsOK(t, "grant", alice.GrantAgent("auditor", "audit/scanner", []string{"ETHERSCAN_KEY"}))
	if got := v.Open.MCP["audit/scanner"]; !slices.Equal(got, []string{"ETHERSCAN_KEY"}) {
		t.Fatalf("granted %+v", v.Open.MCP)
	}
	cli := defiosCLI(t, ap.Config, "agent", "show", "auditor")
	if !strings.Contains(cli, "audit/scanner") || !strings.Contains(cli, "ETHERSCAN_KEY") {
		t.Fatalf("the CLI after the grant:\n%s", cli)
	}
	agentsOK(t, "grant a tool", alice.GrantAgent("auditor", "sign_list", nil))
	v = alice.GrantAgent("auditor", "audit/scanner", []string{"NOT_ASKED"})
	if v.Problem == nil || v.Problem.Code != "grant-refused" || v.Where != "grant:audit/scanner" {
		t.Fatalf("a Secret it does not ask for: %+v %q", v.Problem, v.Where)
	}
	v = agentsOK(t, "revoke", alice.RevokeAgent("auditor", "audit/scanner"))
	if len(v.Open.MCP) != 0 || !slices.Contains(v.Open.Tools, "sign_list") {
		t.Fatalf("after revoke: %+v", v.Open)
	}
	if cli := defiosCLI(t, ap.Config, "agent", "show", "auditor"); strings.Contains(cli, "audit/scanner") {
		t.Fatalf("the CLI after the revoke:\n%s", cli)
	}

	// An invalid edit writes nothing and says which field.
	before := *alice.Agents("auditor").Open
	for _, c := range []struct {
		name  string
		edit  func(*shell.ProfileDraft)
		field string
	}{
		{"max turns", func(d *shell.ProfileDraft) { d.MaxTurns = "0" }, node.FieldMaxTurns},
		{"max turns not a number", func(d *shell.ProfileDraft) { d.MaxTurns = "lots" }, node.FieldMaxTurns},
		{"body", func(d *shell.ProfileDraft) { d.Body = "{nope" }, shell.FieldBody},
		{"provider", func(d *shell.ProfileDraft) { d.Provider = "" }, node.FieldProvider},
	} {
		d := draftOf(before)
		d.Brief, d.Model = "changed", "changed"
		c.edit(&d)
		v := alice.SaveAgentProfile(d)
		if v.Saved || v.Problem == nil || v.Field != c.field || v.Where != "profile" {
			t.Errorf("%s: %+v field %q where %q", c.name, v.Problem, v.Field, v.Where)
		}
		if after := *alice.Agents("auditor").Open; !equalProfiles(after, before) {
			t.Errorf("%s: a refused save wrote %+v", c.name, after)
		}
	}
	if v := alice.SaveAgentProfile(shell.ProfileDraft{New: true, Handle: "auditor", Provider: "anthropic"}); v.Field != node.FieldHandle || v.Problem.Code != "profile-exists" {
		t.Fatalf("adding it again: %+v %q", v.Problem, v.Field)
	}

	// Providers of a Group not served: an edit applies when it is.
	if st := alice.AddGroup("work", "https://hs.example", "tskey-auth-work", ""); st.Problem != nil {
		t.Fatalf("add group: %+v", st.Problem)
	}
	if st := alice.Switch("home"); st.View != shell.ViewServed || st.Served != "home" {
		t.Fatalf("back to home: %+v", st)
	}
	alice.SetEditGroup("work")
	v = alice.SaveProvider(shell.ProviderDraft{New: true, Alias: "local", BaseURL: "http://127.0.0.1:8080", APIPath: "/v1/nope"})
	if v.Problem == nil || v.Field != node.FieldAPIPath || v.Where != "new-provider" || len(v.Providers) != 0 {
		t.Fatalf("a bad API path: %+v %q %+v", v.Problem, v.Field, v.Providers)
	}
	v = agentsOK(t, "add local", alice.SaveProvider(shell.ProviderDraft{New: true, Alias: "local", BaseURL: "http://127.0.0.1:8080", APIPath: "/v1/chat/completions", Body: `{"stream": true}`}))
	if v.Group != "work" || v.Served || len(v.Providers) != 1 || v.Providers[0].Body != "{\n  \"stream\": true\n}" {
		t.Fatalf("work's: %+v %+v", v, v.Providers)
	}
	v = alice.SaveProvider(shell.ProviderDraft{Alias: "local", BaseURL: "http://127.0.0.1:8080", APIPath: "/v1/chat/completions", APIKey: "bad name"})
	if v.Field != node.FieldAPIKey || v.Where != "provider:local" || v.Providers[0].APIKey != "" {
		t.Fatalf("a bad key: %+v %q %+v", v.Problem, v.Field, v.Providers)
	}
	agentsOK(t, "remove", alice.RemoveProvider("local"))
	if v := alice.Agents(""); len(v.Providers) != 0 {
		t.Fatalf("after remove: %+v", v.Providers)
	}
	alice.SetEditGroup("home")
	if v := alice.Agents(""); len(v.Providers) != 1 || v.Providers[0].Alias != "anthropic" {
		t.Fatalf("home's untouched: %+v", v.Providers)
	}

	// Not included anywhere, it deletes.
	v = agentsOK(t, "delete", alice.DeleteAgentProfile("auditor"))
	if v.Open != nil || len(v.Profiles) != 0 {
		t.Fatalf("after delete: %+v", v)
	}
}

func equalProfiles(a, b shell.ProfileView) bool {
	return a.Provider == b.Provider && a.Model == b.Model && a.Body == b.Body && a.MaxTurns == b.MaxTurns && a.Brief == b.Brief
}

// A profile's Livery is picked on the Agents page as agent edit --livery
// sets it: one another party holds is refused and names its field, none
// clears it, and Talk shows a change at once.
func TestAProfilesLiveryIsPickedOnTheAgentsPage(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _, ap, _ := formedAt(t)
	agentsOK(t, "add provider", alice.SaveProvider(shell.ProviderDraft{New: true, Alias: "anthropic", Preset: "anthropic"}))
	talkWithin(t, alice, 20*time.Second, func(ts shell.TalkState) bool { return len(ts.Conversations) == 1 })
	v := alice.Agents("")
	if len(v.FreeLiveries) != 63 || slices.Contains(v.FreeLiveries, v.Liveries[node.NodeParty(bobID(t, alice))]) {
		t.Fatalf("free %d of 64, held %+v", len(v.FreeLiveries), v.Liveries)
	}
	pick, other := v.FreeLiveries[0], v.FreeLiveries[1]

	v = agentsOK(t, "add with a Livery", alice.SaveAgentProfile(shell.ProfileDraft{New: true, Handle: "auditor", Provider: "anthropic", Livery: pick}))
	if v.Open.Livery != pick || slices.Contains(v.FreeLiveries, pick) || v.Liveries[node.AgentParty("auditor")] != pick {
		t.Fatalf("added: %q, free has it: %v", v.Open.Livery, slices.Contains(v.FreeLiveries, pick))
	}
	if cli := defiosCLI(t, ap.Config, "agent", "show", "auditor"); !strings.Contains(cli, pick) {
		t.Fatalf("the CLI after the add:\n%s", cli)
	}
	v = alice.SaveAgentProfile(shell.ProfileDraft{New: true, Handle: "helper", Provider: "anthropic", Livery: pick})
	if v.Problem == nil || v.Field != node.FieldLivery || len(v.Profiles) != 1 {
		t.Fatalf("another with the same Livery: %+v %q %+v", v.Problem, v.Field, v.Profiles)
	}
	v = agentsOK(t, "add with none", alice.SaveAgentProfile(shell.ProfileDraft{New: true, Handle: "helper", Provider: "anthropic"}))
	if v.Open.Livery != "" {
		t.Fatalf("added with none: %q", v.Open.Livery)
	}

	d := draftOf(*alice.Agents("auditor").Open)
	d.Livery = other
	if v := agentsOK(t, "change it", alice.SaveAgentProfile(d)); v.Open.Livery != other {
		t.Fatalf("changed: %q", v.Open.Livery)
	}
	if a := alice.Talk().Agents; !slices.Contains(a, shell.Agent{Handle: "auditor", Party: node.AgentParty("auditor"), Livery: other}) {
		t.Fatalf("Talk at once: %+v", a)
	}
	d.Livery, d.Brief = "", "a new Brief"
	if v := agentsOK(t, "clear it", alice.SaveAgentProfile(d)); v.Open.Livery != "" {
		t.Fatalf("cleared: %q", v.Open.Livery)
	}
}

// bobID is the other device's Node ID on s's Roster.
func bobID(t *testing.T, s *shell.Shell) string {
	t.Helper()
	ts := talkWithin(t, s, 20*time.Second, func(ts shell.TalkState) bool { return len(ts.Roster) == 2 })
	for _, r := range ts.Roster {
		if !r.Self {
			return r.ID
		}
	}
	t.Fatal("no other device")
	return ""
}

// The Agents page lists a Provider's models with its key from the Vault
// the app holds open; one that refuses the list is a Problem, and so is
// an alias the Group has no Provider by.
func TestTheAgentsPageListsAProvidersModels(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-vault" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}],"has_more":false}`)
	}))
	defer srv.Close()
	alice, _, _, _ := formedAt(t)
	agentsOK(t, "add provider", alice.SaveProvider(shell.ProviderDraft{New: true, Alias: "claude", Preset: "anthropic", BaseURL: srv.URL, APIKey: "CLAUDE_KEY"}))

	if m := alice.ProviderModels("claude"); m.Problem == nil || m.Problem.Code != "no-key" || m.Models == nil {
		t.Fatalf("with no Secret set: %+v", m)
	}
	if sv := alice.NewSecret("CLAUDE_KEY", "sk-vault"); sv.Problem != nil {
		t.Fatalf("new Secret: %+v", sv.Problem)
	}
	m := alice.ProviderModels("claude")
	if want := []shell.ModelOption{{ID: "claude-opus-5-5", Name: "Claude Opus 5.5"}}; m.Problem != nil || !slices.Equal(m.Models, want) {
		t.Fatalf("models: %+v; want %+v", m, want)
	}
	if sv := alice.NewSecret("CLAUDE_KEY", "sk-wrong"); sv.Problem != nil {
		t.Fatalf("replace Secret: %+v", sv.Problem)
	}
	if m := alice.ProviderModels("claude"); m.Problem == nil || m.Problem.Code != "models-unlisted" || !strings.Contains(m.Problem.Detail, "401") {
		t.Fatalf("a refused list: %+v", m.Problem)
	}
	if m := alice.ProviderModels("ghost"); m.Problem == nil || m.Problem.Code != "unknown-provider" {
		t.Fatalf("an unknown alias: %+v", m.Problem)
	}
}
