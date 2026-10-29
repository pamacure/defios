package serve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Transcript outcomes: how a run ended (ADR 0018). A run cut off by a serve
// stop has none.
const (
	OutcomeSaid      = "said"      // seq: the message's row
	OutcomeSilent    = "silent"    // ended its turn without say
	OutcomeError     = "error"     // error: why
	OutcomeCancelled = "cancelled" // a cancel stopped it
)

// TranscriptLine is one line of a chain's Transcript. Every line names the
// run it belongs to: its step in the chain's depth-first order (from 1), its
// Handle, depth and attempt (from 1). Kind says which of the rest it holds.
type TranscriptLine struct {
	Step    int    `json:"step"`
	Handle  string `json:"handle"`
	Depth   uint32 `json:"depth"`
	Attempt int    `json:"attempt"`
	// Kind is "request" (a request body as sent, never its headers),
	// "block" (one output block), "usage" (a model call's usage) or
	// "outcome".
	Kind    string          `json:"kind"`
	Request json.RawMessage `json:"request,omitempty"`
	Block   json.RawMessage `json:"block,omitempty"`
	Usage   json.RawMessage `json:"usage,omitempty"`
	Outcome string          `json:"outcome,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// transcript appends a chain's Transcript, plaintext JSONL under the
// Group's data directory, opened on its first line. One chain runs at a
// time per Conversation, so it is the file's one writer. A write that fails
// is logged once and the chain goes on: the Transcript is this Node's
// record, not what the chain is for.
type transcript struct {
	path   string
	logf   func(string, ...any)
	f      *os.File
	failed bool
}

func (t *transcript) write(l TranscriptLine) {
	if t.failed {
		return
	}
	err := t.append(l)
	if err != nil {
		t.failed = true
		t.logf("serve: transcript %s: %v; the rest of this chain is not recorded", t.path, err)
	}
}

func (t *transcript) append(l TranscriptLine) error {
	if t.f == nil {
		if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(t.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		t.f = f
	}
	line, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encoding a line: %w", err)
	}
	_, err = t.f.Write(append(line, '\n'))
	return err
}

func (t *transcript) close() {
	if t.f != nil {
		t.f.Close()
	}
}
