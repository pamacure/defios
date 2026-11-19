package shell_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/transcript"
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
	msgID := fmt.Sprintf("a9%062x", len(r.node.sg().runs[chain])+run.Step*16)
	run.Chain, run.Handle, run.Depth, run.Outcome, run.MsgID = chain, handle, depth, transcript.OutcomeSaid, msgID
	r.node.mu.Lock()
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
// followed by those its reply summons, with how its run ended or that it
// was skipped, and what it said. A step that failed ends the chain there.
func TestYourMessageDetailsListItsChainsSteps(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.tk().included[node.GroupTalk] = []string{"auditor", "scribe", "critic"}
	s := served(t, r)
	s.SendMessage("@auditor and @scribe, look")
	chain := r.node.tk().committed[node.GroupTalk][0].MsgID
	a := sayAs(r, node.GroupTalk, chain, "auditor", 1, transcript.Run{Step: 1}, "over to @critic and @ghost, and me, @auditor")
	r.node.mu.Lock()
	r.node.sg().runs[chain] = append(r.node.sg().runs[chain], transcript.Run{Chain: chain, Step: 2, Handle: "critic", Depth: 2, Outcome: transcript.OutcomeSilent})
	r.node.mu.Unlock()
	// @ghost, step 3, is in no profile here: skipped, with no run.
	b := sayAs(r, node.GroupTalk, chain, "scribe", 1, transcript.Run{Step: 4}, "done")
	ended(r, s, node.GroupTalk, chain, b.MsgID)

	d := s.MessageDetails(chain)
	want := []shell.ChainStep{
		{Step: 1, Handle: "auditor", Depth: 1, State: transcript.OutcomeSaid, MsgID: a.MsgID},
		{Step: 2, Handle: "critic", Depth: 2, State: transcript.OutcomeSilent},
		{Step: 3, Handle: "ghost", Depth: 2, State: shell.StepSkipped},
		{Step: 4, Handle: "scribe", Depth: 1, State: transcript.OutcomeSaid, MsgID: b.MsgID},
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

	d := s.RunDetails(chain, "auditor")
	if d.Problem != nil || d.Run == nil || d.Chain != chain || d.Step != 1 || d.Device != "This Device" || !d.Online {
		t.Fatalf("details: %+v", d)
	}
	if o := d.Run.Opening; o != (shell.Opening{Heading: "Job nightly-check", Summons: "(the Job's prompt)", Also: "(facts)"}) || !d.Run.Cleared {
		t.Fatalf("run: %+v", d.Run)
	}
	if d := s.RunDetails(chain, "scribe"); d.Problem == nil {
		t.Fatalf("details of no run: %+v", d)
	}

	e := s.EarlierRuns("auditor")
	if e.Problem != nil || len(e.Runs) != 1 || e.Runs[0].Chain != "c0ffee" || e.Runs[0].Cleared {
		t.Fatalf("earlier runs: %+v", e)
	}
}
