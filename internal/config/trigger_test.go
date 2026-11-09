package config_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"defios.com/internal/config"
)

// A Group's config.toml defines Jobs and the Hooks that run them (ADR 0032):
// each Job has exactly one of agent, exec or webhook, and a Hook's filters
// read as written, from and conversation as one name or a list.
func TestAGroupConfigReadsJobsAndHooks(t *testing.T) {
	c, err := config.ReadFile(write(t, `
[jobs.make_sign_decision]
agent = "auditor"
prompt = "Review this Sign Request."

[jobs.notify]
webhook = "$NTFY_URL"
headers = { Authorization = "$NTFY_TOKEN", "X-Title" = "defios" }
timeout = "5s"

[jobs.archive]
exec = ["archive-event", "--to", "$DEFIOS_GROUP"]
secrets = ["ARCHIVE_TOKEN"]

[hooks.review_requests]
events = ["sign.request", "request.expired"]
from = "others"
handles = ["user", "auditor"]
conversation = ["group", "ecdsa-02a1b2c3"]
text = "(?i)urgent"
jobs = ["notify", "make_sign_decision"]

[hooks.everything]
jobs = ["archive"]
`))
	if err != nil {
		t.Fatal(err)
	}
	wantJobs := map[string]config.Job{
		"make_sign_decision": {Agent: "auditor", Prompt: "Review this Sign Request."},
		"notify":             {Webhook: "$NTFY_URL", Headers: map[string]string{"Authorization": "$NTFY_TOKEN", "X-Title": "defios"}, Timeout: config.Age(5 * time.Second)},
		"archive":            {Exec: []string{"archive-event", "--to", "$DEFIOS_GROUP"}, Secrets: []string{"ARCHIVE_TOKEN"}},
	}
	if !reflect.DeepEqual(c.Jobs, wantJobs) {
		t.Errorf("jobs %+v, want %+v", c.Jobs, wantJobs)
	}
	wantHooks := map[string]config.Hook{
		"review_requests": {
			Events: []string{"sign.request", "request.expired"}, From: config.Names{"others"},
			Handles: []string{"user", "auditor"}, Conversation: config.Names{"group", "ecdsa-02a1b2c3"},
			Text: "(?i)urgent", Jobs: []string{"notify", "make_sign_decision"},
		},
		"everything": {Jobs: []string{"archive"}},
	}
	if !reflect.DeepEqual(c.Hooks, wantHooks) {
		t.Errorf("hooks %+v, want %+v", c.Hooks, wantHooks)
	}
	if got := c.Jobs["archive"].ExecTimeout(); got != 60*time.Second {
		t.Errorf("exec timeout %v, want the 60s default", got)
	}
	if got := c.Jobs["notify"].WebhookTimeout(); got != 5*time.Second {
		t.Errorf("webhook timeout %v, want the 5s set", got)
	}
	if got := (config.Job{Webhook: "https://x.example"}).WebhookTimeout(); got != 10*time.Second {
		t.Errorf("webhook timeout %v, want the 10s default", got)
	}
}

// Jobs and Hooks round-trip through WriteFile, as group new writes them.
func TestJobsAndHooksRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := config.Config{
		DisplayName: "laptop", Hostname: "defios-a",
		Jobs:  map[string]config.Job{"sign_accept": {Exec: []string{"defios", "sign", "accept", "$DEFIOS_SIGN_REQUEST_ID"}}},
		Hooks: map[string]config.Hook{"h": {Events: []string{"app.freeform"}, From: config.Names{"self"}, Jobs: []string{"sign_accept"}}},
	}
	if err := config.WriteFile(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := config.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("read %+v, want %+v", out, in)
	}
}

