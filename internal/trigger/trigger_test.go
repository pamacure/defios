package trigger_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/identity"
	"defios.com/internal/secret"
	"defios.com/internal/signreq"
	"defios.com/internal/trigger"
)

var (
	self  = identity.ID{1}
	other = identity.ID{2}
)

// A Hook's filters are ANDed, each optional, each a list any entry of
// which matches (ADR 0032).
func TestAHookMatchesAnEventEveryFilterAllows(t *testing.T) {
	said := trigger.Event{Type: "app.freeform", From: other.String(), Handle: "user", Conversation: "group", Text: "please sign, URGENT"}
	request := trigger.Event{Type: "request.expired", From: self.String(), Handle: "auditor", Conversation: "ecdsa-02a1b2c3"}
	for _, tc := range []struct {
		name string
		hook config.Hook
		ev   trigger.Event
		want bool
	}{
		{"no filters", config.Hook{}, said, true},
		{"event listed", config.Hook{Events: []string{"sign.request", "app.freeform"}}, said, true},
		{"event not listed", config.Hook{Events: []string{"sign.request"}}, said, false},
		{"status event", config.Hook{Events: []string{"request.expired"}}, request, true},
		{"from others", config.Hook{From: config.Names{"others"}}, said, true},
		{"from self, written by another", config.Hook{From: config.Names{"self"}}, said, false},
		{"from self", config.Hook{From: config.Names{"self"}}, request, true},
		{"from its Node ID", config.Hook{From: config.Names{other.String()}}, said, true},
		{"from another Node ID", config.Hook{From: config.Names{identity.ID{3}.String()}}, said, false},
		{"a person", config.Hook{Handles: []string{"user"}}, said, true},
		{"an agent, said by a person", config.Hook{Handles: []string{"auditor"}}, said, false},
		{"that agent", config.Hook{Handles: []string{"user", "auditor"}}, request, true},
		{"group-level", config.Hook{Conversation: config.Names{"group"}}, said, true},
		{"another Conversation", config.Hook{Conversation: config.Names{"group"}}, request, false},
		{"text matches", config.Hook{Text: "(?i)urgent"}, said, true},
		{"text does not match", config.Hook{Text: "^urgent"}, said, false},
		{"text, on an event with none", config.Hook{Text: ".*"}, request, false},
		{"all ANDed, one fails", config.Hook{Events: []string{"app.freeform"}, From: config.Names{"self"}}, said, false},
		{"all ANDed, all pass", config.Hook{Events: []string{"app.freeform"}, From: config.Names{"others"}, Handles: []string{"user"}, Conversation: config.Names{"group"}, Text: "sign"}, said, true},
	} {
		if got := trigger.Matches(tc.hook, tc.ev, self); got != tc.want {
			t.Errorf("%s: matches %v, want %v", tc.name, got, tc.want)
		}
	}
}

// runner is a Runner whose log and Secrets the test holds.
type runner struct {
	trigger.Runner
	mu    sync.Mutex
	lines []string
}

func newRunner(t *testing.T, env map[string]string, vault map[string]string) *runner {
	r := &runner{}
	r.Runner = trigger.Runner{
		Group: "home",
		Dir:   t.TempDir(),
		Env:   map[string]string{"DEFIOS_DATA_DIR": "/data"},
		Secrets: func() *secret.Resolver {
			return &secret.Resolver{Group: "home", Getenv: func(k string) string { return env[k] }, Vault: func() (map[string]string, error) { return vault, nil }}
		},
		Getenv:  func(k string) string { return env[k] },
		Logf:    r.logf,
		Backoff: time.Millisecond,
	}
	return r
}

func (r *runner) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *runner) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lines)
}

var event = trigger.Event{
	Type: "sign.request", Trigger: "review", Group: "home", Seq: 7, MsgID: "ab12", SignRequestID: "ab12",
	From: other.String(), Handle: "user", Conversation: "ecdsa-02a1b2c3", Depth: 0,
}

