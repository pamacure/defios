// Package transcript is an Agent's Transcript (ADR 0036): everything it
// generated in its runs in one Conversation, one JSON line at a time, each
// marked with the chain it ran in, kept on the Node that ran it under the
// Conversation's transcripts/ and never replicated. serve appends it as a
// chain runs; anything may read it, serve up or down. A chain is rebuilt
// from the runs marked with it.
package transcript

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"defios.com/internal/agent"
	"defios.com/internal/nodedir"
)

// Outcomes: how a run ended (ADR 0018). A run cut off by a serve stop has
// none.
const (
	OutcomeSaid      = "said"      // msg_id, and seq once it has a row: the message
	OutcomeSilent    = "silent"    // ended its turn without talk_write
	OutcomeError     = "error"     // error: why
	OutcomeCancelled = "cancelled" // a cancel stopped it
)

// Line kinds: which of a Line's fields it holds.
const (
	KindTurn     = "turn"              // turn, seq and unapplied: the run's user turn, and the talk it accounts for
	KindRequest  = agent.RecordRequest // request: a request body as sent, never its headers
	KindBlock    = agent.RecordBlock   // block: one output block, or a tool call's result
	KindUsage    = agent.RecordUsage   // usage: a model call's usage
	KindActed    = "acted"             // tool and msg_id: a sign Tool acted on that Sign Request
	KindSummoned = "summoned"          // summoned: talk_summon accepted that Handle (ADR 0041)
	KindOutcome  = "outcome"           // outcome and at, and msg_id and seq or error with it
)

