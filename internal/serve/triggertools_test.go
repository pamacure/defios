package serve_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/group"
)

// triggerTools grants every Trigger-config Tool.
const triggerTools = `["talk_write", "job_list", "job_add", "job_edit", "job_delete", "hook_list", "hook_add", "hook_edit", "hook_delete", "schedule_list", "schedule_add", "schedule_edit", "schedule_delete"]`

// builderPair is agentPair with alice's @builder, in Group-level talk,
// granted the Trigger-config Tools, and a Key minted whose alias is
// treasury, whose Conversation includes @auditor; it returns that Key's
// Workspace name.
func builderPair(t *testing.T) (a *node, provider *fakeProvider, workspace string) {
	t.Helper()
	a, b, gid, provider := agentPair(t, "builder")
	writeConfig(t, a.paths.AgentProfileFile("builder"), "provider = \"claude\"\nmax_turns = 40\ntools = "+triggerTools+"\n\n[body]\nmodel = \"claude-test\"\nstream = true\nmax_tokens = 1024\n")
	workspace = mintKeyOf(t, a, b, gid, 0x33)
	waitWorkspace(t, a, workspace)
	includeIn(t, a, workspace, "auditor")
	if err := config.WriteKeyAlias(a.paths.ConversationConfigFile(group.ImplicitAlias, workspace), "treasury"); err != nil {
		t.Fatal(err)
	}
	return a, provider, workspace
}

// runCalls summons @builder in a's Group-level talk to make calls, a Tool
// and its input each, and returns each call's result: the last turn of
// the Provider call after it.
func runCalls(t *testing.T, a *node, provider *fakeProvider, calls [][2]string) []string {
	t.Helper()
	before := len(provider.seen())
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if i := n - before - 1; i < len(calls) {
			endCall(w, calls[i][0], calls[i][1])
			return
		}
		endTalkWrite(w, "done")
	})
	if _, got := summonIn(t, a, nil, "@builder set up the triggers"); !strings.HasSuffix(got, "commit:done end") {
		t.Fatalf("events = %s", got)
	}
	seen := provider.seen()[before:]
	if len(seen) != len(calls)+1 {
		t.Fatalf("%d Provider calls, want %d", len(seen), len(calls)+1)
	}
	out := make([]string, len(calls))
	for i := range calls {
		out[i] = lastTurn(seen[i+1])
	}
	return out
}

// An agent adds, edits, lists and deletes Jobs, Hooks and Schedules by
// their config.toml keys (ADR 0046): a Key named by its alias is written
// by its Workspace name, by is the agent's Handle, and lists give by, the
// alias of each Workspace name, and a Schedule's next firing.
func TestAnAgentWritesTriggerConfig(t *testing.T) {
	a, provider, ws := builderPair(t)
	got := runCalls(t, a, provider, [][2]string{
		{"job_add", `{"name":"notify","job":{"webhook":"https://ntfy.example/x","timeout":"5s"}}`},
		{"job_add", `{"name":"summarise","job":{"agent":"builder","prompt":"Summarise.","target":"group"}}`},
		{"job_edit", `{"name":"notify","job":{"exec":["true"],"timeout":"9s"}}`},
		{"hook_add", `{"name":"on_request","hook":{"events":["sign.request"],"conversation":"treasury","jobs":["notify"]}}`},
		{"hook_edit", `{"name":"on_request","hook":{"conversation":["treasury","group"],"jobs":["notify","summarise"]}}`},
		{"schedule_add", `{"name":"daily","schedule":{"cron":"0 9 * * 1-5","jobs":["summarise"]}}`},
		{"schedule_edit", `{"name":"daily","schedule":{"cron":"30 9 * * *","tz":"Europe/London","jobs":["summarise"]}}`},
		{"job_list", `{}`},
		{"hook_list", `{}`},
		{"schedule_list", `{}`},
		{"schedule_delete", `{"name":"daily"}`},
		{"hook_delete", `{"name":"on_request"}`},
		{"job_delete", `{"name":"notify"}`},
		{"job_list", `{}`},
		{"job_add", `{"name":"review","job":{"agent":"auditor","prompt":"Review it.","target":"treasury"}}`},
	})
	for i, r := range got {
		if strings.Contains(r, `"is_error":true`) {
			t.Errorf("call %d refused: %s", i, r)
		}
	}
	for _, want := range []string{`\"name\":\"notify\",\"job\":{\"exec\":[\"true\"],\"timeout\":\"9s\"},\"by\":\"builder\"`, `\"target\":\"group\"`} {
		if !strings.Contains(got[7], want) {
			t.Errorf("job_list lacks %s: %s", want, got[7])
		}
	}
	if want := fmt.Sprintf(`\"conversation\":[\"%s\",\"group\"],\"jobs\":[\"notify\",\"summarise\"]},\"by\":\"builder\",\"aliases\":{\"%s\":\"treasury\"}`, ws, ws); !strings.Contains(got[8], want) {
		t.Errorf("hook_list lacks %s: %s", want, got[8])
	}
	for _, want := range []string{`\"cron\":\"30 9 * * *\"`, `\"tz\":\"Europe/London\"`, `\"by\":\"builder\"`, `\"next\":\"`} {
		if !strings.Contains(got[9], want) {
			t.Errorf("schedule_list lacks %s: %s", want, got[9])
		}
	}
	if strings.Contains(got[13], "notify") || !strings.Contains(got[13], "summarise") {
		t.Errorf("job_list after delete: %s", got[13])
	}
	c, err := config.ReadFile(a.paths.GroupConfigFile(group.ImplicitAlias))
	if err != nil {
		t.Fatal(err)
	}
	if j := c.Jobs["summarise"]; j.By != "builder" || j.Target != "group" || len(c.Hooks)+len(c.Schedules) != 0 {
		t.Errorf("config.toml holds %+v, hooks %v, schedules %v", j, c.Hooks, c.Schedules)
	}
	if j := c.Jobs["review"]; j.Target != ws || j.By != "builder" {
		t.Errorf("a Job targeting the alias treasury is written as %+v; want target %s", j, ws)
	}
}

