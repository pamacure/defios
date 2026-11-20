package serve_test

import (
	"bytes"
	"context"
	"fmt"
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
	"defios.com/internal/key"
	"defios.com/internal/serve"
	"defios.com/internal/signreq"
	"defios.com/internal/trigger"
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

// A Job run by hand keeps a Job Run, marked so, with no Trigger: on the
// event given, if any, with its output, and the talk messages it writes
// with the DEFIOS_JOB_RUN it is given. It shows on its event's stream as
// job.run once kept, then job.end. On a Sign Request's event it is not
// among that request's Hook runs.
func TestAJobRunByHandKeepsAJobRun(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "tok")
	a, b := pairWith(t, func(n *node) {
		if n.name == "bob" {
			n.tune = func(o *serve.Options) { o.Getenv = os.Getenv }
		}
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	writeConfig(t, b.paths.GroupConfigFile(group.ImplicitAlias), fmt.Sprintf(`
[jobs.say]
exec = ["sh", "-c", "echo said; echo \"$DEFIOS_JOB_RUN\" > \"$0\"", %q]
`, tokFile))
	s := watch(t, b)
	ctx := context.Background()
	ev := &trigger.Event{Type: "request.open", SignRequestID: "abcd", MsgID: "abcd"}
	v, err := b.client().JobRun(ctx, "say", ev)
	if err != nil || !v.OK {
		t.Fatalf("job run say = %+v, %v", v, err)
	}
	runs := waitJobRuns(t, b, func(runs []jobrun.Run) bool { return len(runs) == 1 && runs[0].State == jobrun.OK })
	r := runs[0]
	if !r.ByHand || r.Trigger != "" || r.Job != "say" || r.Event.Type != "request.open" || r.Event.SignRequestID != "abcd" ||
		r.Exec == nil || r.Exec.Stdout != "said\n" || r.Firing != r.ID {
		t.Errorf("Job Run %+v, exec %+v", r, r.Exec)
	}
	if got := strings.TrimSpace(readFile(t, tokFile)); got != r.Token() {
		t.Errorf("DEFIOS_JOB_RUN = %q, want %q", got, r.Token())
	}
	if run := nextOfTypes(t, s, serve.EventJobRun, serve.EventJobEnd); run.Type != serve.EventJobRun || run.Job != "say" || run.Event == nil || run.Event.Type != "request.open" {
		t.Errorf("first = %+v, want say's job.run", run)
	}
	if end := nextOfTypes(t, s, serve.EventJobRun, serve.EventJobEnd); end.Type != serve.EventJobEnd || end.Job != "say" {
		t.Errorf("second = %+v, want say's job.end", end)
	}
	sent, err := b.client().ConversationSendAs(ctx, "from the job", "", 1, r.Token(), nil)
	if err != nil {
		t.Fatal(err)
	}
	runs = waitJobRuns(t, b, func(runs []jobrun.Run) bool { return len(runs) == 1 && len(runs[0].Messages) == 1 })
	if runs[0].Messages[0] != sent.MsgID {
		t.Errorf("Job Run messages %v, want %s", runs[0].Messages, sent.MsgID)
	}
	if hr, err := b.client().HookRuns(ctx); err != nil || len(hr) != 0 {
		t.Errorf("Hook runs = %+v, %v; want none", hr, err)
	}

	if v, err = b.client().JobRun(ctx, "say", nil); err != nil || !v.OK {
		t.Fatalf("job run say with no event = %+v, %v", v, err)
	}
	runs = waitJobRuns(t, b, func(runs []jobrun.Run) bool { return len(runs) == 2 && runs[0].State == jobrun.OK })
	if r := runs[0]; !r.ByHand || r.Event.Type != "" || r.Event.ConversationID != "" {
		t.Errorf("with no event, Job Run %+v", r)
	}
}

// The Sign Requests an exec Job proposes, accepts and refuses with its
// DEFIOS_JOB_RUN are recorded on that Job Run, each by its whole msg_id,
// apart from its talk messages; one proposed with a token naming no Job
// Run kept is proposed all the same.
func TestAJobRunRecordsWhatItsJobDidToSignRequests(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "tok")
	a, b := pairWith(t, func(n *node) {
		recordsStances(n)
		if n.name == "bob" {
			tune := n.tune
			n.tune = func(o *serve.Options) { tune(o); o.Getenv = os.Getenv }
		}
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	mintKey(t, a, b, gid)
	pk := bytes.Repeat([]byte{0x33}, 33)
	keyQuery := "ecdsa:" + hexOf(pk)
	conv := key.ConversationID("ecdsa", pk)
	waitMinted(t, a, gid, conv[:])
	waitMinted(t, b, gid, conv[:])
	setNetworks(t, a, "[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\n")
	writeConfig(t, b.paths.GroupConfigFile(group.ImplicitAlias), fmt.Sprintf(`
[networks.eth]
type = "ethereum"
chain_id = 1

[jobs.pay]
exec = ["sh", "-c", "echo \"$DEFIOS_JOB_RUN\" > \"$0\"", %q]
`, tokFile))
	ctx := context.Background()
	if v, err := b.client().JobRun(ctx, "pay", nil); err != nil || !v.OK {
		t.Fatalf("job run pay = %+v, %v", v, err)
	}
	r := waitJobRuns(t, b, func(runs []jobrun.Run) bool { return len(runs) == 1 && runs[0].State == jobrun.OK })[0]

	propose := func(n *node, reason, token string) string {
		t.Helper()
		v, err := n.client().SignPropose(ctx, keyQuery, signreq.Spec{Reason: reason, Transactions: payEth, Depth: 1}, token)
		if err != nil {
			t.Fatal(err)
		}
		waitSign(t, b, v.MsgID, func(signreq.Detail) bool { return true })
		return v.MsgID
	}
	mine := propose(b, "pay", r.Token())
	accepted, refused := propose(a, "pay one", ""), propose(a, "pay two", "")
	if _, err := b.client().SignAccept(ctx, accepted[:12], r.Token()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.client().SignRefuse(ctx, refused, "no", r.Token()); err != nil {
		t.Fatal(err)
	}
	runs := waitJobRuns(t, b, func(runs []jobrun.Run) bool { return len(runs) == 1 && len(runs[0].Signs) == 3 })
	want := []jobrun.Sign{{Act: jobrun.Propose, MsgID: mine}, {Act: jobrun.Accept, MsgID: accepted}, {Act: jobrun.Refuse, MsgID: refused}}
	if got := runs[0]; !slices.Equal(got.Signs, want) || len(got.Messages) != 0 {
		t.Errorf("Job Run signs %v, messages %v; want %v and none", got.Signs, got.Messages, want)
	}
	propose(b, "pay again", "9-1") // a token naming no Job Run kept
}
