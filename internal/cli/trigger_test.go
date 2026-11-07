package cli_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"defios.com/internal/group"
	"defios.com/internal/nodedir"
	"defios.com/internal/serve"
	"defios.com/internal/trigger"
)

// triggerNode is a Node with a Group whose config.toml also holds body.
func triggerNode(t *testing.T, body string) string {
	t.Helper()
	dir := secretNode(t)
	path := nodedir.Resolve(nodedir.Lookup{Dir: dir}).GroupConfigFile(group.ImplicitAlias)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, []byte(body)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const triggersBody = `
[jobs.notify]
webhook = "https://ntfy.example/x"

[jobs.ping]
exec = ["true"]

[hooks.review_requests]
events = ["sign.request", "request.expired"]
from = "others"
handles = ["user"]
text = "(?i)urgent"
jobs = ["notify", "sign_refuse"]

[hooks.everything]
jobs = ["ping"]

[schedules.daily]
cron = "0 9 * * *"
tz = "Europe/Paris"
jobs = ["ping"]

[schedules.launch]
at = "2099-12-01T09:00:00Z"
jobs = ["notify", "ping"]

[schedules.gone]
at = "2001-01-01T00:00:00Z"
jobs = ["ping"]
`

// hook list and schedule list read the Group's config.toml, with no serve
// running: each Trigger by name with what it fires on and the Jobs it runs,
// and a Schedule's next firing, none once an at time has passed.
func TestHookAndScheduleListReadConfig(t *testing.T) {
	dir := triggerNode(t, triggersBody)
	r := run(t, nil, "", "--dir", dir, "hook", "list")
	want := "everything  on every event  jobs ping\n" +
		"review_requests  on sign.request, request.expired  from others  handles user  text (?i)urgent  jobs notify, sign_refuse\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("hook list exit %d:\n%s%s\nwant:\n%s", r.code, r.stdout, r.stderr, want)
	}
	r = run(t, nil, "", "--dir", dir, "schedule", "list")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 3 {
		t.Fatalf("schedule list exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.HasPrefix(lines[0], `daily  cron "0 9 * * *" Europe/Paris  next `) || !strings.Contains(lines[0], "T09:00:00+0") || !strings.HasSuffix(lines[0], "  jobs ping") {
		t.Errorf("daily = %q", lines[0])
	}
	if want := "gone  at 2001-01-01T00:00:00Z  next none  jobs ping"; lines[1] != want {
		t.Errorf("gone = %q, want %q", lines[1], want)
	}
	if want := "launch  at 2099-12-01T09:00:00Z  next 2099-12-01T09:00:00Z  jobs notify, ping"; lines[2] != want {
		t.Errorf("launch = %q, want %q", lines[2], want)
	}

	r = run(t, nil, "", "--dir", dir, "--json", "schedule", "list")
	var sv []struct {
		Name string   `json:"name"`
		At   string   `json:"at"`
		Next string   `json:"next"`
		Jobs []string `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &sv); err != nil || len(sv) != 3 || sv[2].Name != "launch" || sv[2].Next != "2099-12-01T09:00:00Z" || sv[1].Next != "" {
		t.Fatalf("schedule list --json = %s (%v)", r.stdout, err)
	}
	r = run(t, nil, "", "--dir", dir, "--json", "hook", "list")
	var hv []struct {
		Name   string   `json:"name"`
		Events []string `json:"events"`
		From   []string `json:"from"`
		Jobs   []string `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &hv); err != nil || len(hv) != 2 || hv[1].Name != "review_requests" || hv[1].From[0] != "others" || len(hv[1].Jobs) != 2 {
		t.Fatalf("hook list --json = %s (%v)", r.stdout, err)
	}

	// None defined is nothing to list.
	empty := secretNode(t)
	if r := run(t, nil, "", "--dir", empty, "hook", "list"); r.code != 0 || r.stdout != "" {
		t.Errorf("no hooks: exit %d %q", r.code, r.stdout)
	}
}

// fakeJobRun answers POST /v1/job/run with view, keeping the body.
func fakeJobRun(t *testing.T, dir string, view serve.JobView) *atomic.Value {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, "serve.sock"))
	if err != nil {
		t.Fatal(err)
	}
	body := new(atomic.Value)
	body.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/job/run", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		json.NewEncoder(w).Encode(view)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return body
}