// An exec Job runs its argument list with no shell, in the Group
// Workspace, the event as JSON on its stdin, and an environment of PATH,
// the DEFIOS_* variables and the Secrets it names only; $DEFIOS_* in its
// arguments are expanded, and its output goes to the serve log.
func TestAnExecJobRunsItsArgvWithTheEventAndAClosedEnvironment(t *testing.T) {
	r := newRunner(t, map[string]string{"PATH": os.Getenv("PATH"), "HOME": "/home/x", "TOKEN": "from-env"}, map[string]string{"STORED": "from-vault"})
	out := filepath.Join(t.TempDir(), "out")
	j := config.Job{
		Exec:    []string{"sh", "-c", `{ pwd; env | sort; cat; echo; echo "arg=$1"; } > "$0"; echo done; echo oops >&2`, out, "${DEFIOS_SIGN_REQUEST_ID}-$DEFIOS_EVENT $HOME"},
		Secrets: []string{"TOKEN", "STORED"},
	}
	r.JobRun = "12-345"
	if _, err := r.Run(context.Background(), "archive", j, event); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	if got[0] != r.Dir {
		t.Errorf("ran in %s, want the Group Workspace %s", got[0], r.Dir)
	}
	var env []string
	var stdin string
	for _, l := range got[1:] {
		switch {
		case strings.HasPrefix(l, "{"):
			stdin = l
		case strings.HasPrefix(l, "arg="):
			if l != "arg=ab12-sign.request $HOME" {
				t.Errorf("%s, want $DEFIOS_* expanded and nothing else", l)
			}
		case strings.Contains(l, "="):
			env = append(env, strings.SplitN(l, "=", 2)[0])
		}
	}
	// sh sets PWD, SHLVL and _ itself.
	env = slices.DeleteFunc(env, func(k string) bool { return k == "PWD" || k == "SHLVL" || k == "_" })
	want := []string{"DEFIOS_CONVERSATION", "DEFIOS_DATA_DIR", "DEFIOS_DEPTH", "DEFIOS_EVENT", "DEFIOS_GROUP", "DEFIOS_JOB_RUN", "DEFIOS_MSG_ID", "DEFIOS_SESSION_ID", "DEFIOS_SIGN_REQUEST_ID", "DEFIOS_TRIGGER", "PATH", "STORED", "TOKEN"}
	if !slices.Equal(env, want) {
		t.Errorf("environment %v, want %v", env, want)
	}
	if !strings.Contains(string(b), "STORED=from-vault") || !strings.Contains(string(b), "TOKEN=from-env") || !strings.Contains(string(b), "DEFIOS_DEPTH=1") ||
		!strings.Contains(string(b), "DEFIOS_JOB_RUN=12-345") {
		t.Errorf("environment values wrong:\n%s", b)
	}
	var ev trigger.Event
	if err := json.Unmarshal([]byte(stdin), &ev); err != nil || ev != event {
		t.Errorf("stdin %q (%v), want the event as JSON", stdin, err)
	}
	log := strings.Join(r.log(), "\n")
	if !strings.Contains(log, "done") || !strings.Contains(log, "oops") {
		t.Errorf("log %q, want the Job's stdout and stderr", log)
	}
}

