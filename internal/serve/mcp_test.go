package serve_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"defios.com/internal/serve"
)

// fixture is the test MCP Server, testdata/mcpfixture, built once.
var fixture struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fixture.dir != "" {
		os.RemoveAll(fixture.dir)
	}
	os.Exit(code)
}

// fixturePath builds the fixture MCP Server, the first time it is asked for.
func fixturePath(t *testing.T) string {
	t.Helper()
	fixture.once.Do(func() {
		if fixture.dir, fixture.err = os.MkdirTemp("", "mcpfixture"); fixture.err != nil {
			return
		}
		fixture.path = filepath.Join(fixture.dir, "mcpfixture")
		out, err := exec.Command("go", "build", "-o", fixture.path, "./testdata/mcpfixture").CombinedOutput()
		if err != nil {
			fixture.err = fmt.Errorf("building the fixture: %v\n%s", err, out)
		}
	})
	if fixture.err != nil {
		t.Fatal(fixture.err)
	}
	return fixture.path
}

// installTools installs, as plugin install leaves it, a Plugin tools on n
// whose MCP Server fx runs the fixture with args, asking for env. It returns
// the Plugin's directory, where the fixture logs each start.
func installTools(t *testing.T, n *node, args, env string) string {
	t.Helper()
	dir := filepath.Join(n.paths.PluginsDir(), "tools")
	writeConfig(t, filepath.Join(dir, "plugin.toml"), fmt.Sprintf("name = \"tools\"\nversion = \"1.0.0\"\ndescription = \"Test tools\"\n\n[mcp.fx]\ncommand = %q\nargs = %s\nenv = %s\n",
		fixturePath(t), args, env))
	return dir
}

// grantMCP sets n's profile h, as agentNode writes it, to grant the MCP
// Servers tables, TOML [mcp."<plugin>/<server>"] tables.
func grantMCP(t *testing.T, n *node, h, tables string) {
	t.Helper()
	writeConfig(t, n.paths.AgentProfileFile(h), "provider = \"claude\"\n\n[body]\nmodel = \"claude-test\"\nstream = true\nmax_tokens = 1024\n\n"+tables)
}

// starts are the pids the fixture logged starting in dir.
func starts(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "starts.log"))
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

// chainOf sends text on a and waits for the chain it starts to end,
// returning its events.
func chainOf(t *testing.T, a *node, text string) string {
	t.Helper()
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), text, nil)
	if err != nil {
		t.Fatal(err)
	}
	return summary(untilEnd(t, s, v.MsgID))
}

// An agent granted an MCP Server is offered its tools as
// <plugin>__<server>__<tool>, calls one and gets its text result; the
// server runs in its Plugin's directory, and an MCP error is a tool error
// (ADR 0030).
func TestAnAgentCallsAGrantedMCPServersTool(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	dir := installTools(t, a, "[]", "[]")
	grantMCP(t, a, "auditor", "[mcp.\"tools/fx\"]\n")
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		switch n {
		case 1:
			endCall(w, "tools__fx__echo", `{"text":"hi"}`)
		case 2:
			endCall(w, "tools__fx__cwd", `{}`)
		case 3:
			endCall(w, "tools__fx__fail", `{}`)
		default:
			endSay(w, "done")
		}
	})
	if got := chainOf(t, a, "@auditor go"); !strings.HasSuffix(got, "commit:done end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if len(calls) != 4 {
		t.Fatalf("%d calls, want 4", len(calls))
	}
	want := "say tools__fx__crash tools__fx__cwd tools__fx__echo tools__fx__env tools__fx__fail tools__fx__pid tools__fx__sleep"
	if calls[0].Tools != want {
		t.Errorf("tools offered %q, want %q", calls[0].Tools, want)
	}
	if got := lastTurn(calls[1]); !strings.Contains(got, "echo: hi") || strings.Contains(got, `"is_error":true`) {
		t.Errorf("echo answered %s", got)
	}
	if got := lastTurn(calls[2]); !strings.Contains(got, dir) {
		t.Errorf("cwd answered %s, want %s", got, dir)
	}
	if got := lastTurn(calls[3]); !strings.Contains(got, `"is_error":true`) || !strings.Contains(got, "it failed on purpose") {
		t.Errorf("fail answered %s", got)
	}
}