// Line is one line of a Transcript. Every line names the run it belongs
// to: its chain, the hex msg_id of the message that started it or a Job's
// random id, its step in that chain's depth-first order (from 1), its
// Handle and depth. Kind says which of the rest it holds.
type Line struct {
	Chain  string `json:"chain"`
	Step   int    `json:"step"`
	Handle string `json:"handle"`
	Depth  uint32 `json:"depth"`
	// Kind is KindTurn, KindRequest, KindBlock, KindUsage, KindActed,
	// KindSummoned or KindOutcome.
	Kind string `json:"kind"`
	// Turn, with KindTurn, is the run's user turn as sent, which a later
	// run replays (ADR 0036). Seq is then the last seq of the talk its
	// Node's replica held, all of which the turn accounts for, and
	// Unapplied the hex msg_ids of this Node's own messages beyond the
	// replica that it accounts for too, queued or not yet applied: the
	// agent's next turn carries only the talk after them.
	Turn      string   `json:"turn,omitempty"`
	Unapplied []string `json:"unapplied,omitempty"`
	// Shape, Tools, Skills and Binding, with KindTurn, are what the run
	// was made under: its Provider's API Shape, the names of the Tools
	// offered it but skill_load, which its Skills imply, and of the Skills
	// granted to it, and its agent.Request's Binding. A later
	// run tells its agent what changed since, or starts a new Transcript
	// in another Shape (ADR 0036).
	Shape   string   `json:"shape,omitempty"`
	Tools   []string `json:"tools,omitempty"`
	Skills  []string `json:"skills,omitempty"`
	Binding string   `json:"binding,omitempty"`
	// Provider and ModelID, with KindTurn, are the Provider alias the run
	// called and the Model ID its request named, "" for none.
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"model_id,omitempty"`
	// By, Job and Summons, with KindTurn, are what summoned the run: the
	// hex msg_id of the message that did, or the name of the Job, and that
	// part of Turn as written, the summoning message's line or the Job's
	// prompt and event.
	By      string          `json:"by,omitempty"`
	Job     string          `json:"job,omitempty"`
	Summons string          `json:"summons,omitempty"`
	Request json.RawMessage `json:"request,omitempty"`
	Block   json.RawMessage `json:"block,omitempty"`
	Usage   json.RawMessage `json:"usage,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	// Summoned, with KindSummoned, is the Handle the run summoned.
	Summoned string `json:"summoned,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	MsgID    string `json:"msg_id,omitempty"`
	Seq      uint64 `json:"seq,omitempty"`
	Error    string `json:"error,omitempty"`
	// At, with KindOutcome, is when the run ended, UTC.
	At time.Time `json:"at,omitzero"`
}

// Outcome is the closing line of a run that said msgID, hex, at seq
// (msgID "" if nothing), and ended with err, recording when, now, UTC; a run that commits nothing
// and ends without an error is silent. Once a talk_write has committed it is
// `said`, whatever failed after. A run cut off by the serve stopping
// before that has none: ok is false.
func Outcome(msgID string, seq uint64, err error, cancelled, stopped bool) (l Line, ok bool) {
	l.Kind, l.At = KindOutcome, time.Now().UTC()
	switch {
	case msgID != "":
		l.Outcome, l.MsgID, l.Seq = OutcomeSaid, msgID, seq
	case err == nil:
		l.Outcome = OutcomeSilent
	case cancelled:
		l.Outcome = OutcomeCancelled
	case stopped:
		return l, false
	default:
		l.Outcome, l.Error = OutcomeError, err.Error()
	}
	return l, true
}

// Writer appends the runs of one chain to their agents' live Transcripts,
// each opened on its run's first line and held until the run's outcome, so
// a run finishes in the Transcript it started in though a Clear closes it
// meanwhile. One chain runs at a time per Conversation, so it is the one
// writer of each. A write that fails is logged once and the chain goes on:
// a Transcript is this Node's record, not what the chain is for.
type Writer struct {
	p      nodedir.Paths
	alias  string
	conv   string
	chain  string
	logf   func(string, ...any)
	files  map[string]*os.File // by Handle
	failed bool
}

// NewWriter is a Writer of the runs of chain, a hex msg_id or a Job's id,
// in conv (nil for Group-level talk), that logs a failed write to logf.
func NewWriter(p nodedir.Paths, alias string, conv []byte, chain string, logf func(string, ...any)) *Writer {
	return &Writer{p: p, alias: alias, conv: nodedir.ConversationName(conv), chain: chain, logf: logf, files: map[string]*os.File{}}
}

// Write appends l, marked with the chain, to the live Transcript of
// l.Handle, unless a write has failed before.
func (w *Writer) Write(l Line) {
	if w.failed {
		return
	}
	l.Chain = w.chain
	if err := w.append(l); err != nil {
		w.failed = true
		w.logf("serve: transcript of @%s, chain %s: %v; the rest of this chain is not recorded", l.Handle, w.chain, err)
	}
}

func (w *Writer) append(l Line) error {
	f := w.files[l.Handle]
	if f == nil {
		path := w.p.LiveTranscriptFile(w.alias, w.conv, l.Handle)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		var err error
		if f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err != nil {
			return err
		}
		w.files[l.Handle] = f
	}
	line, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encoding a line: %w", err)
	}
	_, err = f.Write(append(line, '\n'))
	if l.Kind == KindOutcome {
		// The run is over: the Agent's next one opens whichever
		// Transcript is live by then.
		delete(w.files, l.Handle)
		err = errors.Join(err, f.Close())
	}
	return err
}

// Earlier are the lines of handle's live Transcript as its run about to be
// written starts, nil if it has none. The Transcript is held open from
// here, so the run is recorded in the one it replays though a Clear closes
// it meanwhile (ADR 0036).
func (w *Writer) Earlier(handle string) ([]Line, error) {
	if f := w.files[handle]; f != nil {
		// Left by a run that wrote no outcome: whichever is live now is
		// the one to read.
		delete(w.files, handle)
		f.Close()
	}
	f, err := os.OpenFile(w.p.LiveTranscriptFile(w.alias, w.conv, handle), os.O_RDWR|os.O_APPEND, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	w.files[handle] = f
	return parseLines(data), nil
}

// Renew closes handle's live Transcript, which Earlier read, as Clear
// does, so the run about to be written starts a new one (ADR 0036). With
// none live there is nothing to close.
func (w *Writer) Renew(handle string) error {
	if f := w.files[handle]; f != nil {
		delete(w.files, handle)
		f.Close()
	}
	_, err := closeLive(w.p.LiveTranscriptFile(w.alias, w.conv, handle))
	return err
}

// Close closes the files its lines opened.
func (w *Writer) Close() {
	for _, f := range w.files {
		f.Close()
	}
}

// Live are the Handles of the Agents with a live Transcript in conv (nil
// for Group-level talk), in order; nil if there is none.
func Live(p nodedir.Paths, alias string, conv []byte) ([]string, error) {
	name := nodedir.ConversationName(conv)
	agents, err := os.ReadDir(p.TranscriptsDir(alias, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var live []string
	for _, a := range agents {
		if !a.IsDir() {
			continue
		}
		fi, err := os.Lstat(p.LiveTranscriptFile(alias, name, a.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if fi.Mode().IsRegular() {
			live = append(live, a.Name())
		}
	}
	return live, nil
}

// Size is how large one Agent's live Transcript has grown: Tokens are the
// input tokens of the last model call it records, which its next run
// replays give or take that call's output; 0 before its first.
type Size struct {
	Handle string
	Tokens int64
}

// Sizes are the sizes of the live Transcripts in conv (nil for Group-level
// talk), by Handle in order; nil if there is none. A Transcript only grows,
// and nothing compacts it: its size says when to clear it (ADR 0036).
func Sizes(p nodedir.Paths, alias string, conv []byte) ([]Size, error) {
	live, err := Live(p, alias, conv)
	if err != nil {
		return nil, err
	}
	var out []Size
	for _, h := range live {
		lines, err := readFile(p.LiveTranscriptFile(alias, nodedir.ConversationName(conv), h))
		if err != nil {
			return nil, err
		}
		s := Size{Handle: h}
		for _, l := range slices.Backward(lines) {
			if l.Kind == KindUsage {
				s.Tokens, _ = Tokens(l.Usage)
				break
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Clear closes the live Transcript of the Agent handle in conv (nil for
// Group-level talk), or with handle "" of every Agent there, and returns
// the Handles it closed one of, in order: nil if none had one. A closed
// Transcript is kept beside where the live one was, under the time it was
// closed, and that Agent's next run there starts a new live one; a run
// still going finishes in the one closed (ADR 0036). handle must be a
// valid Handle: it names a directory.
func Clear(p nodedir.Paths, alias string, conv []byte, handle string) ([]string, error) {
	live, err := Live(p, alias, conv)
	if err != nil {
		return nil, err
	}
	var cleared []string
	for _, h := range live {
		if handle != "" && h != handle {
			continue
		}
		closed, err := closeLive(p.LiveTranscriptFile(alias, nodedir.ConversationName(conv), h))
		if err != nil {
			return cleared, fmt.Errorf("closing the Transcript of @%s: %w", h, err)
		}
		if closed {
			cleared = append(cleared, h)
		}
	}
	return cleared, nil
}

// closedLayout names a closed Transcript by when it was closed, UTC, so
// an Agent's sort oldest first.
const closedLayout = "20060102T150405.000000000Z"

// closeLive renames the live Transcript at live to a closed one's name,
// one no Transcript beside it has; false if it is no longer there.
func closeLive(live string) (bool, error) {
	for at := time.Now().UTC(); ; at = at.Add(time.Nanosecond) {
		closed := filepath.Join(filepath.Dir(live), at.Format(closedLayout)+filepath.Ext(live))
		if _, err := os.Lstat(closed); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		err := os.Rename(live, closed)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
}

// Transcript is one Agent's Transcript in one Conversation as read: its
// live one, or one closed and kept beside it.
type Transcript struct {
	Handle string
	// Lines are its lines as stored, in order.
	Lines []Line
	// Live: it is the Agent's live one, not one cleared and closed.
	Live bool
}

// LiveLines are the lines of the live Transcript of the Agent handle in
// conv (nil for Group-level talk), in order; nil if it has none. handle
// must be a valid Handle: it names a directory.
func LiveLines(p nodedir.Paths, alias string, conv []byte, handle string) ([]Line, error) {
	return readFile(p.LiveTranscriptFile(alias, nodedir.ConversationName(conv), handle))
}

// ReadConversation is every Transcript in conv (nil for Group-level
// talk): by Handle, an Agent's closed ones in name order, then its live
// one; nil if there is none. A line it cannot read, such as one cut off
// by a stop, is passed over.
func ReadConversation(p nodedir.Paths, alias string, conv []byte) ([]Transcript, error) {
	return readDir(p.TranscriptsDir(alias, nodedir.ConversationName(conv)))
}

// readDir is the Transcripts under dir, a Conversation's transcripts/:
// one directory per Handle.
func readDir(dir string) ([]Transcript, error) {
	agents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Transcript
	for _, a := range agents {
		if !a.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, a.Name()))
		if err != nil {
			return nil, err
		}
		var names []string
		for _, f := range files {
			if f.Type().IsRegular() && nodedir.IsTranscriptFile(f.Name()) && f.Name() != nodedir.LiveTranscript {
				names = append(names, f.Name())
			}
		}
		for _, name := range append(names, nodedir.LiveTranscript) {
			lines, err := readFile(filepath.Join(dir, a.Name(), name))
			if err != nil {
				return nil, err
			}
			if lines != nil {
				out = append(out, Transcript{Handle: a.Name(), Lines: lines, Live: name == nodedir.LiveTranscript})
			}
		}
	}
	return out, nil
}

// readFile is the lines of file; nil if it is not there or holds none.
func readFile(file string) ([]Line, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseLines(data), nil
}

// parseLines is the lines of a Transcript file's data, passing over any
// it cannot read.
func parseLines(data []byte) []Line {
	var lines []Line
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		var l Line
		if json.Unmarshal(line, &l) != nil || l.Step < 1 || l.Chain == "" {
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

// Chains are the chains with a run in ts, in order; nil if there is none.
func Chains(ts []Transcript) []string {
	var chains []string
	for _, t := range ts {
		for _, l := range t.Lines {
			chains = append(chains, l.Chain)
		}
	}
	slices.Sort(chains)
	return slices.Compact(chains)
}

// ChainLines are the lines of chain's runs in ts, a Conversation's
// Transcripts, across agents: by step, each run's in order; nil if it has
// none.
func ChainLines(ts []Transcript, chain string) []Line {
	var lines []Line
	for _, t := range ts {
		for _, l := range t.Lines {
			if l.Chain == chain {
				lines = append(lines, l)
			}
		}
	}
	slices.SortStableFunc(lines, func(a, b Line) int { return cmp.Compare(a.Step, b.Step) })
	return lines
}

// Run is one run of one step of a chain, as its lines show it. Outcome is
// "" for a run cut off by a stop.
type Run struct {
	Chain  string
	Step   int
	Handle string
	Depth  uint32
	// Turn is its user turn as sent, "" for a run that never got as far.
	// The turn accounts for the talk its Node's replica held, up to
	// TalkSeq, and for this Node's own messages beyond the replica,
	// TalkUnapplied, hex msg_ids (ADR 0036).
	Turn          string
	TalkSeq       uint64
	TalkUnapplied []string
	// Shape, Tools, Skills and Binding are what it was made under, as its
	// turn line records them.
	Shape   string
	Tools   []string
	Skills  []string
	Binding string
	// Provider and ModelID are the Provider alias it called and the Model
	// ID it named; By, Job and Summons what summoned it, as its turn line
	// records them.
	Provider, ModelID string
	By, Job, Summons  string
	// Requests are the request bodies of its model calls as sent, Blocks
	// its output blocks and the results of its tool calls, and Usage each
	// call's usage, in order.
	Requests []json.RawMessage
	Blocks   []json.RawMessage
	Usage    []json.RawMessage
	// Acted is what its sign Tools did, in order (ADR 0029).
	Acted []Acted
	// Summoned are the Handles it summoned with talk_summon, in order
	// (ADR 0041).
	Summoned []string
	Outcome  string
	MsgID    string // with OutcomeSaid: what it said, hex
	Seq      uint64 // with OutcomeSaid, once it has a row
	Error    string // with OutcomeError: why
	// Ended is when it ended, as its outcome line records it: zero for a
	// run with no outcome.
	Ended time.Time
}

// Acted is one successful call of a sign Tool: the Tool, and the Sign
// Request it acted on, hex.
type Acted struct {
	Tool  string
	MsgID string
}

// Replays reports whether a later run of its Agent replays r: it was given
// its turn and generated something. A run that failed before any output,
// such as one whose Provider refused it, is left out of the replay, and
// the talk in its turn is given again (ADR 0036).
func (r Run) Replays() bool { return r.Turn != "" && len(r.Blocks) > 0 }

// Output is what r generated, in order: each model call's blocks,
// thinking, text and tool calls, followed by the results the run answered
// its tool calls with, agent.ToolResults. A call it did not answer, as an
// accepted talk_write, has none.
func (r Run) Output() []json.RawMessage { return r.Blocks }

// Runs are the runs of lines, one per step of each chain, in the order
// each starts: an Agent's in the order it ran them, a chain's
// (ChainLines) by step.
func Runs(lines []Line) []Run {
	type key struct {
		chain string
		step  int
	}
	var runs []Run
	at := map[key]int{}
	for _, l := range lines {
		k := key{l.Chain, l.Step}
		i, ok := at[k]
		if !ok {
			i = len(runs)
			at[k] = i
			runs = append(runs, Run{Chain: l.Chain, Step: l.Step, Handle: l.Handle, Depth: l.Depth})
		}
		r := &runs[i]
		switch l.Kind {
		case KindTurn:
			r.Turn, r.TalkSeq, r.TalkUnapplied = l.Turn, l.Seq, l.Unapplied
			r.Shape, r.Tools, r.Skills, r.Binding = l.Shape, l.Tools, l.Skills, l.Binding
			r.Provider, r.ModelID, r.By, r.Job, r.Summons = l.Provider, l.ModelID, l.By, l.Job, l.Summons
		case KindRequest:
			r.Requests = append(r.Requests, l.Request)
		case KindBlock:
			r.Blocks = append(r.Blocks, l.Block)
		case KindUsage:
			r.Usage = append(r.Usage, l.Usage)
		case KindActed:
			r.Acted = append(r.Acted, Acted{Tool: l.Tool, MsgID: l.MsgID})
		case KindSummoned:
			r.Summoned = append(r.Summoned, l.Summoned)
		case KindOutcome:
			r.Outcome, r.MsgID, r.Seq, r.Error, r.Ended = l.Outcome, l.MsgID, l.Seq, l.Error, l.At
		}
	}
	return runs
}

// Unposted are the runs in ts, a Conversation's Transcripts, that posted
// nothing: those that stayed silent, failed or were cancelled, in
// Transcript order, each's in the order its Agent ran them. A run cut off
// with no outcome is not among them.
func Unposted(ts []Transcript) []Run {
	var out []Run
	for _, t := range ts {
		for _, r := range Runs(t.Lines) {
			switch r.Outcome {
			case OutcomeSilent, OutcomeError, OutcomeCancelled:
				out = append(out, r)
			}
		}
	}
	return out
}

// Acting are the runs in ts whose sign Tools acted on a Sign Request.
func Acting(ts []Transcript) []Run {
	var out []Run
	for _, t := range ts {
		for _, r := range Runs(t.Lines) {
			if len(r.Acted) > 0 {
				out = append(out, r)
			}
		}
	}
	return out
}

// Tokens are the input and output tokens a usage line bills, whichever
// API Shape wrote it: input_tokens, with the Messages API's
// cache_creation_input_tokens and cache_read_input_tokens added, or
// prompt_tokens; output_tokens or completion_tokens. Usage it cannot read
// bills nothing.
func Tokens(usage json.RawMessage) (input, output int64) {
	var u struct {
		Input       int64 `json:"input_tokens"`
		CacheWrite  int64 `json:"cache_creation_input_tokens"`
		CacheRead   int64 `json:"cache_read_input_tokens"`
		Prompt      int64 `json:"prompt_tokens"`
		Output      int64 `json:"output_tokens"`
		Completions int64 `json:"completion_tokens"`
	}
	if json.Unmarshal(usage, &u) != nil {
		return 0, 0
	}
	return u.Input + u.CacheWrite + u.CacheRead + u.Prompt, u.Output + u.Completions
}

// ReadAll is every Transcript of alias's Conversations, Group-level talk
// included, as ReadByConversation reads them, in one list.
func ReadAll(p nodedir.Paths, alias string) ([]Transcript, error) {
	convs, err := ReadByConversation(p, alias)
	if err != nil {
		return nil, err
	}
	var out []Transcript
	for _, c := range convs {
		out = append(out, c.Transcripts...)
	}
	return out, nil
}

// Conversation is the Transcripts of one Conversation as ReadConversation
// reads them, with its ID, nil for Group-level talk.
type Conversation struct {
	ID          []byte
	Transcripts []Transcript
}

// ReadByConversation is the Transcripts of alias's Conversations,
// Group-level talk included, one Conversation per directory, in directory
// name order; a directory that names no Conversation is passed over.
func ReadByConversation(p nodedir.Paths, alias string) ([]Conversation, error) {
	convs, err := os.ReadDir(p.ConversationsDir(alias))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Conversation
	for _, c := range convs {
		if !c.IsDir() {
			continue
		}
		var id []byte
		if c.Name() != nodedir.ConversationName(nil) {
			if id, err = hex.DecodeString(c.Name()); err != nil || nodedir.ConversationName(id) != c.Name() {
				continue
			}
		}
		ts, err := readDir(p.TranscriptsDir(alias, c.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, Conversation{ID: id, Transcripts: ts})
	}
	return out, nil
}