// An exec Job fails on a non-zero exit, on its timeout, and on a Secret
// that resolves nowhere, which it does not start without.
func TestAnExecJobFails(t *testing.T) {
	r := newRunner(t, map[string]string{"PATH": os.Getenv("PATH")}, nil)
	if _, err := r.Run(context.Background(), "j", config.Job{Exec: []string{"false"}}, event); err == nil {
		t.Error("a non-zero exit succeeded")
	}
	start := time.Now()
	if _, err := r.Run(context.Background(), "j", config.Job{Exec: []string{"sleep", "10"}, Timeout: config.Age(100 * time.Millisecond)}, event); err == nil {
		t.Error("a run past its timeout succeeded")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the timed-out run took %v", d)
	}
	out := filepath.Join(t.TempDir(), "ran")
	_, err := r.Run(context.Background(), "j", config.Job{Exec: []string{"touch", out}, Secrets: []string{"MISSING"}}, event)
	if !errors.Is(err, secret.ErrUnresolved) {
		t.Errorf("unresolved Secret: %v, want ErrUnresolved", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("the Job ran without its Secret")
	}
}

// A webhook Job POSTs the event as JSON to its URL, with its headers, each
// URL and header value a literal or a Secret reference; a 2xx succeeds.
func TestAWebhookJobPostsTheEvent(t *testing.T) {
	var got struct {
		method, auth, title, ctype string
		body                       []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got.method, got.auth, got.title, got.ctype = req.Method, req.Header.Get("Authorization"), req.Header.Get("X-Title"), req.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(req.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	r := newRunner(t, map[string]string{"HOOK_URL": srv.URL + "/notify"}, map[string]string{"HOOK_TOKEN": "Bearer t"})
	j := config.Job{Webhook: "$HOOK_URL", Headers: map[string]string{"Authorization": "$HOOK_TOKEN", "X-Title": "defios"}}
	if _, err := r.Run(context.Background(), "notify", j, event); err != nil {
		t.Fatal(err)
	}
	var ev trigger.Event
	if err := json.Unmarshal(got.body, &ev); err != nil || ev != event {
		t.Errorf("body %s (%v), want the event", got.body, err)
	}
	if got.method != http.MethodPost || got.auth != "Bearer t" || got.title != "defios" || got.ctype != "application/json" {
		t.Errorf("request %+v", got)
	}
}

// A webhook Job retries a 5xx or a dropped connection up to 3 times with
// backoff, and never a 4xx.
func TestAWebhookJobRetriesServerErrorsOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []int // per attempt; 0 drops the connection
		ok       bool
		attempts int32
	}{
		{"ok at once", []int{200}, true, 1},
		{"5xx then ok", []int{503, 500, 204}, true, 3},
		{"dropped then ok", []int{0, 200}, true, 2},
		{"5xx four times", []int{500, 500, 500, 500, 200}, false, 4},
		{"4xx", []int{404, 200}, false, 1},
	} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			i := n.Add(1) - 1
			st := tc.statuses[min(int(i), len(tc.statuses)-1)]
			if st == 0 {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			w.WriteHeader(st)
		}))
		r := newRunner(t, nil, nil)
		_, err := r.Run(context.Background(), "notify", config.Job{Webhook: srv.URL}, event)
		srv.Close()
		if (err == nil) != tc.ok || n.Load() != tc.attempts {
			t.Errorf("%s: err %v after %d attempts, want ok %v after %d", tc.name, err, n.Load(), tc.ok, tc.attempts)
		}
	}
}

// Each webhook attempt is bounded by the Job's timeout.
func TestAWebhookAttemptTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	r := newRunner(t, nil, nil)
	start := time.Now()
	_, err := r.Run(context.Background(), "notify", config.Job{Webhook: srv.URL, Timeout: config.Age(50 * time.Millisecond)}, event)
	if err == nil {
		t.Fatal("a hung webhook succeeded")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("4 timed-out attempts took %v", d)
	}
}

