package shell

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/jobrun"
	"defios.com/internal/mention"
	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/transcript"
)

// Details is what the Talk page's panel shows of the line selected in the
// open Conversation, beyond what its Item says: where it sits in the log
// and in a chain, the device that wrote it, and, for this device's agent,
// the run that wrote it, or for this person's message, the chain it
// started; for a Job line, or a message this device's Job wrote, its Job
// Run. What does not apply is left zero.
type Details struct {
	MsgID string
	Seq   uint64 // 0 while queued, and for a system line
	// Chain and Step place it in a chain: its run's, or for this person's
	// message, the chain it starts. Depth is the message's.
	Chain string
	Step  int
	Depth uint32
	// Device is the device that wrote it, "This Device" for this one,
	// DeviceID its Node ID, and Online whether it is reachable now.
	Device, DeviceID string
	Online           bool
	// Run is the run of this device's agent that wrote it.
	Run *RunView
	// Steps are the chain this person's message started, step by step.
	Steps []ChainStep
	// Job is the Job Run of a Job line, or of this device's Job that
	// wrote the message; an agent Job's run is Run.
	Job *JobRunView
	// Problem is why the details could not be read.
	Problem *Problem
}

// RunView is one run of one of this device's agents, as its Transcript
// recorded it.
type RunView struct {
	Handle string
	Chain  string
	Step   int
	Depth  uint32
	// Outcome is how it ended, a transcript.Outcome*, "" if it was cut off
	// or is running; Error is why, with OutcomeError, and MsgID what it
	// said, with OutcomeSaid.
	Outcome, Error, MsgID string
	// InputTokens and OutputTokens are its model calls' added up.
	InputTokens, OutputTokens int64
	// Provider, ModelID and Shape are what it ran on.
	Provider, ModelID, Shape string
	// Signs are the Sign Requests its sign Tools acted on.
	Signs []RunSign
	// Tools and Skills are those granted to it for the run.
	Tools, Skills []string
	// Opening is its turn: what summoned it, and what else it was given.
	Opening Opening
	// Output is what it generated: thinking, text and tool calls, each
	// call's result after it (transcript.Run.Output).
	Output []json.RawMessage
	// Cleared: the run is in a Transcript since cleared, which its agent
	// no longer replays (ADR 0036).
	Cleared bool
}

// JobRunView is a Job Run of this device's (ADR 0040) as the panel shows
// it: what fired it, how it ended, and what it produced.
type JobRunView struct {
	ID uint64
	// Trigger fired on Event, its type, with EventMsgID the message or
	// Sign Request it is about, "" for none; a Schedule's is
	// trigger.ScheduleEvent. Trigger is "" for a Job run by hand.
	Trigger, Event, EventMsgID string
	// ByHand: run by hand (Run now, job run), with no Trigger.
	ByHand                bool
	Job, Kind             string // Kind is agent, exec or webhook
	State, Error          string // State is a node.Job* state
	Fired, Started, Ended time.Time
	// An exec Job's command, its exit code, nil if it never started or
	// was cut off, and the last of its output.
	Command        []string
	ExitCode       *int
	Stdout, Stderr string
	// A webhook's method and URL as configured, each attempt, and the
	// last response's body and Content-Type.
	Method, URL       string
	Attempts          []JobAttempt
	Body, ContentType string
	// Handle is an agent Job's agent.
	Handle string
	// Signs are the Sign Requests an exec Job proposed, accepted or
	// refused; an agent Job's are its run's.
	Signs []RunSign
}

// JobAttempt is one webhook attempt: its HTTP status, 0 for none, and its
// error, if it failed.
type JobAttempt struct {
	Status int
	Error  string
}

// jobRunView is r as the panel shows it; t.mu is held.
func (t *talk) jobRunView(r node.JobRun) *JobRunView {
	v := &JobRunView{
		ID: r.ID, Trigger: r.Trigger, ByHand: r.ByHand, Event: r.Event.Type, EventMsgID: cmp.Or(r.Event.MsgID, r.Event.SignRequestID),
		Job: r.Job, Kind: r.Kind, State: r.State, Error: r.Error, Fired: r.Fired, Started: r.Started, Ended: r.Ended,
		Handle: r.Handle,
	}
	if e := r.Exec; e != nil {
		v.Command, v.ExitCode, v.Stdout, v.Stderr = e.Command, e.ExitCode, e.Stdout, e.Stderr
	}
	if w := r.Webhook; w != nil {
		v.Method, v.URL, v.Body, v.ContentType = w.Method, w.URL, w.Body, w.ContentType
		for _, a := range w.Attempts {
			v.Attempts = append(v.Attempts, JobAttempt{Status: a.Status, Error: a.Error})
		}
	}
	for _, sg := range r.Signs {
		v.Signs = append(v.Signs, t.runSign(jobSignTool[sg.Act], sg.MsgID))
	}
	return v
}

