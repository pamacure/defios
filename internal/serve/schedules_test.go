package serve_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/jobrun"
	"defios.com/internal/serve"
)

// A Schedule fires its Jobs, in order, at a time that passes while serve
// serves its Group: an exec Job at depth 1, and an agent Job running a
// chain in its target that answers at depth 1. One whose time passed
// before serve took it up never fires: it is not made up.
func TestAScheduleFiresLiveOnly(t *testing.T) {
	var logs logLines
	a, b, gid, provider := agentPairTuned(t, func(o *serve.Options) {
		o.Getenv = os.Getenv
		o.Logf = logs.logf
	}, "auditor")
	replyAs(provider, map[string]string{"auditor": "summed up"})
	out, missed := filepath.Join(t.TempDir(), "out"), filepath.Join(t.TempDir(), "missed")
	s := watch(t, a)
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	soon := time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339)
	writeConfig(t, path, readFile(t, path)+fmt.Sprintf(`
[jobs.record]
exec = ["sh", "-c", "echo \"$DEFIOS_EVENT:$DEFIOS_DEPTH:$DEFIOS_TRIGGER\" >> \"$0\"; grep -o '\"time\":\"[^\"]*\"' >> \"$0\"", %q]

[jobs.summary]
agent = "auditor"
target = "group"
prompt = "Summarise the day."

[jobs.missed]
exec = ["touch", %q]

[schedules.soon]
at = %q
jobs = ["record", "summary"]

[schedules.past]
at = %q
jobs = ["missed"]
`, out, missed, soon, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)))
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	waitLines(t, out, []string{"schedule:1:soon", fmt.Sprintf(`"time":"%s"`, soon)})
	events := nextChain(t, s)
	if got := summary(events); !strings.HasPrefix(got, "chain.start start:@auditor/1 ") || !strings.HasSuffix(got, "commit:summed up end") {
		t.Fatalf("events = %s", got)
	}
	last := provider.seen()[len(provider.seen())-1].Turns
	last = last[strings.LastIndex(last, "[user] "):]
	for _, want := range []string{"Summarise the day.", `"type":"schedule"`, `"trigger":"soon"`} {
		if !strings.Contains(last, want) {
			t.Errorf("last user turn lacks %q: %q", want, last)
		}
	}
	for _, ff := range freeforms(t, b.groupLevelRows(t, gid, 1)) {
		if ff.Handle == "auditor" && ff.Depth != 1 {
			t.Errorf("the reply is at depth %d, want 1", ff.Depth)
		}
	}
	waitLog(t, &logs, "schedule soon: job summary done")
	// Each Job keeps a Job Run; the agent Job's names where its run is.
	runs := waitJobRuns(t, a, func(runs []jobrun.Run) bool { return len(runs) == 2 && runs[0].State == jobrun.OK })
	rec, sum := runs[1], runs[0]
	if rec.Trigger != "soon" || rec.Event.Type != "schedule" || rec.Job != "record" || rec.State != jobrun.OK || rec.Exec == nil {
		t.Errorf("record = %+v", rec)
	}
	if sum.Kind != "agent" || sum.Handle != "auditor" || sum.Chain == "" || sum.Conversation != "" || sum.Firing != rec.ID {
		t.Errorf("summary = %+v", sum)
	}
	if _, err := os.Stat(missed); err == nil {
		t.Error("a Schedule whose time had passed fired")
	}
}
