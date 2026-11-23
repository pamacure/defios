package shell_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/transcript"
	"defios.com/internal/trigger"
)

// raws is each of js as a raw JSON value.
func raws(js ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(js))
	for i, j := range js {
		out[i] = json.RawMessage(j)
	}
	return out
}

// sayAs commits text as this device's agent handle's at depth, said by run
// of chain: what a chain leaves once the agent answered.
func sayAs(r *rig, conv, chain, handle string, depth uint32, run transcript.Run, text string) node.Message {
	r.t.Helper()
	r.node.mu.Lock()
	msgID := fmt.Sprintf("a9%062x", len(r.node.sg().runs[chain])+run.Step*16)
	run.Chain, run.Handle, run.Depth, run.Outcome, run.MsgID = chain, handle, depth, transcript.OutcomeSaid, msgID
	r.node.sg().runs[chain] = append(r.node.sg().runs[chain], run)
	r.node.mu.Unlock()
	return r.node.commit(conv, node.Message{MsgID: msgID, NodeID: r.node.NodeID(), DisplayName: "laptop", Handle: handle, Depth: depth, Text: text})
}

// ended ends chain in conv, and waits for the page to show msgID as said
// in it.
func ended(r *rig, s *shell.Shell, conv, chain, msgID string) {
	r.t.Helper()
	chainEvent(r.node, conv, node.ChainEvent{Type: serve.EventChainEnd, Chain: chain})
	eventuallyTalk(r.t, s, func(ts shell.TalkState) bool {
		return slices.ContainsFunc(ts.Items, func(it shell.Item) bool { return it.MsgID == msgID && it.Chain == chain })
	})
}

// The panel of this device's agent's message gives its run: how it ended,
// the tokens of its model calls added up, what it ran on, the Sign
// Requests it acted on, its grants, and its turn split into what summoned
// it, headed by who did, and the rest it was given; then what it
// generated.
func TestAnAgentsMessageDetailsAreItsRun(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().included[node.GroupTalk] = []string{"auditor"}
	s := served(t, r)
	s.SendMessage("@auditor check this")
	chain := r.node.tk().committed[node.GroupTalk][0].MsgID
	req := r.node.propose(node.GroupTalk, r.node.NodeID(), "auditor", "pay the auditor", 2)
	summons := "[1 2026-11-19 09:01] laptop: @auditor check this"
	said := sayAs(r, node.GroupTalk, chain, "auditor", 1, transcript.Run{
		Step: 1, By: chain, Summons: summons,
		Turn:     "(Talk since your last run:)\n[0 2026-11-19 09:00] bob: hi\n(You are summoned by:)\n" + summons + "\n(facts)",
		Provider: "anthropic", ModelID: "claude-opus-5-5", Shape: "messages",
		Tools: []string{"talk_write", "sign_accept"}, Skills: []string{"base/audit"},
		Usage:  raws(`{"input_tokens":10,"output_tokens":2}`, `{"input_tokens":20,"cache_read_input_tokens":5,"output_tokens":3}`),
		Blocks: raws(`{"type":"thinking","thinking":"hm"}`, `{"type":"tool_use","id":"t1","name":"sign_accept"}`, `{"type":"tool_result","tool_use_id":"t1","content":"ok"}`),
		Acted:  []transcript.Acted{{Tool: "sign_accept", MsgID: req}},
	}, "Looks fine")

	ended(r, s, node.GroupTalk, chain, said.MsgID)
	d := s.MessageDetails(said.MsgID)
	if d.Problem != nil {
		t.Fatalf("details: %+v", d.Problem)
	}
	if d.MsgID != said.MsgID || d.Seq != said.Seq || d.Chain != chain || d.Step != 1 || d.Depth != 1 ||
		d.Device != "This Device" || d.DeviceID != r.node.NodeID() || len(d.Steps) != 0 {
		t.Fatalf("details: %+v", d)
	}
	run := d.Run
	if run == nil {
		t.Fatal("no run")
	}
	want := shell.RunView{
		Handle: "auditor", Chain: chain, Step: 1, Depth: 1, Outcome: transcript.OutcomeSaid, MsgID: said.MsgID,
		InputTokens: 35, OutputTokens: 5, Provider: "anthropic", ModelID: "claude-opus-5-5", Shape: "messages",
		Signs: []shell.RunSign{{Tool: "sign_accept", MsgID: req, Reason: "pay the auditor"}},
		Tools: []string{"talk_write", "sign_accept"}, Skills: []string{"base/audit"},
		Opening: shell.Opening{
			Heading: "Your message", By: chain, Summons: summons,
			Also: "(Talk since your last run:)\n[0 2026-11-19 09:00] bob: hi\n(facts)",
		},
		Output: raws(`{"type":"thinking","thinking":"hm"}`, `{"type":"tool_use","id":"t1","name":"sign_accept"}`, `{"type":"tool_result","tool_use_id":"t1","content":"ok"}`),
	}
	if !reflect.DeepEqual(*run, want) {
		t.Fatalf("run:\n%+v\nwant\n%+v", *run, want)
	}
}