// The Hooks that fire on an event are those it matches, in name order, and
// none past the Group's max_depth: depth bounds loops (ADR 0032). An agent's
// reply at max_depth fires them, so a Hook on its Handle can.
func TestHooksFireUpToMaxDepthOnly(t *testing.T) {
	hooks := map[string]config.Hook{
		"b_said":   {Events: []string{"app.freeform"}},
		"a_all":    {},
		"requests": {Events: []string{"sign.request"}},
	}
	said := trigger.Event{Type: "app.freeform", From: other.String(), Handle: "user", Conversation: "group"}
	if got := trigger.Firing(hooks, said, self, 1); !slices.Equal(got, []string{"a_all", "b_said"}) {
		t.Errorf("at depth 0, max 1: %v, want a_all and b_said", got)
	}
	said.Depth, said.Handle = 1, "general"
	if got := trigger.Firing(hooks, said, self, 1); !slices.Equal(got, []string{"a_all", "b_said"}) {
		t.Errorf("at depth 1, max 1: %v, want a_all and b_said", got)
	}
	general := map[string]config.Hook{"general": {Handles: []string{"general"}}}
	if got := trigger.Firing(general, said, self, 1); !slices.Equal(got, []string{"general"}) {
		t.Errorf("@general's reply at depth 1, max 1: %v, want the Hook on its Handle", got)
	}
	said.Depth = 2
	if got := trigger.Firing(hooks, said, self, 1); len(got) != 0 {
		t.Errorf("at depth 2, max 1: %v, want none", got)
	}
	if got := trigger.Firing(hooks, said, self, 2); len(got) != 2 {
		t.Errorf("at depth 2, max 2: %v, want two", got)
	}
}

// A Sign Request's status change is a request.<status> event: one first
// seen is a change from nothing, and one that keeps its status is none.
// It has the request's depth.
func TestStatusChangesAreRequestEvents(t *testing.T) {
	list := []signreq.Summary{
		{Seq: 3, MsgID: "aa", Conversation: "c1", Proposer: other.String(), Status: signreq.Open},
		{Seq: 5, MsgID: "bb", Conversation: "c1", Proposer: self.String(), ProposerHandle: "auditor", Depth: 2, Status: signreq.Expired},
		{Seq: 8, MsgID: "cc", Conversation: "c1", Proposer: self.String(), Status: signreq.Open},
	}
	prev := map[string]string{"aa": signreq.Open, "bb": signreq.Open}
	evs, cur := trigger.StatusChanges(prev, list)
	want := []trigger.Event{
		{Type: "request.expired", Seq: 5, MsgID: "bb", SignRequestID: "bb", From: self.String(), Handle: "auditor", Depth: 2, ConversationID: "c1", Status: signreq.Expired},
		{Type: "request.open", Seq: 8, MsgID: "cc", SignRequestID: "cc", From: self.String(), Handle: "user", ConversationID: "c1", Status: signreq.Open},
	}
	if !slices.Equal(evs, want) {
		t.Errorf("events %+v, want %+v", evs, want)
	}
	if cur["aa"] != signreq.Open || cur["bb"] != signreq.Expired || cur["cc"] != signreq.Open || len(cur) != 3 {
		t.Errorf("statuses %v", cur)
	}
}

// An exec Job's run keeps its command, as run, its exit code, and its
// stdout and stderr apart, while each line still goes to the serve log.
func TestAnExecJobKeepsItsCommandExitCodeAndOutput(t *testing.T) {
	r := newRunner(t, map[string]string{"PATH": os.Getenv("PATH")}, nil)
	j := config.Job{Exec: []string{"sh", "-c", "echo out; echo err >&2; exit 3", "$DEFIOS_EVENT"}}
	out, err := r.Run(context.Background(), "j", j, event)
	if err == nil {
		t.Fatal("a non-zero exit succeeded")
	}
	x := out.Exec
	if x == nil {
		t.Fatal("no exec output kept")
	}
	if want := []string{"sh", "-c", "echo out; echo err >&2; exit 3", "sign.request"}; !slices.Equal(x.Command, want) {
		t.Errorf("command %q, want %q", x.Command, want)
	}
	if x.ExitCode == nil || *x.ExitCode != 3 {
		t.Errorf("exit code %v, want 3", x.ExitCode)
	}
	if x.Stdout != "out\n" || x.Stderr != "err\n" {
		t.Errorf("stdout %q, stderr %q", x.Stdout, x.Stderr)
	}
	if out.Webhook != nil {
		t.Error("an exec Job kept webhook output")
	}
	log := strings.Join(r.log(), "\n")
	if !strings.Contains(log, "out") || !strings.Contains(log, "err") {
		t.Errorf("log %q, want the Job's output too", log)
	}
}