// jobSignTool is the sign Tool an agent does each of an exec Job's acts
// with, as the panel names what a run did.
var jobSignTool = map[string]string{
	jobrun.Propose: config.ToolSignPropose,
	jobrun.Accept:  config.ToolSignAccept,
	jobrun.Refuse:  config.ToolSignRefuse,
}

// runSign is the Sign Request msgID that tool acted on, with its reason;
// t.mu is held.
func (t *talk) runSign(tool, msgID string) RunSign {
	sign := RunSign{Tool: tool, MsgID: msgID}
	for _, q := range t.reqs {
		if q.MsgID == msgID {
			sign.Reason = q.Reason
		}
	}
	return sign
}

// RunSign is a Sign Request a run's sign Tool acted on, and its reason.
type RunSign struct {
	Tool, MsgID, Reason string
}

// Opening is a run's turn: Heading names what summoned it ("Your
// message", "@scout's mention", "Job nightly-check"); By is the summoning
// message's msg_id, "" for a Job's; Summons is that message's line or the
// Job's prompt, in full; Also is the rest of the turn.
type Opening struct {
	Heading, By, Summons, Also string
}

// Chain step states, besides a run's transcript.Outcome*.
const (
	StepSkipped = "skipped" // the chain passed it over
	StepRunning = "running" // its agent is answering now
	StepWaiting = "waiting" // the chain has not reached it yet
	StepCutOff  = "cut off" // a serve stop ended its run
)

// ChainStep is one step of a chain: its agent, and State, its run's
// Outcome or a Step* state, with Error why it failed and MsgID what it
// said.
type ChainStep struct {
	Step   int
	Handle string
	Depth  uint32
	State  string
	Error  string
	MsgID  string
}

// EarlierView is an agent's live Transcript in the open Conversation: its
// runs, oldest first.
type EarlierView struct {
	Runs    []RunView
	Problem *Problem
}

// MessageDetails are the details of message msgID in the open
// Conversation.
func (s *Shell) MessageDetails(msgID string) Details { return s.talk.messageDetails(msgID) }

// RunDetails are the details of the run of this device's agent at step of
// chain in the open Conversation, as a run that posted nothing names it.
func (s *Shell) RunDetails(chain string, step int) Details { return s.talk.runDetails(chain, step) }

// JobRunDetails are the details of this device's Job Run id, as a Job
// line names it.
func (s *Shell) JobRunDetails(id uint64) Details { return s.talk.jobRunDetails(id) }

// EarlierRuns are the runs of this device's agent handle's live Transcript
// in the open Conversation: what it remembers of its work there.
func (s *Shell) EarlierRuns(handle string) EarlierView { return s.talk.earlierRuns(handle) }

var (
	errNoMessage = &Problem{Code: "no-message", Message: "That message is no longer here."}
	errNoJobRun  = &Problem{Code: "no-job-run", Message: "This device no longer keeps that Job Run."}
)

func (t *talk) messageDetails(msgID string) Details {
	a, ok := t.now()
	if !ok {
		return Details{Problem: problemOf(node.ErrNotServed)}
	}
	t.mu.Lock()
	m, ok := t.message(msgID)
	if !ok || !t.still(a) {
		t.mu.Unlock()
		return Details{Problem: errNoMessage}
	}
	chain := t.said[msgID]
	job := m.NodeID == t.self && m.Depth > 0 && (m.Handle == "" || chain != "")
	d := Details{MsgID: m.MsgID, Seq: m.Seq, Depth: m.Depth, DeviceID: m.NodeID, Device: t.device(m.NodeID), Online: t.online(m.NodeID)}
	t.mu.Unlock()
	switch {
	case m.Mine && len(mention.Handles(m.Text)) > 0:
		d.Chain = m.MsgID
		steps, err := t.chainSteps(a, m)
		if err != nil {
			d.Problem = problemOf(err)
		}
		d.Steps = steps
	case chain != "":
		tr, err := a.n.Transcript(t.ctx, a.group, a.conv, chain)
		if err != nil {
			d.Problem = problemOf(err)
			break
		}
		for _, r := range tr.Runs {
			if r.Outcome == transcript.OutcomeSaid && r.MsgID == msgID {
				t.mu.Lock()
				v := t.runView(r, slices.Contains(tr.Cleared, r.Step))
				t.mu.Unlock()
				d.Run, d.Chain, d.Step = &v, chain, r.Step
				break
			}
		}
	}
	if job && d.Problem == nil {
		d.Job = t.jobRunOf(a, m, chain)
	}
	return d
}