// A malformed Job or Hook stops the file reading, as any config error does.
func TestConfigRefusesMalformedJobsAndHooks(t *testing.T) {
	for name, body := range map[string]string{
		"job with no kind":             "[jobs.j]\ntimeout = \"1s\"",
		"job with two kinds":           "[jobs.j]\nexec = [\"true\"]\nwebhook = \"https://x.example\"",
		"bad job name":                 "[jobs.Notify]\nexec = [\"true\"]",
		"empty exec":                   "[jobs.j]\nexec = []",
		"exec with an empty program":   "[jobs.j]\nexec = [\"\"]",
		"bad agent handle":             "[jobs.j]\nagent = \"Auditor\"",
		"prompt on an exec job":        "[jobs.j]\nexec = [\"true\"]\nprompt = \"x\"",
		"secrets on a webhook":         "[jobs.j]\nwebhook = \"https://x.example\"\nsecrets = [\"A\"]",
		"headers on an exec job":       "[jobs.j]\nexec = [\"true\"]\nheaders = { A = \"b\" }",
		"timeout on an agent job":      "[jobs.j]\nagent = \"auditor\"\ntimeout = \"1s\"",
		"group agent not in profiles":  "[agent]\nprofiles = [\"scribe\"]\n[jobs.j]\nagent = \"auditor\"\ntarget = \"group\"",
		"group agent with no profiles": "[jobs.j]\nagent = \"auditor\"\ntarget = \"group\"",
		"bad secret name":              "[jobs.j]\nexec = [\"true\"]\nsecrets = [\"lower\"]",
		"webhook not a url":            "[jobs.j]\nwebhook = \"ftp://x.example\"",
		"webhook bad reference":        "[jobs.j]\nwebhook = \"$lower\"",
		"header bad reference":         "[jobs.j]\nwebhook = \"https://x.example\"\nheaders = { A = \"$lower\" }",
		"zero timeout":                 "[jobs.j]\nexec = [\"true\"]\ntimeout = \"0s\"",
		"unknown job key":              "[jobs.j]\nexec = [\"true\"]\nshell = true",
		"hook with no jobs":            "[hooks.h]\nevents = [\"app.freeform\"]",
		"hook naming no job":           "[hooks.h]\njobs = [\"missing\"]",
		"hook naming a job twice":      "[jobs.j]\nexec = [\"true\"]\n[hooks.h]\njobs = [\"j\", \"j\"]",
		"bad hook name":                "[jobs.j]\nexec = [\"true\"]\n[hooks.H]\njobs = [\"j\"]",
		"unknown event":                "[jobs.j]\nexec = [\"true\"]\n[hooks.h]\nevents = [\"app.recap\"]\njobs = [\"j\"]",
		"unknown status":               "[jobs.j]\nexec = [\"true\"]\n[hooks.h]\nevents = [\"request.lost\"]\njobs = [\"j\"]",
		"bad from":                     "[jobs.j]\nexec = [\"true\"]\n[hooks.h]\nfrom = \"me\"\njobs = [\"j\"]",
		"bad handle":                   "[jobs.j]\nexec = [\"true\"]\n[hooks.h]\nhandles = [\"Auditor\"]\njobs = [\"j\"]",
		"bad conversation":             "[jobs.j]\nexec = [\"true\"]\n[hooks.h]\nconversation = \"../x\"\njobs = [\"j\"]",
		"hook naming a key alias":      "[jobs.j]\nexec = [\"true\"]\n[hooks.h]\nconversation = \"Treasury\"\njobs = [\"j\"]",
		"job targeting a key alias":    "[agent]\nprofiles = [\"auditor\"]\n[jobs.j]\nagent = \"auditor\"\ntarget = \"Treasury\"",
		"bad text":                     "[jobs.j]\nexec = [\"true\"]\n[hooks.h]\ntext = \"(\"\njobs = [\"j\"]",
		"from not a string":            "[jobs.j]\nexec = [\"true\"]\n[hooks.h]\nfrom = 1\njobs = [\"j\"]",
		"unknown hook key":             "[jobs.j]\nexec = [\"true\"]\n[hooks.h]\njobs = [\"j\"]\nwhen = \"x\"",
	} {
		if _, err := config.ReadFile(write(t, body)); err == nil {
			t.Errorf("%s: read without error", name)
		}
	}
	// An agent Job in Group-level talk names a Handle the Group includes.
	if _, err := config.ReadFile(write(t, "[agent]\nprofiles = [\"auditor\"]\n[jobs.j]\nagent = \"auditor\"\ntarget = \"group\"")); err != nil {
		t.Errorf("an included Handle: %v", err)
	}
	// Jobs and Hooks are the Group's: a Conversation's config.toml may not
	// hold them.
	for _, body := range []string{"[jobs.j]\nexec = [\"true\"]", "[hooks.h]\njobs = [\"j\"]"} {
		if _, err := config.ReadWorkspace(write(t, body)); err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("Conversation %q: %v, want an unknown key", body, err)
		}
	}
}