// A Job that never starts keeps no exit code.
func TestAnExecJobThatNeverStartsKeepsNoExitCode(t *testing.T) {
	r := newRunner(t, map[string]string{"PATH": os.Getenv("PATH")}, nil)
	out, err := r.Run(context.Background(), "j", config.Job{Exec: []string{"no-such-command-defios"}}, event)
	if err == nil || out.Exec == nil || out.Exec.ExitCode != nil {
		t.Fatalf("err %v, output %+v: want a failure with no exit code", err, out.Exec)
	}
}

// Output keeps its last 64 KiB, marked where it was cut.
func TestAJobKeepsTheLast64KiBOfItsOutput(t *testing.T) {
	r := newRunner(t, map[string]string{"PATH": os.Getenv("PATH")}, nil)
	// 100000 a's, then END: 100003 bytes, of which the last 65536 stay.
	j := config.Job{Exec: []string{"sh", "-c", `head -c 100000 /dev/zero | tr '\0' a; printf END`}}
	out, err := r.Run(context.Background(), "j", j, event)
	if err != nil {
		t.Fatal(err)
	}
	want := "[34467 bytes cut]\n" + strings.Repeat("a", 65533) + "END"
	if out.Exec.Stdout != want {
		t.Errorf("stdout is %d bytes starting %q, want %d starting %q", len(out.Exec.Stdout), out.Exec.Stdout[:30], len(want), want[:30])
	}
}

// A webhook Job's run keeps its method, its URL as configured, each
// attempt's status, the last response's body and Content-Type, and no
// Secret: not the URL's, not a header's.
func TestAWebhookJobKeepsItsAttemptsAndNoSecret(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Request-Echo", req.Header.Get("Authorization"))
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "busy")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "thanks")
	}))
	defer srv.Close()
	secretURL := srv.URL + "/hook-path-s3cr3t"
	r := newRunner(t, map[string]string{"HOOK_URL": secretURL}, map[string]string{"HOOK_TOKEN": "Bearer tok-s3cr3t"})
	j := config.Job{Webhook: "$HOOK_URL", Headers: map[string]string{"Authorization": "$HOOK_TOKEN"}}
	out, err := r.Run(context.Background(), "notify", j, event)
	if err != nil {
		t.Fatal(err)
	}
	w := out.Webhook
	if w == nil {
		t.Fatal("no webhook output kept")
	}
	if w.Method != http.MethodPost || w.URL != "$HOOK_URL" {
		t.Errorf("method %q URL %q, want POST $HOOK_URL", w.Method, w.URL)
	}
	if len(w.Attempts) != 2 || w.Attempts[0].Status != 503 || w.Attempts[1].Status != 200 {
		t.Errorf("attempts %+v, want 503 then 200", w.Attempts)
	}
	if w.Body != "thanks" || w.ContentType != "text/plain" {
		t.Errorf("body %q, Content-Type %q", w.Body, w.ContentType)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "s3cr3t") {
		t.Errorf("a Secret is kept: %s", b)
	}
}

// A webhook response body keeps its last 64 KiB, marked where it was cut.
func TestAWebhookKeepsTheLast64KiBOfItsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, strings.Repeat("b", 70000)+"END")
	}))
	defer srv.Close()
	r := newRunner(t, nil, nil)
	out, err := r.Run(context.Background(), "notify", config.Job{Webhook: srv.URL}, event)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[4467 bytes cut]\n" + strings.Repeat("b", 65533) + "END"; out.Webhook.Body != want {
		t.Errorf("body is %d bytes starting %q", len(out.Webhook.Body), out.Webhook.Body[:30])
	}
}
