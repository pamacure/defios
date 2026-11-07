package serve_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/serve"
	"defios.com/internal/trigger"
)

// nextOfTypes reads s until an event of one of types arrives.
func nextOfTypes(t *testing.T, s *serve.ChainStream, types ...string) serve.ChainEvent {
	t.Helper()
	for {
		ev, err := s.Next()
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for _, ty := range types {
			if ev.Type == ty {
				return ev
			}
		}
	}
}

// A Trigger's firing shows on its event's Conversation's stream as
// trigger.fire, with the event, and each Job it runs as job.end, ok or
// with the error it failed on. job run runs a Job, not a Trigger, on the
// event given or none, at its depth, and reports how it ended.
func TestTriggersAndJobsShowOnTheStream(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	a, b := pairWith(t, func(n *node) {
		if n.name != "bob" {
			return
		}
		n.tune = func(o *serve.Options) {
			o.Getenv = os.Getenv
			o.Config.Jobs = map[string]config.Job{
				"ok":   {Exec: []string{"true"}},
				"fail": {Exec: []string{"false"}},
			}
			o.Config.Hooks = map[string]config.Hook{
				"said": {Text: "^hi$", Jobs: []string{"ok", "fail"}},
			}
		}
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	s := watch(t, b)
	ctx := context.Background()
	if _, err := a.client().ConversationSend(ctx, "hi", nil); err != nil {
		t.Fatal(err)
	}
	fire := nextOfTypes(t, s, serve.EventTriggerFire, serve.EventJobEnd)
	if fire.Type != serve.EventTriggerFire || fire.Trigger != "said" || fire.Event == nil || fire.Event.Type != "app.freeform" || fire.Event.Text != "hi" {
		t.Fatalf("first = %+v, want said's trigger.fire on the message", fire)
	}
	if ev := nextOfTypes(t, s, serve.EventTriggerFire, serve.EventJobEnd); ev.Type != serve.EventJobEnd || ev.Job != "ok" || ev.OK == nil || !*ev.OK || ev.Error != "" {
		t.Fatalf("second = %+v, want ok's job.end", ev)
	}
	if ev := nextOfTypes(t, s, serve.EventTriggerFire, serve.EventJobEnd); ev.Type != serve.EventJobEnd || ev.Job != "fail" || ev.OK == nil || *ev.OK || !strings.Contains(ev.Error, "exit status 1") {
		t.Fatalf("third = %+v, want fail's job.end with its error", ev)
	}

	// job run reads the Jobs config.toml now holds, and runs one there, no
	// Trigger's: the event's depth, one deeper for what it writes.
	writeConfig(t, b.paths.GroupConfigFile(group.ImplicitAlias), fmt.Sprintf(`
[jobs.fail]
exec = ["false"]

[jobs.record]
exec = ["sh", "-c", "echo \"$DEFIOS_DEPTH:$DEFIOS_TRIGGER:$DEFIOS_EVENT\" >> \"$0\"", %q]
`, out))
	v, err := b.client().JobRun(ctx, "record", &trigger.Event{Type: "app.freeform", Depth: 2})
	if err != nil || !v.OK || v.Job != "record" {
		t.Fatalf("job run record = %+v, %v", v, err)
	}
	if got := strings.TrimSpace(readFile(t, out)); got != "3::app.freeform" {
		t.Errorf("record wrote %q, want depth 3, no Trigger", got)
	}
	if ev := nextOfTypes(t, s, serve.EventTriggerFire, serve.EventJobEnd); ev.Type != serve.EventJobEnd || ev.Job != "record" || !*ev.OK {
		t.Errorf("after job run = %+v, want record's job.end alone", ev)
	}
	if v, err = b.client().JobRun(ctx, "record", nil); err != nil || !v.OK {
		t.Fatalf("job run record with no event = %+v, %v", v, err)
	}
	if got := readLines(t, out); len(got) != 2 || got[1] != "1::" {
		t.Errorf("record wrote %v, want depth 1 with no event", got)
	}
	if v, err = b.client().JobRun(ctx, "fail", nil); err != nil || v.OK || !strings.Contains(v.Error, "exit status 1") {
		t.Errorf("job run fail = %+v, %v; want it failed", v, err)
	}
	if _, err = b.client().JobRun(ctx, "missing", nil); err == nil || !strings.Contains(err.Error(), "no [jobs.missing]") {
		t.Errorf("job run missing: %v", err)
	}
}