// An MCP Server's process sees PATH, HOME and the Secrets its grant names,
// from the serve's environment or else the Vault, and nothing else of the
// serve's environment: not a Secret its server asks for that the grant
// does not name, nor the Provider's key (ADR 0030).
func TestAnMCPServerSeesOnlyPathHomeAndItsGrantedSecrets(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin:/bin", "HOME": "/home/alice", "LEAKY": "serve-only", "FROM_ENV": "e1", "NOT_GRANTED": "n1"}
	a, _, _, provider := agentPairTuned(t, func(o *serve.Options) { o.Getenv = func(k string) string { return env[k] } }, "auditor")
	a.secrets["FROM_VAULT"] = "v1"
	installTools(t, a, "[]", `["FROM_ENV", "FROM_VAULT", "NOT_GRANTED"]`)
	grantMCP(t, a, "auditor", "[mcp.\"tools/fx\"]\nsecrets = [\"FROM_VAULT\", \"FROM_ENV\"]\n")
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == 1 {
			endCall(w, "tools__fx__env", `{}`)
			return
		}
		endSay(w, "done")
	})
	if got := chainOf(t, a, "@auditor go"); !strings.HasSuffix(got, "commit:done end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2", len(calls))
	}
	want := `FROM_ENV=e1\nFROM_VAULT=v1\nHOME=/home/alice\nPATH=/usr/bin:/bin"`
	if got := lastTurn(calls[1]); !strings.Contains(got, `"content":"`+want) {
		t.Errorf("env answered %s\nwant exactly %s", got, want)
	}
}

// A grant of a Secret its server does not ask for fails the summon, as
// does a grant naming no installed server, and the provider is never
// called.
func TestAnMCPGrantItsServerCannotTakeFailsTheSummon(t *testing.T) {
	for _, tc := range []struct{ tables, want string }{
		{"[mcp.\"tools/fx\"]\nsecrets = [\"OTHER\"]\n", "does not ask for $OTHER"},
		{"[mcp.\"tools/nope\"]\n", "Plugin tools has no MCP Server \"nope\""},
		{"[mcp.\"gone/fx\"]\n", "no Plugin \"gone\" is installed"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			a, _, _, provider := agentPair(t, "auditor")
			dir := installTools(t, a, "[]", "[]")
			grantMCP(t, a, "auditor", tc.tables)
			s := watch(t, a)
			v, err := a.client().ConversationSend(context.Background(), "@auditor go", nil)
			if err != nil {
				t.Fatal(err)
			}
			var failed string
			for _, e := range untilEnd(t, s, v.MsgID) {
				if e.Type == "agent.error" {
					failed = e.Error
				}
			}
			if !strings.Contains(failed, "@auditor") || !strings.Contains(failed, tc.want) {
				t.Errorf("agent.error = %q, want %q", failed, tc.want)
			}
			if n := len(provider.seen()); n != 0 {
				t.Errorf("%d provider calls, want 0", n)
			}
			if got := starts(t, dir); got != nil {
				t.Errorf("the server started: %v", got)
			}
		})
	}
}

// pidAnswer is the pid a call of the fixture's pid tool answered in c.
func pidAnswer(t *testing.T, c providerCall) string {
	t.Helper()
	got := lastTurn(c)
	_, after, ok := strings.Cut(got, `"content":"`)
	pid, _, _ := strings.Cut(after, `"`)
	if !ok || pid == "" || strings.Contains(got, `"is_error":true`) {
		t.Fatalf("pid answered %s", got)
	}
	return pid
}

// alive reports whether the process pid runs.
func alive(pid string) bool {
	n, err := strconv.Atoi(pid)
	if err != nil {
		return false
	}
	return syscall.Kill(n, 0) == nil
}

// An MCP Server starts only when a run of an agent granting it begins, and
// that one process serves every agent granting it so, run after run; it
// stops when the serve stops (ADR 0030).
func TestAnMCPServerStartsLazilyIsSharedAndStopsWithTheServe(t *testing.T) {
	a, _, _, provider := agentPair(t, "first", "second", "plain")
	dir := installTools(t, a, "[]", "[]")
	grantMCP(t, a, "first", "[mcp.\"tools/fx\"]\n")
	grantMCP(t, a, "second", "[mcp.\"tools/fx\"]\n")
	if got := starts(t, dir); got != nil {
		t.Fatalf("started before any run: %v", got)
	}
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		c := provider.seen()[n-1]
		if strings.Contains(c.Tools, "tools__fx__pid") && !strings.Contains(c.Turns, `"tool_result"`) {
			endCall(w, "tools__fx__pid", `{}`)
			return
		}
		endSay(w, "done")
	})
	if got := chainOf(t, a, "@plain hi"); !strings.HasSuffix(got, "commit:done end") {
		t.Fatalf("events = %s", got)
	}
	if got := starts(t, dir); got != nil {
		t.Fatalf("started for an agent not granting it: %v", got)
	}
	for _, text := range []string{"@first hi", "@second hi", "@first again"} {
		if got := chainOf(t, a, text); !strings.HasSuffix(got, "commit:done end") {
			t.Fatalf("%s: events = %s", text, got)
		}
	}
	var pids []string
	for _, c := range provider.seen() {
		if strings.Contains(c.Turns, `"tool_result"`) {
			pids = append(pids, pidAnswer(t, c))
		}
	}
	started := starts(t, dir)
	if len(pids) != 3 || len(started) != 1 || pids[0] != started[0] || pids[1] != started[0] || pids[2] != started[0] {
		t.Fatalf("calls answered pids %v; started %v; want one process for all", pids, started)
	}
	if !alive(started[0]) {
		t.Fatal("the server is not running between runs")
	}
	a.stop()
	if alive(started[0]) {
		t.Errorf("the server %s still runs after the serve stopped", started[0])
	}
}

