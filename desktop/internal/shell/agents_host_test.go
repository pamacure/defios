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

// sayingAPI is an Anthropic Messages API whose agent says "checked" once
// a chain reaches it, then ends its turn. It keeps the system prompt of
// every call.
type sayingAPI struct {
	mu      sync.Mutex
	systems []string
}

func (a *sayingAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if strings.Contains(string(raw), `"tool_result"`) {
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`)
		return
	}
	io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[`+
		`{"type":"tool_use","id":"toolu_1","name":"say","input":{"text":"checked"}}],`+
		`"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":3}}`)
}

// lastSystem is the system prompt of the last call.
func (a *sayingAPI) lastSystem() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.systems) == 0 {
		return ""
	}
	return a.systems[len(a.systems)-1]
}

func saidBy(handle string, n int) func(shell.TalkState) bool {
	return func(ts shell.TalkState) bool {
		said := 0
		for _, it := range ts.Items {
			if it.Handle == handle && it.Text == "checked" {
				said++
			}
		}
		return said >= n
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
// changed SYSTEM.md is the next chain's system prompt.
func TestAProfileMadeInTheAppAnswersInTalkWithItsSystemPrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group and runs an agent")
	}
	api := &sayingAPI{}
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
	v = agentsOK(t, "add profile", alice.SaveAgentProfile(shell.ProfileDraft{
		New: true, Handle: "auditor", Provider: "claude", Model: "claude-test",
		Body: `{"max_tokens": 1024}`, MaxTurns: "4", System: "You audit, v1.",
	}))
	if o := v.Open; o == nil || o.Handle != "auditor" || o.Model != "claude-test" || o.MaxTurns != "4" ||
		o.System != "You audit, v1." || o.Body != "{\n  \"max_tokens\": 1024\n}" {
		t.Fatalf("open: %+v", v.Open)
	}

	ready := func(ts shell.TalkState) bool { return ts.MajorityReachable && len(ts.Conversations) == 1 }
	talkWithin(t, alice, 20*time.Second, ready)
	if ts := alice.SetAgentIncluded("auditor", true); ts.Problem != nil {
		t.Fatalf("include: %+v", ts.Problem)
	}
	if ts := alice.SendMessage("@auditor look"); ts.Refused != nil || ts.Problem != nil {
		t.Fatalf("send: %+v %+v", ts.Refused, ts.Problem)
	}
	talkWithin(t, alice, 20*time.Second, saidBy("auditor", 1))
	if !strings.Contains(api.lastSystem(), "You audit, v1.") {
		t.Fatalf("system prompt %s", api.lastSystem())
	}

	d := draftOf(*alice.Agents("auditor").Open)
	d.System = "You audit, v2."
	if v := agentsOK(t, "edit SYSTEM.md", alice.SaveAgentProfile(d)); v.Open.System != "You audit, v2." {
		t.Fatalf("system after save: %q", v.Open.System)
	}
	alice.SendMessage("@auditor again")
	talkWithin(t, alice, 20*time.Second, saidBy("auditor", 2))
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
	return shell.ProfileDraft{Handle: o.Handle, Provider: o.Provider, Model: o.Model, Body: o.Body, MaxTurns: o.MaxTurns, System: o.System}
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
	if o := v.Open; o.System == "" || o.Body != "" || o.MaxTurns != "" || !slices.Equal(o.Tools, []string{"say"}) {
		t.Fatalf("a new profile: %+v", o)
	}
	if !slices.Contains(v.Catalog.Tools, "sign_list") || !slices.ContainsFunc(v.Catalog.Servers, func(s shell.CatalogServer) bool {
		return s.Ref == "audit/scanner" && slices.Equal(s.Env, []string{"ETHERSCAN_KEY", "OTHER"})
	}) {
		t.Fatalf("catalog: %+v", v.Catalog)
	}

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
		d.System, d.Model = "changed", "changed"
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
	return a.Provider == b.Provider && a.Model == b.Model && a.Body == b.Body && a.MaxTurns == b.MaxTurns && a.System == b.System
}
