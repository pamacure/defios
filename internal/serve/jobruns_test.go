package serve_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