// The panel of this person's message lists the chain it started, step by
// step as the chain walks them: each agent its message mentions, each
// followed by those its run summoned with talk_summon, whether it spoke or
// stayed silent, with how its run ended or that it was skipped, and what
// it said. What an agent's text mentions summons nobody. A step that
// failed ends the chain there.
func TestYourMessageDetailsListItsChainsSteps(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().included[node.GroupTalk] = []string{"auditor", "scribe", "critic"}
	s := served(t, r)
	s.SendMessage("@auditor and @scribe, look")
	chain := r.node.tk().committed[node.GroupTalk][0].MsgID
	a := sayAs(r, node.GroupTalk, chain, "auditor", 1, transcript.Run{Step: 1, Summoned: []string{"critic", "ghost"}}, "over to @scribe")
	r.node.mu.Lock()
	r.node.sg().runs[chain] = append(r.node.sg().runs[chain],
		transcript.Run{Chain: chain, Step: 2, Handle: "critic", Depth: 2, Outcome: transcript.OutcomeSilent, Summoned: []string{"scribe"}},
		transcript.Run{Chain: chain, Step: 3, Handle: "scribe", Depth: 3, Outcome: transcript.OutcomeSilent})
	r.node.mu.Unlock()
	// @ghost, step 4, is in no profile here: skipped, with no run.
	b := sayAs(r, node.GroupTalk, chain, "scribe", 1, transcript.Run{Step: 5}, "done")
	ended(r, s, node.GroupTalk, chain, b.MsgID)

	d := s.MessageDetails(chain)
	want := []shell.ChainStep{
		{Step: 1, Handle: "auditor", Depth: 1, State: transcript.OutcomeSaid, MsgID: a.MsgID},
		{Step: 2, Handle: "critic", Depth: 2, State: transcript.OutcomeSilent},
		{Step: 3, Handle: "scribe", Depth: 3, State: transcript.OutcomeSilent},
		{Step: 4, Handle: "ghost", Depth: 2, State: shell.StepSkipped},
		{Step: 5, Handle: "scribe", Depth: 1, State: transcript.OutcomeSaid, MsgID: b.MsgID},
	}
	if d.Problem != nil || d.Chain != chain || d.Run != nil || !reflect.DeepEqual(d.Steps, want) {
		t.Fatalf("details: %+v\nsteps %+v\nwant  %+v", d, d.Steps, want)
	}

	s.SendMessage("@auditor and @scribe, again")
	again := r.node.tk().committed[node.GroupTalk][3].MsgID
	r.node.mu.Lock()
	r.node.sg().runs[again] = []transcript.Run{{Chain: again, Step: 1, Handle: "auditor", Depth: 1, Outcome: transcript.OutcomeError, Error: "boom"}}
	r.node.mu.Unlock()
	eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return slices.ContainsFunc(ts.Items, func(it shell.Item) bool { return it.MsgID == again })
	})
	if d := s.MessageDetails(again); !reflect.DeepEqual(d.Steps, []shell.ChainStep{{Step: 1, Handle: "auditor", Depth: 1, State: transcript.OutcomeError, Error: "boom"}}) {
		t.Fatalf("a failed chain's steps: %+v", d.Steps)
	}

	// A message that summons nobody started no chain.
	s.SendMessage("thanks")
	thanks := r.node.tk().committed[node.GroupTalk][4].MsgID
	eventuallyTalk(t, s, func(ts shell.TalkState) bool {
		return slices.ContainsFunc(ts.Items, func(it shell.Item) bool { return it.MsgID == thanks })
	})
	if d := s.MessageDetails(thanks); d.Problem != nil || d.Chain != "" || d.Steps != nil || d.Run != nil {
		t.Fatalf("details of a message with no chain: %+v", d)
	}
}