// Two agents granting the same server different Secrets get a process each.
func TestDifferentSecretGrantsRunAProcessEach(t *testing.T) {
	a, _, _, provider := agentPair(t, "first", "second")
	a.secrets["TOKEN"] = "t"
	dir := installTools(t, a, "[]", `["TOKEN"]`)
	grantMCP(t, a, "first", "[mcp.\"tools/fx\"]\n")
	grantMCP(t, a, "second", "[mcp.\"tools/fx\"]\nsecrets = [\"TOKEN\"]\n")
	provider.set(func(_ int, w http.ResponseWriter, _ *http.Request) { streamParts(w); endSay(w, "done") })
	chainOf(t, a, "@first hi")
	chainOf(t, a, "@second hi")
	chainOf(t, a, "@first again")
	if got := starts(t, dir); len(got) != 2 {
		t.Fatalf("started %v, want two processes", got)
	}
}

// A server that crashes is started again on its next use: the crashing
// call is a tool error, and the next call is answered by a new process.
func TestACrashedMCPServerRestartsOnNextUse(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	dir := installTools(t, a, "[]", "[]")
	grantMCP(t, a, "auditor", "[mcp.\"tools/fx\"]\n")
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		switch n {
		case 1:
			endCall(w, "tools__fx__pid", `{}`)
		case 2:
			endCall(w, "tools__fx__crash", `{}`)
		case 3:
			endCall(w, "tools__fx__pid", `{}`)
		default:
			endSay(w, "done")
		}
	})
	if got := chainOf(t, a, "@auditor go"); !strings.HasSuffix(got, "commit:done end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if len(calls) != 4 {
		t.Fatalf("%d calls, want 4", len(calls))
	}
	before := pidAnswer(t, calls[1])
	if got := lastTurn(calls[2]); !strings.Contains(got, `"is_error":true`) {
		t.Errorf("crash answered %s", got)
	}
	after := pidAnswer(t, calls[3])
	if before == after || !alive(after) {
		t.Errorf("pid %s before the crash, %s after; want a new process running", before, after)
	}
	if got := starts(t, dir); len(got) != 2 {
		t.Errorf("started %v, want two", got)
	}
}

// A server's tool whose offered name would be over 64 characters fails
// the summon, naming the tool, and the provider is never called.
func TestAnMCPToolNameTooLongFailsTheSummon(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	installTools(t, a, `["--long"]`, "[]")
	grantMCP(t, a, "auditor", "[mcp.\"tools/fx\"]\n")
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor go", nil)
	if err != nil {
		t.Fatal(err)
	}
	var failed string
	for _, e := range untilEnd(t, s, v.MsgID) {
		if e.Type == "agent.error" {
			failed = e.Error
		}
	}
	if !strings.Contains(failed, "@auditor") || !strings.Contains(failed, "tools__fx__"+strings.Repeat("x", 60)) || !strings.Contains(failed, "1–64") {
		t.Errorf("agent.error = %q", failed)
	}
	if n := len(provider.seen()); n != 0 {
		t.Errorf("%d provider calls, want 0", n)
	}
}

// A server that fails to start fails the run with that error, and nothing
// commits.
func TestAnMCPServerThatFailsToStartFailsTheRun(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	dir := filepath.Join(a.paths.PluginsDir(), "tools")
	writeConfig(t, filepath.Join(dir, "plugin.toml"), "name = \"tools\"\nversion = \"1.0.0\"\ndescription = \"Test tools\"\n\n[mcp.fx]\ncommand = \"/bin/false\"\n")
	grantMCP(t, a, "auditor", "[mcp.\"tools/fx\"]\n")
	got := chainOf(t, a, "@auditor go")
	if strings.Contains(got, "commit") || !strings.Contains(got, "error") {
		t.Errorf("events = %s", got)
	}
	if n := len(provider.seen()); n != 0 {
		t.Errorf("%d provider calls, want 0", n)
	}
}

// A call the server does not answer within the per-call timeout is a tool
// error saying so.
func TestAnMCPCallTimesOut(t *testing.T) {
	a, _, _, provider := agentPairTuned(t, func(o *serve.Options) { o.MCPCallTimeout = 2 * time.Second }, "auditor")
	installTools(t, a, "[]", "[]")
	grantMCP(t, a, "auditor", "[mcp.\"tools/fx\"]\n")
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == 1 {
			endCall(w, "tools__fx__sleep", `{}`)
			return
		}
		endSay(w, "done")
	})
	chainOf(t, a, "@auditor go")
	calls := provider.seen()
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2", len(calls))
	}
	if got := lastTurn(calls[1]); !strings.Contains(got, `"is_error":true`) || !strings.Contains(got, "did not answer within 2s") {
		t.Errorf("sleep answered %s", got)
	}
}
