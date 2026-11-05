// Package transcript is a chain's Transcript (ADR 0018): everything its
// agents generated, one JSON line at a time, kept on the Node that ran it
// under the Conversation's transcripts/ and never replicated. serve
// appends it as the chain runs; anything may read it, serve up or down.
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

	"defios.com/internal/agent"
	"defios.com/internal/nodedir"
)

// Outcomes: how a run ended (ADR 0018). A run cut off by a serve stop has
// none.
const (
	OutcomeSaid      = "said"      // msg_id, and seq once it has a row: the message
	OutcomeSilent    = "silent"    // ended its turn without say
	OutcomeError     = "error"     // error: why
	OutcomeCancelled = "cancelled" // a cancel stopped it
)

// Line kinds: which of a Line's fields it holds.
const (
	KindRequest = agent.RecordRequest // request: a request body as sent, never its headers
	KindBlock   = agent.RecordBlock   // block: one output block
	KindUsage   = agent.RecordUsage   // usage: a model call's usage
	KindOutcome = "outcome"           // outcome, and msg_id and seq or error with it
)

// Line is one line of a chain's Transcript. Every line names the run it
// belongs to: its step in the chain's depth-first order (from 1), its
// Handle and depth. Kind says which of the rest it holds.
type Line struct {
	Step   int    `json:"step"`
	Handle string `json:"handle"`
	Depth  uint32 `json:"depth"`
	// Kind is KindRequest, KindBlock, KindUsage or KindOutcome.
	Kind    string          `json:"kind"`
	Request json.RawMessage `json:"request,omitempty"`
	Block   json.RawMessage `json:"block,omitempty"`
	Usage   json.RawMessage `json:"usage,omitempty"`
	Outcome string          `json:"outcome,omitempty"`
	MsgID   string          `json:"msg_id,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// Outcome is the closing line of a run that said msgID, hex, at seq
// (msgID "" if nothing), and ended with err; a run that commits nothing
// and ends without an error is silent. Once a say has committed it is
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

// path is the Transcript of chain in conv (nil for Group-level talk).
func path(p nodedir.Paths, alias string, conv []byte, chain string) string {
	return p.TranscriptFile(alias, nodedir.ConversationName(conv), chain)
}

// Writer appends a chain's Transcript, opened on its first line. One chain
// runs at a time per Conversation, so it is the file's one writer. A write
// that fails is logged once and the chain goes on: the Transcript is this
// Node's record, not what the chain is for.
type Writer struct {
	path   string
	logf   func(string, ...any)
	f      *os.File
	failed bool
}

// NewWriter is a Writer of the Transcript of chain, a hex msg_id, in conv
// (nil for Group-level talk), that logs a failed write to logf.
func NewWriter(p nodedir.Paths, alias string, conv []byte, chain string, logf func(string, ...any)) *Writer {
	return &Writer{path: path(p, alias, conv, chain), logf: logf}
}

// Write appends l, unless a write has failed before.
func (w *Writer) Write(l Line) {
	if w.failed {
		return
	}
	if err := w.append(l); err != nil {
		w.failed = true
		w.logf("serve: transcript %s: %v; the rest of this chain is not recorded", w.path, err)
	}
}

func (w *Writer) append(l Line) error {
	if w.f == nil {
		if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		w.f = f
	}
	line, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encoding a line: %w", err)
	}
	_, err = w.f.Write(append(line, '\n'))
	return err
}

// Close closes the file, if a line opened it.
func (w *Writer) Close() {
	if w.f != nil {
		w.f.Close()
	}
}

// Transcript is a chain's Transcript as read.
type Transcript struct {
	// Lines are its lines as stored, in order.
	Lines []Line
}

// Read is the Transcript of chain in conv (nil for Group-level talk);
// empty if it has none. A line it cannot read, such as one cut off by a
// stop, is passed over.
func Read(p nodedir.Paths, alias string, conv []byte, chain string) (Transcript, error) {
	var t Transcript
	data, err := os.ReadFile(path(p, alias, conv, chain))
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		var l Line
		if json.Unmarshal(line, &l) != nil || l.Step < 1 {
			continue
		}
		t.Lines = append(t.Lines, l)
	}
	return t, nil
}

// List is the chains, hex msg_ids, with a Transcript in conv (nil for
// Group-level talk), in name order; nil if there is none.
func List(p nodedir.Paths, alias string, conv []byte) ([]string, error) {
	entries, err := os.ReadDir(p.TranscriptsDir(alias, nodedir.ConversationName(conv)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var chains []string
	for _, e := range entries {
		if id, ok := nodedir.TranscriptChain(e.Name()); ok && e.Type().IsRegular() {
			chains = append(chains, id)
		}
	}
	return chains, nil
}

// Run is one run of one step of a chain, as its lines show it. Outcome is
// "" for a run cut off by a stop.
type Run struct {
	Step   int
	Handle string
	Depth  uint32
	// Requests are the request bodies of its model calls as sent, Blocks
	// its output blocks and Usage each call's usage, in order.
	Requests []json.RawMessage
	Blocks   []json.RawMessage
	Usage    []json.RawMessage
	Outcome  string
	MsgID    string // with OutcomeSaid: what it said, hex
	Seq      uint64 // with OutcomeSaid, once it has a row
	Error    string // with OutcomeError: why
}

// Output is what r generated, in order: its blocks, thinking, text and
// tool calls, each tool call followed by the result the run answered it
// with, an agent.ToolResult, which the next request carried. A call it did
// not answer, as an accepted say, has none.
func (r Run) Output() []json.RawMessage {
	results := agent.ToolResults(r.Requests)
	out := make([]json.RawMessage, 0, len(r.Blocks))
	for _, raw := range r.Blocks {
		out = append(out, raw)
		var b agent.Block
		if json.Unmarshal(raw, &b) != nil || b.Type != "tool_use" {
			continue
		}
		if res, ok := results[b.ID]; ok {
			if j, err := json.Marshal(res); err == nil {
				out = append(out, j)
			}
		}
	}
	return out
}

// Runs are t's runs, one per step, by step.
func (t Transcript) Runs() []Run {
	var runs []Run
	at := map[int]int{}
	for _, l := range t.Lines {
		i, ok := at[l.Step]
		if !ok {
			i = len(runs)
			at[l.Step] = i
			runs = append(runs, Run{Step: l.Step, Handle: l.Handle, Depth: l.Depth})
		}
		r := &runs[i]
		switch l.Kind {
		case KindRequest:
			r.Requests = append(r.Requests, l.Request)
		case KindBlock:
			r.Blocks = append(r.Blocks, l.Block)
		case KindUsage:
			r.Usage = append(r.Usage, l.Usage)
		case KindOutcome:
			r.Outcome, r.MsgID, r.Seq, r.Error = l.Outcome, l.MsgID, l.Seq, l.Error
		}
	}
	slices.SortStableFunc(runs, func(a, b Run) int {
		return cmp.Compare(a.Step, b.Step)
	})
	return runs
}