// The panel of another device's message names that device, its Device
// ID and whether it is online now, and has no run or chain: Transcripts
// are never replicated.
func TestAnotherDevicesMessageDetailsNameTheDevice(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	r.node.publish(node.Status{Group: "alpha", Serving: true, MajorityReachable: true, Roster: []node.RosterNode{
		{NodeID: r.node.NodeID(), DisplayName: "laptop", Reachable: true}, {NodeID: "b0b", DisplayName: "bob", Reachable: false}}})
	m := r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Handle: "scout", Depth: 1, Text: "found it"})
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Items) == 1 && len(ts.Roster) == 2 })
	d := s.MessageDetails(m.MsgID)
	if !reflect.DeepEqual(d, shell.Details{MsgID: m.MsgID, Seq: m.Seq, Depth: 1, Device: "bob", DeviceID: "b0b"}) {
		t.Fatalf("details: %+v", d)
	}
	if d := s.MessageDetails("gone"); d.Problem == nil {
		t.Fatalf("details of no message: %+v", d)
	}
}

// The panel of a line about a run, as a Job's agent that stayed silent,
// gives that run: headed by the Job that summoned it, marked as from a
// cleared Transcript once its agent was cleared since. Earlier runs are
// the runs its agent's live Transcript holds.
func TestARunsLineDetailsAreItsRunAndEarlierRunsTheLiveTranscript(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	chain := fmt.Sprintf("10b%061x", 1)
	r.node.mu.Lock()
	r.node.sg().runs[chain] = []transcript.Run{{
		Chain: chain, Step: 1, Handle: "auditor", Depth: 1, Outcome: transcript.OutcomeSilent,
		Job: "nightly-check", Summons: "(the Job's prompt)", Turn: "(the Job's prompt)\n(facts)",
	}}
	r.node.sg().cleared[chain] = []int{1}
	r.node.tk().liveRuns[node.GroupTalk+"/auditor"] = []node.Run{{Chain: "c0ffee", Step: 1, Handle: "auditor", Depth: 1, Outcome: transcript.OutcomeSilent}}
	r.node.mu.Unlock()
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventAgentSilent, Chain: chain, Handle: "auditor"})
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return len(ts.Items) == 1 })

	d := s.RunDetails(chain, 1)
	if d.Problem != nil || d.Run == nil || d.Chain != chain || d.Step != 1 || d.Device != "This Device" || !d.Online {
		t.Fatalf("details: %+v", d)
	}
	if o := d.Run.Opening; o != (shell.Opening{Heading: "Job nightly-check", Summons: "(the Job's prompt)", Also: "(facts)"}) || !d.Run.Cleared {
		t.Fatalf("run: %+v", d.Run)
	}
	if d := s.RunDetails(chain, 2); d.Problem == nil {
		t.Fatalf("details of no run: %+v", d)
	}

	e := s.EarlierRuns("auditor")
	if e.Problem != nil || len(e.Runs) != 1 || e.Runs[0].Chain != "c0ffee" || e.Runs[0].Cleared {
		t.Fatalf("earlier runs: %+v", e)
	}
}

// A Job line's panel is its Job Run: what fired it on what, or that it
// was run by hand, how it ended and when, and what it produced: an exec
// Job's command, exit code and output, a webhook's request, statuses and
// response.
func TestAJobLinesDetailsAreItsJobRun(t *testing.T) {
	r := newRig(t, "alpha")
	ev := trigger.Event{Type: "app.freeform", MsgID: "e1"}
	fired := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	exit := 3
	r.node.addJobRuns(
		node.JobRun{Trigger: "said", Event: ev, Fired: fired, Job: "say", Kind: "exec", State: node.JobFailed, Started: fired, Ended: fired.Add(time.Second), Error: "exit status 3",
			Output: trigger.Output{Exec: &trigger.ExecOutput{Command: []string{"sh", "-c", "echo hi"}, ExitCode: &exit, Stdout: "hi\n", Stderr: "oops\n"}}},
		node.JobRun{Trigger: "said", Event: ev, Fired: fired, Job: "web", Kind: "webhook", State: node.JobNotRun},
	)
	r.node.addJobRuns(node.JobRun{Trigger: "said", Event: ev, Fired: fired, Job: "web", Kind: "webhook", State: node.JobOK, Started: fired, Ended: fired,
		Output: trigger.Output{Webhook: &trigger.WebhookOutput{Method: "POST", URL: "$HOOK_URL", Attempts: []trigger.Attempt{{Status: 502, Error: "HTTP 502"}, {Status: 200}}, Body: "got it", ContentType: "text/plain"}}})
	s := served(t, r)
	eventuallyTalk(t, s, items(4))

	d := s.JobRunDetails(1)
	j := d.Job
	if d.Problem != nil || j == nil || d.Device != "This Device" || d.DeviceID != r.node.NodeID() || !d.Online || d.Run != nil {
		t.Fatalf("exec details: %+v", d)
	}
	if j.ID != 1 || j.Trigger != "said" || j.Event != "app.freeform" || j.EventMsgID != "e1" || j.Job != "say" || j.Kind != "exec" ||
		j.State != node.JobFailed || j.Error != "exit status 3" || !j.Fired.Equal(fired) || !j.Started.Equal(fired) || !j.Ended.Equal(fired.Add(time.Second)) ||
		!slices.Equal(j.Command, []string{"sh", "-c", "echo hi"}) || j.ExitCode == nil || *j.ExitCode != 3 || j.Stdout != "hi\n" || j.Stderr != "oops\n" || j.Method != "" {
		t.Fatalf("exec Job Run: %+v", j)
	}
	j = s.JobRunDetails(3).Job
	if j == nil || j.Kind != "webhook" || j.Method != "POST" || j.URL != "$HOOK_URL" || j.Body != "got it" || j.ContentType != "text/plain" ||
		!reflect.DeepEqual(j.Attempts, []shell.JobAttempt{{Status: 502, Error: "HTTP 502"}, {Status: 200}}) || j.ExitCode != nil {
		t.Fatalf("webhook Job Run: %+v", j)
	}
	r.node.addJobRuns(node.JobRun{ByHand: true, Fired: fired, Job: "say", Kind: "exec", State: node.JobOK})
	if j = s.JobRunDetails(4).Job; j == nil || !j.ByHand || j.Trigger != "" {
		t.Fatalf("Job Run by hand: %+v", j)
	}
	if d := s.JobRunDetails(9); d.Problem == nil || d.Job != nil {
		t.Fatalf("no such Job Run: %+v", d)
	}
}