// A Schedule fires its Jobs on a cron spec, read in its tz or the Node's
// local zone, or once at a time (ADR 0032).
func TestAGroupConfigReadsSchedules(t *testing.T) {
	c, err := config.ReadFile(write(t, `
[agent]
profiles = ["auditor"]

[jobs.daily_summary]
agent = "auditor"
target = "group"
prompt = "Summarise yesterday's talk."

[jobs.ping]
exec = ["true"]

[schedules.morning]
cron = "0 9 * * 1-5"
tz = "Europe/Paris"
jobs = ["daily_summary", "ping"]

[schedules.launch]
at = "2026-12-01T09:00:00Z"
jobs = ["ping"]
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]config.Schedule{
		"morning": {Cron: "0 9 * * 1-5", TZ: "Europe/Paris", Jobs: []string{"daily_summary", "ping"}},
		"launch":  {At: "2026-12-01T09:00:00Z", Jobs: []string{"ping"}},
	}
	if !reflect.DeepEqual(c.Schedules, want) {
		t.Fatalf("schedules %+v, want %+v", c.Schedules, want)
	}
	friday := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC) // 9:00 in Paris
	if got, want := c.Schedules["morning"].Next(friday), time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("morning after Friday 9:00 Paris: %s, want Monday's %s", got.UTC(), want)
	}
	launch := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	if got := c.Schedules["launch"].Next(friday); !got.Equal(launch) {
		t.Errorf("launch: %s, want %s", got, launch)
	}
	if got := c.Schedules["launch"].Next(launch); !got.IsZero() {
		t.Errorf("launch after it fired: %s, want none", got)
	}
	// Unset, tz is the Node's local zone.
	local := config.Schedule{Cron: "0 9 * * *", Jobs: []string{"ping"}}
	if got := local.Next(friday).In(time.Local); got.Hour() != 9 || got.Minute() != 0 {
		t.Errorf("no tz: %s, want 9:00 local", got.In(time.Local))
	}
}

// A malformed Schedule stops the file reading.
func TestConfigRefusesMalformedSchedules(t *testing.T) {
	const ping = "[jobs.ping]\nexec = [\"true\"]\n"
	for name, body := range map[string]string{
		"no time":               ping + "[schedules.s]\njobs = [\"ping\"]",
		"cron and at":           ping + "[schedules.s]\ncron = \"* * * * *\"\nat = \"2026-12-01T09:00:00Z\"\njobs = [\"ping\"]",
		"bad cron":              ping + "[schedules.s]\ncron = \"0 9 * *\"\njobs = [\"ping\"]",
		"cron that never fires": ping + "[schedules.s]\ncron = \"0 0 30 2 *\"\njobs = [\"ping\"]",
		"at with no zone":       ping + "[schedules.s]\nat = \"2026-12-01T09:00:00\"\njobs = [\"ping\"]",
		"tz with at":            ping + "[schedules.s]\nat = \"2026-12-01T09:00:00Z\"\ntz = \"UTC\"\njobs = [\"ping\"]",
		"unknown tz":            ping + "[schedules.s]\ncron = \"* * * * *\"\ntz = \"Mars/Olympus\"\njobs = [\"ping\"]",
		"no jobs":               ping + "[schedules.s]\ncron = \"* * * * *\"",
		"naming no job":         ping + "[schedules.s]\ncron = \"* * * * *\"\njobs = [\"missing\"]",
		"naming a job twice":    ping + "[schedules.s]\ncron = \"* * * * *\"\njobs = [\"ping\", \"ping\"]",
		"bad name":              ping + "[schedules.Morning]\ncron = \"* * * * *\"\njobs = [\"ping\"]",
		"unknown key":           ping + "[schedules.s]\ncron = \"* * * * *\"\njobs = [\"ping\"]\nevents = [\"app.freeform\"]",
		// An agent Job a Schedule runs has no event to take a Conversation
		// from, so names its target.
		"agent job with no target": "[jobs.a]\nagent = \"auditor\"\n[schedules.s]\ncron = \"* * * * *\"\njobs = [\"a\"]",
	} {
		if _, err := config.ReadFile(write(t, body)); err == nil {
			t.Errorf("%s: read without error", name)
		}
	}
	if _, err := config.ReadWorkspace(write(t, ping+"[schedules.s]\ncron = \"* * * * *\"\njobs = [\"ping\"]")); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("a Conversation's Schedule: %v, want an unknown key", err)
	}
}