// jobRunOf is the Job Run of this device's Job that wrote m: an exec
// Job's, recording it, or an agent Job's whose agent said it, running as
// chain; nil for none kept, or if they do not read, as the rest of m's
// details stand without it. They are read afresh: a Job Run records a
// message as it is written.
func (t *talk) jobRunOf(a at, m node.Message, chain string) *JobRunView {
	runs, err := a.n.JobRuns(t.ctx, a.group, "", 0)
	if err != nil {
		return nil
	}
	for _, r := range runs {
		agent := chain != "" && r.Kind == config.JobAgent && r.Chain == chain && r.Handle == m.Handle
		if agent || slices.Contains(r.Messages, m.MsgID) {
			t.mu.Lock()
			defer t.mu.Unlock()
			return t.jobRunView(r)
		}
	}
	return nil
}

func (t *talk) jobRunDetails(id uint64) Details {
	a, ok := t.now()
	if !ok {
		return Details{Problem: problemOf(node.ErrNotServed)}
	}
	r, err := a.n.JobRun(t.ctx, a.group, id)
	if errors.Is(err, node.ErrNoJobRun) {
		return Details{Problem: errNoJobRun}
	}
	if err != nil {
		return Details{Problem: problemOf(err)}
	}
	t.mu.Lock()
	d := Details{DeviceID: t.self, Device: t.device(t.self), Online: true, Job: t.jobRunView(r)}
	t.mu.Unlock()
	if r.Kind != config.JobAgent || r.Chain == "" {
		return d
	}
	// An agent Job: its agent's run, the first of its chain.
	tr, err := a.n.Transcript(t.ctx, a.group, r.Conversation, r.Chain)
	if err != nil {
		d.Problem = problemOf(err)
		return d
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, run := range tr.Runs {
		if run.Handle != r.Handle {
			continue
		}
		v := t.runView(run, slices.Contains(tr.Cleared, run.Step))
		d.Run, d.Chain, d.Step, d.Depth, d.MsgID = &v, r.Chain, run.Step, run.Depth, run.MsgID
		break
	}
	return d
}

func (t *talk) runDetails(chain string, step int) Details {
	a, ok := t.now()
	if !ok {
		return Details{Problem: problemOf(node.ErrNotServed)}
	}
	tr, err := a.n.Transcript(t.ctx, a.group, a.conv, chain)
	if err != nil {
		return Details{Problem: problemOf(err)}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	d := Details{Chain: chain, DeviceID: t.self, Device: t.device(t.self), Online: true}
	for _, r := range tr.Runs {
		if r.Step != step {
			continue
		}
		v := t.runView(r, slices.Contains(tr.Cleared, r.Step))
		d.Run, d.Step, d.Depth, d.MsgID = &v, r.Step, r.Depth, r.MsgID
		break
	}
	if d.Run == nil {
		d.Problem = &Problem{Code: "no-transcript", Message: "This device has no record of that run."}
	}
	return d
}

func (t *talk) earlierRuns(handle string) EarlierView {
	a, ok := t.now()
	if !ok {
		return EarlierView{Problem: problemOf(node.ErrNotServed)}
	}
	runs, err := a.n.LiveRuns(t.ctx, a.group, a.conv, handle)
	if err != nil {
		return EarlierView{Problem: problemOf(err)}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := EarlierView{Runs: []RunView{}}
	for _, r := range runs {
		out.Runs = append(out.Runs, t.runView(r, false))
	}
	return out
}

// message is the message msgID of the open Conversation; t.mu is held.
func (t *talk) message(msgID string) (node.Message, bool) {
	i := slices.IndexFunc(t.msgs, func(m node.Message) bool { return m.MsgID == msgID })
	if i < 0 {
		return node.Message{}, false
	}
	return t.msgs[i], true
}

// online reports whether device nodeID is reachable now; t.mu is held.
func (t *talk) online(nodeID string) bool {
	if nodeID == t.self {
		return true
	}
	for _, r := range t.status.Roster {
		if r.NodeID == nodeID {
			return r.Reachable
		}
	}
	return false
}

// runView is r as the panel shows it; t.mu is held.
func (t *talk) runView(r transcript.Run, cleared bool) RunView {
	v := RunView{
		Handle: r.Handle, Chain: r.Chain, Step: r.Step, Depth: r.Depth,
		Outcome: r.Outcome, Error: r.Error, MsgID: r.MsgID,
		Provider: r.Provider, ModelID: r.ModelID, Shape: r.Shape,
		Tools: r.Tools, Skills: r.Skills, Output: r.Output(), Cleared: cleared,
		Opening: Opening{Heading: t.heading(r), By: r.By, Summons: r.Summons, Also: serve.TurnBesides(r.Turn, r.Summons)},
	}
	for _, u := range r.Usage {
		in, out := transcript.Tokens(u)
		v.InputTokens += in
		v.OutputTokens += out
	}
	for _, ac := range r.Acted {
		v.Signs = append(v.Signs, t.runSign(ac.Tool, ac.MsgID))
	}
	return v
}

// heading names what summoned r; t.mu is held.
func (t *talk) heading(r transcript.Run) string {
	if r.Job != "" {
		return "Job " + r.Job
	}
	m, ok := t.message(r.By)
	if r.By == "" || !ok || !m.Readable() {
		return "The message that summoned it"
	}
	switch {
	case m.Mine:
		return "Your message"
	case m.Handle != "":
		return "@" + m.Handle + "'s mention"
	case m.Depth > 0:
		return "A Job's message"
	}
	return t.device(m.NodeID) + "'s message"
}

// chainSteps are the steps of the chain this person's message m started,
// rebuilt as the chain walks them (internal/chain): depth-first in mention
// order, each said message's mentions after it. A step with no run here
// was skipped, or is not reached yet while the chain runs; one that failed,
// was cancelled or was cut off ended the chain there.
func (t *talk) chainSteps(a at, m node.Message) ([]ChainStep, error) {
	tr, err := a.n.Transcript(t.ctx, a.group, a.conv, m.MsgID)
	if err != nil && !errors.Is(err, serve.ErrNotAChain) {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	live := slices.ContainsFunc(t.live[a.conv], func(c *LiveChain) bool { return c.Chain == m.MsgID })
	ran := map[int]transcript.Run{}
	for _, r := range tr.Runs {
		ran[r.Step] = r
	}
	var out []ChainStep
	n := 0
	fits := true // each run is the Handle its step reaches
	var walk func(handles []string, depth uint32) bool
	walk = func(handles []string, depth uint32) bool {
		for _, h := range handles {
			n++
			st := ChainStep{Step: n, Handle: h, Depth: depth}
			r, ok := ran[n]
			switch {
			case ok && r.Handle != h:
				fits = false
				return false
			case !ok && live:
				st.State = StepWaiting
				out = append(out, st)
				return false
			case !ok:
				st.State = StepSkipped
				out = append(out, st)
				continue
			}
			st.State, st.Error, st.MsgID = r.Outcome, r.Error, r.MsgID
			switch {
			case r.Outcome == "" && live:
				st.State = StepRunning
			case r.Outcome == "":
				st.State = StepCutOff
			}
			out = append(out, st)
			switch r.Outcome {
			case transcript.OutcomeSilent:
				continue
			case transcript.OutcomeSaid:
			default:
				return false // the chain stopped here
			}
			said, ok := t.message(r.MsgID)
			if !ok || !said.Readable() {
				return false // what it summoned is not known here
			}
			next := slices.DeleteFunc(mention.Handles(said.Text), func(s string) bool { return s == h })
			if !walk(next, depth+1) {
				return false
			}
		}
		return true
	}
	walk(mention.Handles(m.Text), 1)
	if !fits {
		// Not the chain its mentions walk: its runs as they are.
		out = nil
		for _, r := range tr.Runs {
			out = append(out, ChainStep{Step: r.Step, Handle: r.Handle, Depth: r.Depth, State: r.Outcome, Error: r.Error, MsgID: r.MsgID})
		}
	}
	return out, nil
}
