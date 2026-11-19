package serve_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/jobrun"
	"defios.com/internal/serve"
)

// waitJobRuns polls n's Job Runs until done says they are all there,
// failing with what it last read. They come newest first.
func waitJobRuns(t *testing.T, n *node, done func([]jobrun.Run) bool) []jobrun.Run {
	t.Helper()
	dir := n.paths.JobRunsDir(group.ImplicitAlias)
	var runs []jobrun.Run
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var err error
		if runs, err = jobrun.List(dir); err != nil {
			t.Fatal(err)
		}
		if done(runs) {
			return runs
		}
	}
	t.Fatalf("Job Runs %+v", runs)
	return nil
}

// A Hook's firing keeps a Job Run for each of its Jobs: an exec Job's
// command, exit code and output, a webhook's attempts and response with
// no Secret, and one never run after a Job failed. They outlast a
// restart of serve.
func TestAHooksFiringKeepsItsJobRuns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "got it")
	}))
	defer srv.Close()
	secrets := map[string]string{"HOOK_URL": srv.URL + "/path-s3cr3t", "HOOK_TOKEN": "Bearer tok-s3cr3t"}
	a, b := pairWith(t, func(n *node) {
		if n.name != "bob" {
			return
		}
		n.tune = func(o *serve.Options) {
			o.Getenv = func(k string) string {
				if v, ok := secrets[k]; ok {
					return v
				}
				return os.Getenv(k)
			}
			o.Config.Jobs = map[string]config.Job{
				"say":   {Exec: []string{"sh", "-c", "echo said; echo whine >&2"}},
				"web":   {Webhook: "$HOOK_URL", Headers: map[string]string{"Authorization": "$HOOK_TOKEN"}},
				"fail":  {Exec: []string{"false"}},
				"after": {Exec: []string{"true"}},
			}
			o.Config.Hooks = map[string]config.Hook{
				"said": {Text: "^hi$", Jobs: []string{"say", "web", "fail", "after"}},
			}
		}
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	if _, err := a.client().ConversationSend(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	ended := func(runs []jobrun.Run) bool {
		return len(runs) == 4 && runs[0].State == jobrun.NotRun && runs[1].State == jobrun.Failed
	}
	runs := waitJobRuns(t, b, ended)
	say, web, fail, after := runs[3], runs[2], runs[1], runs[0]
	for _, r := range runs {
		if r.Trigger != "said" || r.Event.Type != "app.freeform" || r.Event.Text != "hi" || r.Firing != say.ID || r.Fired.IsZero() {
			t.Errorf("run %d: trigger %q on %+v, firing %d", r.ID, r.Trigger, r.Event, r.Firing)
		}
	}
	if say.State != jobrun.OK || say.Kind != config.JobExec || say.Exec == nil || say.Exec.Stdout != "said\n" || say.Exec.Stderr != "whine\n" ||
		say.Exec.ExitCode == nil || *say.Exec.ExitCode != 0 || say.Started.IsZero() || say.Ended.Before(say.Started) {
		t.Errorf("say = %+v, exec %+v", say, say.Exec)
	}
	if w := web.Webhook; web.State != jobrun.OK || w == nil || w.URL != "$HOOK_URL" || len(w.Attempts) != 1 || w.Attempts[0].Status != 200 || w.Body != "got it" || w.ContentType != "text/plain" {
		t.Errorf("web = %+v, webhook %+v", web, w)
	}
	if fail.Error == "" || fail.Exec == nil || fail.Exec.ExitCode == nil || *fail.Exec.ExitCode != 1 {
		t.Errorf("fail = %+v", fail)
	}
	if !after.Started.IsZero() || after.Exec != nil {
		t.Errorf("after = %+v, want it never started", after)
	}
	files, _ := filepath.Glob(filepath.Join(b.paths.JobRunsDir(group.ImplicitAlias), "*"))
	for _, f := range files {
		if strings.Contains(readFile(t, f), "s3cr3t") {
			t.Errorf("%s holds a Secret", filepath.Base(f))
		}
	}

	b.restart(t)
	if again := waitJobRuns(t, b, ended); again[3].Exec == nil || again[3].Exec.Stdout != "said\n" {
		t.Errorf("after a restart, say = %+v", again[3])
	}
}

// An exec Job's talk messages, written with the DEFIOS_JOB_RUN it was
// given, are recorded on its Job Run by msg_id: while it runs, after it
// ended, and queued in the Outbox. The message is the same with the token
// as without, and a token naming no Job Run, or one dropped, writes it all
// the same.
func TestAnExecJobsMessagesAreRecordedOnItsJobRun(t *testing.T) {
	tmp := t.TempDir()
	tokFile, goFile := filepath.Join(tmp, "tok"), filepath.Join(tmp, "go")
	a, b := pairWith(t, func(n *node) {
		if n.name != "bob" {
			return
		}
		n.tune = func(o *serve.Options) {
			o.Getenv = os.Getenv
			o.Config.Jobs = map[string]config.Job{
				"talk": {Exec: []string{"sh", "-c", `echo "$DEFIOS_JOB_RUN" > "$0.tmp"; mv "$0.tmp" "$0"; while [ ! -e "$1" ]; do sleep 0.02; done`, tokFile, goFile}},
			}
			o.Config.Hooks = map[string]config.Hook{"said": {Text: "^hi$", Jobs: []string{"talk"}}}
		}
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	ctx := context.Background()
	if _, err := a.client().ConversationSend(ctx, "hi", nil); err != nil {
		t.Fatal(err)
	}
	var token string
	for deadline := time.Now().Add(20 * time.Second); token == ""; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the Job never ran")
		}
		b, _ := os.ReadFile(tokFile)
		token = strings.TrimSpace(string(b))
	}
	send := func(text, jobRun string) serve.SendView {
		t.Helper()
		v, err := b.client().ConversationSendAs(ctx, text, "", 1, jobRun, nil)
		if err != nil {
			t.Fatalf("send %q with token %q: %v", text, jobRun, err)
		}
		return v
	}
	one, two := send("one", token), send("two", token)
	send("one", "")
	id, _, _ := strings.Cut(token, "-")
	for _, bad := range []string{"nope", "999-1", id + "-1", id} {
		send("stray", bad)
	}
	if err := os.WriteFile(goFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runs := waitJobRuns(t, b, func(runs []jobrun.Run) bool { return len(runs) == 1 && runs[0].State == jobrun.OK })
	if got, want := runs[0].Messages, []string{one.MsgID, two.MsgID}; !slices.Equal(got, want) {
		t.Errorf("Job Run messages %v, want %v", got, want)
	}

	rows := b.groupLevelRows(t, gid, 4)
	var payloads [][]byte
	for _, r := range rows {
		env, err := r.Decode()
		if err != nil {
			t.Fatal(err)
		}
		if r.Sender == b.id.NodeID && strings.Contains(string(env.Payload), "one") {
			payloads = append(payloads, env.Payload)
		}
	}
	if len(payloads) != 2 || !bytes.Equal(payloads[0], payloads[1]) {
		t.Errorf("payloads with and without the token: %x", payloads)
	}

	three := send("three", token) // after the Job ended
	a.stop()
	b.waitLeaderless(t)
	four := send("four", token)
	if !four.Queued {
		t.Fatalf("four = %+v, want it queued", four)
	}
	runs = waitJobRuns(t, b, func(runs []jobrun.Run) bool { return len(runs) == 1 && len(runs[0].Messages) == 4 })
	if got, want := runs[0].Messages, []string{one.MsgID, two.MsgID, three.MsgID, four.MsgID}; !slices.Equal(got, want) {
		t.Errorf("Job Run messages %v, want %v", got, want)
	}
}
