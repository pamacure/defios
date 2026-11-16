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
	"encoding/json"
	"errors"
	"fmt"
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
	KindRequest = agent.RecordRequest // request: a request body as sent, never its headers
	KindBlock   = agent.RecordBlock   // block: one output block, or a tool call's result
	KindUsage   = agent.RecordUsage   // usage: a model call's usage
	KindActed   = "acted"             // tool and msg_id: a sign Tool acted on that Sign Request
	KindOutcome = "outcome"           // outcome, and msg_id and seq or error with it
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
	// Kind is KindRequest, KindBlock, KindUsage, KindActed or KindOutcome.
	Kind    string          `json:"kind"`
	Request json.RawMessage `json:"request,omitempty"`
	Block   json.RawMessage `json:"block,omitempty"`
	Usage   json.RawMessage `json:"usage,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Outcome string          `json:"outcome,omitempty"`
	MsgID   string          `json:"msg_id,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// Outcome is the closing line of a run that said msgID, hex, at seq
// (msgID "" if nothing), and ended with err; a run that commits nothing
// and ends without an error is silent. Once a talk_write has committed it is
// `said`, whatever failed after. A run cut off by the serve stopping
// before that has none: ok is false.
func Outcome(msgID string, seq uint64, err error, cancelled, stopped bool) (l Line, ok bool) {
	l.Kind = KindOutcome
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
				out = append(out, Transcript{Handle: a.Name(), Lines: lines})
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
	var lines []Line
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		var l Line
		if json.Unmarshal(line, &l) != nil || l.Step < 1 || l.Chain == "" {
			continue
		}
		lines = append(lines, l)
	}
	return lines, nil
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
	// Requests are the request bodies of its model calls as sent, Blocks
	// its output blocks and the results of its tool calls, and Usage each
	// call's usage, in order.
	Requests []json.RawMessage
	Blocks   []json.RawMessage
	Usage    []json.RawMessage
	// Acted is what its sign Tools did, in order (ADR 0029).
	Acted   []Acted
	Outcome string
	MsgID   string // with OutcomeSaid: what it said, hex
	Seq     uint64 // with OutcomeSaid, once it has a row
	Error   string // with OutcomeError: why
}

// Acted is one successful call of a sign Tool: the Tool, and the Sign
// Request it acted on, hex.
type Acted struct {
	Tool  string
	MsgID string
}

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
		case KindRequest:
			r.Requests = append(r.Requests, l.Request)
		case KindBlock:
			r.Blocks = append(r.Blocks, l.Block)
		case KindUsage:
			r.Usage = append(r.Usage, l.Usage)
		case KindActed:
			r.Acted = append(r.Acted, Acted{Tool: l.Tool, MsgID: l.MsgID})
		case KindOutcome:
			r.Outcome, r.MsgID, r.Seq, r.Error = l.Outcome, l.MsgID, l.Seq, l.Error
		}
	}
	return runs
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
// included, as ReadConversation reads each.
func ReadAll(p nodedir.Paths, alias string) ([]Transcript, error) {
	convs, err := os.ReadDir(p.ConversationsDir(alias))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Transcript
	for _, c := range convs {
		if !c.IsDir() {
			continue
		}
		ts, err := readDir(p.TranscriptsDir(alias, c.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	return out, nil
}