// job run asks serve to run a Job on the event in --event, or none, and
// exits as the Job ended: 0 if it succeeded, 2 with its error if not.
func TestJobRunThroughServe(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	body := fakeJobRun(t, dir, serve.JobView{Job: "ping", OK: true})
	r := run(t, nil, "", "--dir", dir, "job", "run", "ping")
	if r.code != 0 || r.stdout != "job ping done\n" {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if got := body.Load().(string); strings.TrimSpace(got) != `{"job":"ping"}` {
		t.Errorf("body %s, want the Job with no event", got)
	}
	event := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(event, []byte(`{"type":"sign.request","sign_request_id":"ab","depth":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := run(t, nil, "", "--dir", dir, "job", "run", "ping", "--event", event); r.code != 0 {
		t.Fatalf("--event exit %d: %s", r.code, r.stderr)
	}
	var req struct {
		Event trigger.Event `json:"event"`
	}
	if err := json.Unmarshal([]byte(body.Load().(string)), &req); err != nil || req.Event.Type != "sign.request" || req.Event.SignRequestID != "ab" || req.Event.Depth != 2 {
		t.Errorf("body %s (%v), want the event", body.Load(), err)
	}
	// An event file that does not read as one is a usage error.
	if err := os.WriteFile(event, []byte(`{"typo":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := run(t, nil, "", "--dir", dir, "job", "run", "ping", "--event", event); r.code != 1 || !strings.Contains(r.stderr, "typo") {
		t.Errorf("bad event: exit %d stderr %q", r.code, r.stderr)
	}

	failing := t.TempDir()
	mustInit(t, failing)
	fakeJobRun(t, failing, serve.JobView{Job: "ping", Error: "exit status 1"})
	if r := run(t, nil, "", "--dir", failing, "job", "run", "ping"); r.code != 2 || !strings.Contains(r.stderr, "job ping failed: exit status 1") {
		t.Errorf("failed Job: exit %d stderr %q", r.code, r.stderr)
	}
	unknown := t.TempDir()
	mustInit(t, unknown)
	fakeLocalServe(t, unknown, "/v1/job/", 404, "no-job", "serve: no such Job: no [jobs.pong] in this Group's config.toml")
	if r := run(t, nil, "", "--dir", unknown, "job", "run", "pong"); r.code != 1 || !strings.Contains(r.stderr, "no [jobs.pong]") {
		t.Errorf("unknown Job: exit %d stderr %q", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", t.TempDir(), "job", "run", "ping"); r.code != 2 || !strings.Contains(r.stderr, "defios serve") {
		t.Errorf("no serve: exit %d stderr %q", r.code, r.stderr)
	}
}

// follow shows a Trigger firing and how each Job it ran ended.
func TestConversationFollowShowsTriggersAndJobs(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	ok, failed := true, false
	newChainServe(t, dir, &chainServe{playAtOnce: true, events: []serve.ChainEvent{
		{Type: serve.EventTriggerFire, Trigger: "said", Event: &trigger.Event{Type: "app.freeform", Trigger: "said", Seq: 4}},
		{Type: serve.EventJobEnd, Job: "notify", OK: &ok},
		{Type: serve.EventJobEnd, Job: "sign_refuse", OK: &failed, Error: "exit status 1"},
		{Type: serve.EventTriggerFire, Trigger: "daily", Event: &trigger.Event{Type: trigger.ScheduleEvent, Trigger: "daily", Time: "2026-11-09T09:00:00+01:00"}},
	}})
	r := run(t, nil, "", "--dir", dir, "conversation", "follow")
	want := `[hook said fired on app.freeform at seq 4]
[job notify done]
[job sign_refuse failed: exit status 1]
[schedule daily fired for 2026-11-09T09:00:00+01:00]
`
	if r.stdout != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
}