// An agent Job's Job Run gives its agent's run, as that agent's message
// does; and that message gives its Job Run, as one an exec Job wrote with
// its token does. Another agent's reply in its chain, and another
// device's Job's message, have none.
func TestAJobsMessageGivesItsJobRun(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	chain := fmt.Sprintf("c4%062x", 1)
	said := sayAs(r, node.GroupTalk, chain, "scout", 1, transcript.Run{Step: 1, Job: "nightly", Summons: "(the prompt)", Turn: "(the prompt)"}, "All quiet")
	reply := sayAs(r, node.GroupTalk, chain, "auditor", 2, transcript.Run{Step: 2}, "Agreed")
	wrote := r.node.commit(node.GroupTalk, node.Message{NodeID: r.node.NodeID(), DisplayName: "laptop", Depth: 1, Text: "checked"})
	other := r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Depth: 1, Text: "theirs"})
	now := time.Now()
	r.node.addJobRuns(node.JobRun{Trigger: "night", Event: trigger.Event{Type: trigger.ScheduleEvent}, Fired: now, Job: "nightly", Kind: "agent", State: node.JobOK,
		Started: now, Ended: now, Handle: "scout", Chain: chain})
	r.node.addJobRuns(node.JobRun{Trigger: "said", Event: trigger.Event{Type: "app.freeform"}, Fired: now, Job: "check", Kind: "exec", State: node.JobOK,
		Started: now, Ended: now, Messages: []string{wrote.MsgID}})
	ended(r, s, node.GroupTalk, chain, said.MsgID)
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { // read with said, but perhaps not by the same read
		return slices.ContainsFunc(ts.Items, func(it shell.Item) bool { return it.MsgID == reply.MsgID && it.Chain == chain })
	})

	d := s.JobRunDetails(1)
	if d.Problem != nil || d.Job == nil || d.Job.Handle != "scout" || d.Run == nil || d.Run.MsgID != said.MsgID || d.Chain != chain || d.Step != 1 {
		t.Fatalf("agent Job Run: %+v", d)
	}
	d = s.MessageDetails(said.MsgID)
	if d.Problem != nil || d.Job == nil || d.Job.ID != 1 || d.Run == nil || d.Run.Opening.Heading != "Job nightly" {
		t.Fatalf("agent Job's message: %+v", d)
	}
	d = s.MessageDetails(reply.MsgID)
	if d.Problem != nil || d.Job != nil || d.Run == nil {
		t.Fatalf("a reply in an agent Job's chain, by another agent: %+v", d)
	}
	d = s.MessageDetails(wrote.MsgID)
	if d.Problem != nil || d.Job == nil || d.Job.ID != 2 || d.Job.Job != "check" {
		t.Fatalf("exec Job's message: %+v", d)
	}
	d = s.MessageDetails(other.MsgID)
	if d.Problem != nil || d.Job != nil || d.DeviceID != "b0b" {
		t.Fatalf("another device's Job's message: %+v", d)
	}
}
