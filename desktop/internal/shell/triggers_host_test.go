package shell_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
)

func triggersOK(t *testing.T, what string, v shell.TriggersView) shell.TriggersView {
	t.Helper()
	if v.Problem != nil || !v.Saved {
		t.Fatalf("%s: %+v (where %q, field %q)", what, v.Problem, v.Where, v.Field)
	}
	return v
}

// cliList is the CLI's `<verb> list --json` for the Group home, decoded.
func cliList(t *testing.T, dir, verb string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal([]byte(defiosCLI(t, dir, verb, "list", "--json", "--group", "home")), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// named is the entry of list named name, nil for none.
func named(list []map[string]any, name string) map[string]any {
	for _, e := range list {
		if e["name"] == name {
			return e
		}
	}
	return nil
}

// A Job, a Hook on Sign Requests running it and a Schedule, made through
// the forms, are what the CLI lists; the comments of config.toml stay,
// and Run now runs the Job, its outcome a system line in Talk.
func TestTriggersMadeInTheAppAreWhatTheCLIListsAndRun(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _, ap, _ := formedAt(t)

	tv := alice.TriggersTOML()
	if tv.Problem != nil || tv.Group != "home" {
		t.Fatalf("toml: %+v", tv)
	}
	tv = alice.SaveTriggersTOML("# kept by hand\n" + tv.Text + "\n# and at the end\n")
	if tv.Problem != nil || !tv.Saved {
		t.Fatalf("save toml: %+v", tv.Problem)
	}

	v := triggersOK(t, "add job", alice.SaveJob(shell.JobDraft{New: true, Name: "ping", Kind: "exec", Exec: []string{"true"}, Timeout: "5s"}))
	if j := jobNamed(v, "ping"); j == nil || j.Kind != "exec" || !reflect.DeepEqual(j.Exec, []string{"true"}) || j.Timeout != "5s" {
		t.Fatalf("ping: %+v", v.Jobs)
	}
	triggersOK(t, "add webhook", alice.SaveJob(shell.JobDraft{New: true, Name: "notify", Kind: "webhook", WebhookSecret: "NTFY_URL",
		Headers: []shell.Header{{Name: "Title", Value: "defios"}, {Name: "Authorization", Secret: "NTFY_TOKEN"}}}))
	v = triggersOK(t, "add hook", alice.SaveHook(shell.HookDraft{New: true, Name: "requests", Events: []string{"sign.request"}, From: []string{"others"}, Jobs: []string{"ping", "notify"}}))
	if len(v.Hooks) != 1 || !reflect.DeepEqual(v.Hooks[0].Jobs, []string{"ping", "notify"}) {
		t.Fatalf("hooks: %+v", v.Hooks)
	}
	v = triggersOK(t, "add schedule", alice.SaveSchedule(shell.ScheduleDraft{New: true, Name: "daily", Cron: "0 9 * * *", TZ: "UTC", Jobs: []string{"ping"}}))
	if len(v.Schedules) != 1 || v.Schedules[0].Next == nil || v.Schedules[0].Next.Hour() != 9 || !v.Schedules[0].Next.After(time.Now()) {
		t.Fatalf("schedules: %+v", v.Schedules)
	}
	if j := jobNamed(v, "ping"); !reflect.DeepEqual(j.UsedBy, []string{"Hook requests", "Schedule daily"}) {
		t.Fatalf("ping is used by %v", j.UsedBy)
	}
	if !contains(v.Events, "sign.request") || !contains(v.Events, "request.signed") {
		t.Fatalf("events: %v", v.Events)
	}

	jobs := cliList(t, ap.Config, "job")
	if ping := named(jobs, "ping"); ping == nil || ping["kind"] != "exec" || ping["timeout"] != "5s" ||
		!reflect.DeepEqual(ping["exec"], []any{"true"}) {
		t.Fatalf("job list: %v", jobs)
	}
	if notify := named(jobs, "notify"); notify == nil || notify["kind"] != "webhook" || notify["webhook"] != "$NTFY_URL" {
		t.Fatalf("job list: %v", jobs)
	}
	hooks := cliList(t, ap.Config, "hook")
	if len(hooks) != 1 || !reflect.DeepEqual(hooks[0], map[string]any{
		"name": "requests", "events": []any{"sign.request"}, "from": []any{"others"}, "jobs": []any{"ping", "notify"},
	}) {
		t.Fatalf("hook list: %v", hooks)
	}
	scheds := cliList(t, ap.Config, "schedule")
	if len(scheds) != 1 || scheds[0]["name"] != "daily" || scheds[0]["cron"] != "0 9 * * *" || scheds[0]["tz"] != "UTC" {
		t.Fatalf("schedule list: %v", scheds)
	}

	// An edit, then a delete, keep the comments.
	triggersOK(t, "edit job", alice.SaveJob(shell.JobDraft{Name: "ping", Kind: "exec", Exec: []string{"true", "again"}}))
	triggersOK(t, "delete schedule", alice.DeleteSchedule("daily"))
	text := alice.TriggersTOML().Text
	for _, kept := range []string{"# kept by hand\n", "# and at the end\n", `exec = ["true", "again"]`} {
		if !strings.Contains(text, kept) {
			t.Fatalf("config.toml lost %q:\n%s", kept, text)
		}
	}

	ran := alice.RunJob("ping")
	if ran.Problem != nil || ran.Ran == nil || !ran.Ran.OK {
		t.Fatalf("run: %+v %+v", ran.Problem, ran.Ran)
	}
	talkWithin(t, alice, 10*time.Second, hasText("Job ping finished."))
}

// A form refuses what only a text edit can change, a Job a Trigger runs,
// and a name already taken; Edit TOML refuses text that does not load.
// None writes anything. Run now needs the Group served.
func TestTriggerRefusalsWriteNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _ := formed(t)

	inline := alice.TriggersTOML().Text + "\n[hooks]\nall = { jobs = [\"sign_accept\"] }\n"
	if tv := alice.SaveTriggersTOML(inline); tv.Problem != nil {
		t.Fatalf("save toml: %+v", tv.Problem)
	}
	v := alice.SaveHook(shell.HookDraft{Name: "all", Jobs: []string{"sign_refuse"}})
	if v.Problem == nil || v.Problem.Code != "not-in-place" || v.Table != "hooks.all" || v.Where != "hook:all" {
		t.Fatalf("an inline table: %+v table %q where %q", v.Problem, v.Table, v.Where)
	}
	// A new Hook beside the inline one is still written.
	triggersOK(t, "add beside an inline table", alice.SaveHook(shell.HookDraft{New: true, Name: "more", Jobs: []string{"sign_refuse"}}))
	v = alice.DeleteJob("sign_accept")
	if v.Problem == nil || v.Problem.Code != "job-in-use" || !strings.Contains(v.Problem.Message, "Hook all") {
		t.Fatalf("a Job in use: %+v", v.Problem)
	}
	v = alice.SaveJob(shell.JobDraft{New: true, Name: "sign_accept", Kind: "exec", Exec: []string{"true"}})
	if v.Problem == nil || v.Problem.Code != "already-defined" || v.Field != "name" {
		t.Fatalf("a name taken: %+v field %q", v.Problem, v.Field)
	}
	v = alice.SaveJob(shell.JobDraft{New: true, Name: "empty", Kind: "exec"})
	if v.Problem == nil || v.Field != "exec" {
		t.Fatalf("no program: %+v field %q", v.Problem, v.Field)
	}
	v = alice.SaveJob(shell.JobDraft{New: true, Name: "slow", Kind: "exec", Exec: []string{"true"}, Timeout: "soon"})
	if v.Problem == nil || v.Field != "timeout" {
		t.Fatalf("a bad timeout: %+v field %q", v.Problem, v.Field)
	}
	before := alice.TriggersTOML().Text
	if tv := alice.SaveTriggersTOML(before + "\n[hooks.bad]\njobs = [\"ghost\"]\n"); tv.Problem == nil || tv.Saved {
		t.Fatalf("a Hook naming no Job: %+v", tv)
	}
	if tv := alice.SaveTriggersTOML("not = [toml"); tv.Problem == nil || tv.Saved {
		t.Fatalf("not TOML: %+v", tv)
	}
	if after := alice.TriggersTOML().Text; after != before {
		t.Fatalf("a refusal wrote:\n%s", after)
	}

	// A Group not served lists its Triggers, and Run now says why not.
	if st := alice.AddGroup("work", "https://hs.example", "tskey-auth-work", ""); st.Problem != nil {
		t.Fatalf("add group: %+v", st.Problem)
	}
	if st := alice.Switch("home"); st.Served != "home" {
		t.Fatalf("back to home: %+v", st)
	}
	alice.SetEditGroup("work")
	if v := alice.Triggers(); v.Group != "work" || v.Served || v.ListProblem != nil {
		t.Fatalf("work's: %+v", v)
	}
	if v := alice.RunJob("sign_accept"); v.Problem == nil || v.Problem.Code != "not-served" || v.Ran != nil {
		t.Fatalf("run in a Group not served: %+v", v)
	}
}

func jobNamed(v shell.TriggersView, name string) *shell.JobView {
	for i := range v.Jobs {
		if v.Jobs[i].Name == name {
			return &v.Jobs[i]
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
