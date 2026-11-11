package node_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/node"
	"defios.com/internal/nodedir"
)

// triggersTOML is a Group config.toml written by hand, with comments.
const triggersTOML = `# alpha, by hand
headscale_url = "https://hs.example"
display_name = "laptop"
hostname = "laptop-alpha"

# Ping someone.
[jobs.ping]
exec = ["true"] # does nothing

[jobs.notify]
webhook = "$NTFY_URL"

[jobs.notify.headers]
Title = "defios"

# Hooks below.
[hooks.everything]
jobs = ["ping"]

[schedules.launch]
at = "2099-12-01T09:00:00Z"
jobs = ["ping"]
`

// triggerNode is nodeDir with alpha's config.toml replaced by triggersTOML.
func triggerNode(t *testing.T) (nodedir.Paths, *node.Host) {
	t.Helper()
	p := nodeDir(t)
	if err := os.WriteFile(p.GroupConfigFile("alpha"), []byte(triggersTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := node.Open(p, node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p, h
}

func readConfig(t *testing.T, p nodedir.Paths) string {
	t.Helper()
	b, err := os.ReadFile(p.GroupConfigFile("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Jobs, Hooks and Schedules are listed by name, a Schedule with its next
// firing, with the Group not served.
func TestTriggersAreListed(t *testing.T) {
	_, h := triggerNode(t)
	ctx := context.Background()

	jobs, err := h.Jobs(ctx, "alpha")
	if err != nil || len(jobs) != 2 || jobs[0].Name != "notify" || jobs[1].Name != "ping" ||
		jobs[0].Webhook != "$NTFY_URL" || jobs[0].Headers["Title"] != "defios" || jobs[1].Exec[0] != "true" {
		t.Fatalf("jobs: %#v, %v", jobs, err)
	}
	hooks, err := h.Hooks(ctx, "alpha")
	if err != nil || len(hooks) != 1 || hooks[0].Name != "everything" || !reflect.DeepEqual(hooks[0].Jobs, []string{"ping"}) {
		t.Fatalf("hooks: %#v, %v", hooks, err)
	}
	scheds, err := h.Schedules(ctx, "alpha")
	want := time.Date(2099, 12, 1, 9, 0, 0, 0, time.UTC)
	if err != nil || len(scheds) != 1 || scheds[0].Name != "launch" || !scheds[0].Next.Equal(want) {
		t.Fatalf("schedules: %#v, %v", scheds, err)
	}
	if _, err := h.Jobs(ctx, "nope"); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("an unknown Group: %v; want ErrUnknownGroup", err)
	}
}

// Jobs, Hooks and Schedules are added, replaced and deleted in place:
// the rest of config.toml, comments included, is left as it was.
func TestTriggerWritesKeepTheRestOfConfigTOML(t *testing.T) {
	p, h := triggerNode(t)
	ctx := context.Background()

	if err := h.AddJob(ctx, "alpha", "alert", config.Job{Exec: []string{"notify-send", "sign request"}, Timeout: config.Age(5 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := h.AddHook(ctx, "alpha", "requests", config.Hook{Events: []string{"sign.request"}, From: config.Names{"others"}, Jobs: []string{"alert", "ping"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.AddSchedule(ctx, "alpha", "daily", config.Schedule{Cron: "0 9 * * *", TZ: "Europe/Paris", Jobs: []string{"alert"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.EditJob(ctx, "alpha", "notify", config.Job{Webhook: "https://ntfy.example/x"}); err != nil {
		t.Fatal(err)
	}
	if err := h.DeleteHook(ctx, "alpha", "everything"); err != nil {
		t.Fatal(err)
	}
	if err := h.EditSchedule(ctx, "alpha", "launch", config.Schedule{At: "2099-12-02T09:00:00Z", Jobs: []string{"notify"}}); err != nil {
		t.Fatal(err)
	}

	got := readConfig(t, p)
	for _, kept := range []string{
		"# alpha, by hand\nheadscale_url = \"https://hs.example\"\ndisplay_name = \"laptop\"\nhostname = \"laptop-alpha\"\n",
		"# Ping someone.\n[jobs.ping]\nexec = [\"true\"] # does nothing\n",
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("config.toml lost %q:\n%s", kept, got)
		}
	}
	for _, gone := range []string{"[hooks.everything]", "# Hooks below.", "$NTFY_URL", "[jobs.notify.headers]", "2099-12-01"} {
		if strings.Contains(got, gone) {
			t.Errorf("config.toml still holds %q:\n%s", gone, got)
		}
	}

	jobs, err := h.Jobs(ctx, "alpha")
	if err != nil || len(jobs) != 3 || jobs[0].Name != "alert" || jobs[0].Timeout != config.Age(5*time.Second) ||
		jobs[1].Webhook != "https://ntfy.example/x" || jobs[1].Headers != nil {
		t.Fatalf("jobs: %#v, %v", jobs, err)
	}
	hooks, err := h.Hooks(ctx, "alpha")
	if err != nil || len(hooks) != 1 || hooks[0].Name != "requests" || !reflect.DeepEqual(hooks[0].From, config.Names{"others"}) {
		t.Fatalf("hooks: %#v, %v", hooks, err)
	}
	scheds, err := h.Schedules(ctx, "alpha")
	if err != nil || len(scheds) != 2 || scheds[0].Name != "daily" || scheds[0].Next.IsZero() || scheds[1].At != "2099-12-02T09:00:00Z" {
		t.Fatalf("schedules: %#v, %v", scheds, err)
	}
}

// A write the config loader would refuse writes nothing: a Trigger naming
// no Job, a Job a Trigger still names, an agent Job naming no Agent
// Profile, a name already defined or not defined.
func TestTriggerWritesAreValidated(t *testing.T) {
	p, h := triggerNode(t)
	ctx := context.Background()
	before := readConfig(t, p)

	invalid := func(what string, err error) {
		t.Helper()
		if !errors.As(err, new(*node.InvalidError)) {
			t.Errorf("%s: %v; want InvalidError", what, err)
		}
	}
	invalid("a Hook naming no Job", h.AddHook(ctx, "alpha", "x", config.Hook{Jobs: []string{"ghost"}}))
	invalid("a Schedule naming no Job", h.AddSchedule(ctx, "alpha", "x", config.Schedule{Cron: "0 9 * * *", Jobs: []string{"ghost"}}))
	invalid("a Schedule with no time", h.AddSchedule(ctx, "alpha", "x", config.Schedule{Jobs: []string{"ping"}}))
	invalid("deleting a Job a Hook names", h.DeleteJob(ctx, "alpha", "ping"))
	invalid("a Job of two kinds", h.AddJob(ctx, "alpha", "x", config.Job{Exec: []string{"true"}, Webhook: "https://x.example"}))
	invalid("a bad name", h.AddJob(ctx, "alpha", "Bad Name", config.Job{Exec: []string{"true"}}))
	invalid("an agent Job naming no Agent Profile", h.AddJob(ctx, "alpha", "x", config.Job{Agent: "ghost", Target: "group"}))

	if err := h.AddJob(ctx, "alpha", "ping", config.Job{Exec: []string{"true"}}); !errors.Is(err, node.ErrAlreadyDefined) {
		t.Errorf("adding ping again: %v; want ErrAlreadyDefined", err)
	}
	if err := h.EditHook(ctx, "alpha", "ghost", config.Hook{Jobs: []string{"ping"}}); !errors.Is(err, node.ErrNotDefined) {
		t.Errorf("editing a ghost Hook: %v; want ErrNotDefined", err)
	}
	if err := h.DeleteSchedule(ctx, "alpha", "ghost"); !errors.Is(err, node.ErrNotDefined) {
		t.Errorf("deleting a ghost Schedule: %v; want ErrNotDefined", err)
	}
	if got := readConfig(t, p); got != before {
		t.Fatalf("a refused write changed config.toml:\n%s", got)
	}

	// An agent Job naming an Agent Profile of this Node's is written.
	if _, err := h.AddAgentProfile(ctx, "auditor", node.ProfileEdit{Provider: ptr("claude")}); err != nil {
		t.Fatal(err)
	}
	if err := h.AddJob(ctx, "alpha", "audit", config.Job{Agent: "auditor", Prompt: "audit it"}); err != nil {
		t.Fatalf("an agent Job: %v", err)
	}
}

// config.toml is read and written whole as text; a text that does not
// load is refused and writes nothing.
func TestGroupConfigTOMLIsReadAndWrittenAsText(t *testing.T) {
	p, h := triggerNode(t)
	ctx := context.Background()

	got, err := h.GroupConfigTOML(ctx, "alpha")
	if err != nil || got != triggersTOML {
		t.Fatalf("read %q, %v", got, err)
	}
	next := strings.Replace(triggersTOML, "# Hooks below.\n[hooks.everything]\njobs = [\"ping\"]\n", "", 1)
	if err := h.SetGroupConfigTOML(ctx, "alpha", next); err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t, p); got != next {
		t.Fatalf("wrote:\n%s\nwant:\n%s", got, next)
	}
	for _, bad := range []string{
		"headscale_url = ",                         // not TOML
		next + "\n[hooks.x]\njobs = [\"ghost\"]\n", // names no Job
	} {
		if err := h.SetGroupConfigTOML(ctx, "alpha", bad); !errors.As(err, new(*node.InvalidError)) {
			t.Errorf("writing %q: %v; want InvalidError", bad, err)
		}
	}
	if got := readConfig(t, p); got != next {
		t.Fatalf("a refused write changed config.toml:\n%s", got)
	}
	if _, err := h.GroupConfigTOML(ctx, "nope"); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("an unknown Group: %v; want ErrUnknownGroup", err)
	}
}

// A table config.toml defines inline is not changed in place: that is a
// text edit's, and the write leaves the file as it was. What only looks
// like a header, inside a multi-line string, is no table.
func TestAJobDefinedInlineIsNotChangedInPlace(t *testing.T) {
	p, h := triggerNode(t)
	ctx := context.Background()
	text := triggersTOML + `
[jobs.say]
exec = ["echo", """
[jobs.ping]
exec = ["false"]
"""]

[jobs.inline]
x = 1
`
	text = strings.Replace(text, "[jobs.inline]\nx = 1\n", "[jobs.more]\nexec = [\"true\"]\n", 1)
	text = strings.Replace(text, "[jobs.ping]\nexec = [\"true\"] # does nothing\n", "", 1)
	text = strings.Replace(text, "# alpha, by hand\n", "# alpha, by hand\njobs.ping = { exec = [\"true\"] }\n", 1)
	if err := h.SetGroupConfigTOML(ctx, "alpha", text); err != nil {
		t.Fatal(err)
	}
	if err := h.EditJob(ctx, "alpha", "ping", config.Job{Exec: []string{"false"}}); !errors.Is(err, config.ErrNotInPlace) {
		t.Fatalf("editing an inline Job: %v; want config.ErrNotInPlace", err)
	}
	if got := readConfig(t, p); got != text {
		t.Fatalf("a refused write changed config.toml:\n%s", got)
	}
	if err := h.EditJob(ctx, "alpha", "more", config.Job{Exec: []string{"false"}}); err != nil {
		t.Fatalf("editing the Job after the multi-line string: %v", err)
	}
	jobs, err := h.Jobs(ctx, "alpha")
	if err != nil || len(jobs) != 4 || jobs[0].Name != "more" || jobs[0].Exec[0] != "false" || jobs[3].Name != "say" || len(jobs[3].Exec) != 2 {
		t.Fatalf("jobs: %#v, %v", jobs, err)
	}
}

// RunJob needs the Group served.
func TestRunJobNeedsTheGroupServed(t *testing.T) {
	_, h := triggerNode(t)
	if _, err := h.RunJob(context.Background(), "alpha", "ping", nil); !errors.Is(err, node.ErrNotServed) {
		t.Fatalf("RunJob with nothing served: %v; want ErrNotServed", err)
	}
}