// Each Trigger-config Tool refuses, saying why, what it may not write: a
// by given, an unknown key, a name defined already or not, a Key no
// alias or Workspace name names, an agent not included where its Job
// answers, a Job a Trigger runs, and what the config loader would refuse.
// Nothing refused is written.
func TestTriggerConfigToolsRefuse(t *testing.T) {
	a, provider, _ := builderPair(t)
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	before := readFile(t, path)
	cases := []struct{ tool, input, want string }{
		{"job_list", `{"all":true}`, "its input must be {}"},
		{"hook_list", `{"all":true}`, "its input must be {}"},
		{"schedule_list", `{"all":true}`, "its input must be {}"},
		{"job_add", `{"name":"j","job":{"exec":["true"],"by":"auditor"}}`, "by is set to your Handle"},
		{"job_add", `{"name":"j","job":{"exec":["true"],"webhook":"https://x.example"}}`, "exactly one of agent, exec or webhook"},
		{"job_add", `{"name":"j","job":{"agent":"builder","prompt":"x","target":"vault"}}`, `\"vault\" is neither \"group\", a Key alias nor the Workspace name`},
		{"job_add", `{"name":"j","job":{"agent":"auditor","prompt":"x","target":"group"}}`, "@auditor is not included in group"},
		{"job_delete", `{"name":"sign_accept"}`, "Job sign_accept is run by Hook accept_all"},
		{"job_add", `{"name":"sign_accept","job":{"exec":["true"]}}`, "already defined in this Group's config.toml"},
		{"job_edit", `{"name":"missing","job":{"exec":["true"]}}`, "not defined in this Group's config.toml"},
		{"job_delete", `{"name":"missing"}`, "not defined in this Group's config.toml"},
		{"hook_add", `{"name":"h","hook":{"jobs":["sign_accept"],"when":"now"}}`, "unknown key when"},
		{"hook_edit", `{"name":"missing","hook":{"jobs":["sign_accept"]}}`, "not defined in this Group's config.toml"},
		{"hook_delete", `{"name":"missing"}`, "not defined in this Group's config.toml"},
		{"schedule_add", `{"name":"s","schedule":{"cron":"0 9 * * *","jobs":["missing"]}}`, "jobs: no [jobs.missing]"},
		{"schedule_edit", `{"name":"missing","schedule":{"at":"2030-01-01T09:00:00Z","jobs":["sign_accept"]}}`, "not defined in this Group's config.toml"},
		{"schedule_delete", `{"name":"missing"}`, "not defined in this Group's config.toml"},
	}
	// The Group's config.toml holds sign_accept, as a new Group's does, and
	// a Hook that runs it.
	writeConfig(t, path, before+"\n[jobs.sign_accept]\nexec = [\"true\"]\n\n[hooks.accept_all]\njobs = [\"sign_accept\"]\n")
	before = readFile(t, path)
	calls := make([][2]string, len(cases))
	for i, c := range cases {
		calls[i] = [2]string{c.tool, c.input}
	}
	for i, r := range runCalls(t, a, provider, calls) {
		if !strings.Contains(r, `"is_error":true`) || !strings.Contains(r, cases[i].want) {
			t.Errorf("%s %s answered %s; want a refusal saying %s", cases[i].tool, cases[i].input, r, cases[i].want)
		}
	}
	if after := readFile(t, path); after != before {
		t.Errorf("a refused write changed config.toml:\n%s", after)
	}
}
