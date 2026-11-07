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
	if err := r.Run(context.Background(), "archive", j, event); err != nil {
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
	want := []string{"DEFIOS_CONVERSATION", "DEFIOS_DATA_DIR", "DEFIOS_DEPTH", "DEFIOS_EVENT", "DEFIOS_GROUP", "DEFIOS_MSG_ID", "DEFIOS_SIGN_REQUEST_ID", "DEFIOS_TRIGGER", "PATH", "STORED", "TOKEN"}
	if !slices.Equal(env, want) {
		t.Errorf("environment %v, want %v", env, want)
	}
	if !strings.Contains(string(b), "STORED=from-vault") || !strings.Contains(string(b), "TOKEN=from-env") || !strings.Contains(string(b), "DEFIOS_DEPTH=1") {
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
	if err := r.Run(context.Background(), "j", config.Job{Exec: []string{"false"}}, event); err == nil {
		t.Error("a non-zero exit succeeded")
	}
	start := time.Now()
	if err := r.Run(context.Background(), "j", config.Job{Exec: []string{"sleep", "10"}, Timeout: config.Age(100 * time.Millisecond)}, event); err == nil {
		t.Error("a run past its timeout succeeded")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the timed-out run took %v", d)
	}
	out := filepath.Join(t.TempDir(), "ran")
	err := r.Run(context.Background(), "j", config.Job{Exec: []string{"touch", out}, Secrets: []string{"MISSING"}}, event)
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
	if err := r.Run(context.Background(), "notify", j, event); err != nil {
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
		err := r.Run(context.Background(), "notify", config.Job{Webhook: srv.URL}, event)
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
	err := r.Run(context.Background(), "notify", config.Job{Webhook: srv.URL, Timeout: config.Age(50 * time.Millisecond)}, event)
	if err == nil {
		t.Fatal("a hung webhook succeeded")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("4 timed-out attempts took %v", d)
	}
}

// The Hooks that fire on an event are those it matches, in name order, and
// none at or past the Group's max_depth: depth bounds loops (ADR 0032).
func TestHooksFireBelowMaxDepthOnly(t *testing.T) {
	hooks := map[string]config.Hook{
		"b_said":   {Events: []string{"app.freeform"}},
		"a_all":    {},
		"requests": {Events: []string{"sign.request"}},
	}
	said := trigger.Event{Type: "app.freeform", From: other.String(), Handle: "user", Conversation: "group"}
	if got := trigger.Firing(hooks, said, self, 1); !slices.Equal(got, []string{"a_all", "b_said"}) {
		t.Errorf("at depth 0, max 1: %v, want a_all and b_said", got)
	}
	said.Depth = 1
	if got := trigger.Firing(hooks, said, self, 1); len(got) != 0 {
		t.Errorf("at depth 1, max 1: %v, want none", got)
	}
	if got := trigger.Firing(hooks, said, self, 2); len(got) != 2 {
		t.Errorf("at depth 1, max 2: %v, want two", got)
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
