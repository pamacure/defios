package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"defios.com/internal/agent"
	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/transcript"
)

// runConversationTranscript prints the runs of the chain whose msg_id
// starts with prefix, matched as conversation cancel matches it, among the
// chains with a run in the Transcripts of the Conversation keyQuery names.
// It reads the files, so it needs no serve.
func runConversationTranscript(g *globals, keyQuery, prefix string) error {
	p, err := chainPrefix(prefix)
	if err != nil {
		return err
	}
	if p == "" {
		return exitUsagef("name the chain: its msg id, or a unique prefix of it")
	}
	h, err := openNode(g)
	if err != nil {
		return err
	}
	alias, err := g.resolveAlias()
	if err != nil {
		return err
	}
	conv, err := queriedConversation(g, h, keyQuery)
	if err != nil {
		return err
	}
	t, err := h.Transcript(context.Background(), alias, conv, p)
	var notFound *node.ChainNotFoundError
	if errors.As(err, &notFound) {
		return exitUsagef("%w%s", notFound, elsewhereHints(notFound))
	}
	if errors.Is(err, serve.ErrNotAChain) || errors.Is(err, serve.ErrAmbiguousChain) {
		return exitUsagef("%w", err)
	}
	if err != nil {
		return talkError(err)
	}
	if g.json {
		return emitJSONL(g, t.Lines)
	}
	printRuns(g, t.Runs)
	return nil
}

// elsewhereHints say, for each chain the prefix of e names elsewhere on
// this Node, where it is and the flags that find it.
func elsewhereHints(e *node.ChainNotFoundError) string {
	var b strings.Builder
	for _, at := range e.Elsewhere {
		fmt.Fprintf(&b, "; %s is in %s: %s", at.Chain, at.ChainPlace, findFlags(e.Searched, at.ChainPlace))
	}
	return b.String()
}

// findFlags are the flag changes that make conversation transcript read
// at rather than searched.
func findFlags(searched, at node.ChainPlace) string {
	var pass []string
	if at.Group != searched.Group {
		pass = append(pass, "--group "+at.Group)
	}
	if at.Conversation != node.GroupTalk {
		pass = append(pass, "--key "+at.Conversation)
	}
	var changes []string
	if len(pass) > 0 {
		changes = append(changes, "pass "+strings.Join(pass, " "))
	}
	if at.Conversation == node.GroupTalk && searched.Conversation != node.GroupTalk {
		changes = append(changes, "drop --key")
	}
	return strings.Join(changes, " and ")
}

// clearLine is `conversation clear`'s result: the Handles of the agents
// whose live Transcript it closed.
type clearLine struct {
	Cleared []string `json:"cleared"`
}

// runConversationClear clears the agent handle, or unless byAgent (--agent
// was passed) every agent, in the Conversation keyQuery names: it closes
// their live Transcripts, which are files of this Node's, so it needs no
// serve (ADR 0036).
func runConversationClear(g *globals, keyQuery, handle string, byAgent bool) error {
	handle = strings.TrimPrefix(handle, "@")
	if byAgent && handle == "" {
		return exitUsagef("--agent: name the agent's Handle")
	}
	h, err := openNode(g)
	if err != nil {
		return err
	}
	alias, err := g.resolveAlias()
	if err != nil {
		return err
	}
	conv, err := queriedConversation(g, h, keyQuery)
	if err != nil {
		return err
	}
	cleared, err := h.ClearTranscripts(context.Background(), alias, conv, handle)
	if err != nil {
		return talkError(err)
	}
	if g.json {
		return g.emitJSON(clearLine{Cleared: append([]string{}, cleared...)})
	}
	out := g.app.Stdout
	switch {
	case len(cleared) == 0 && byAgent:
		fmt.Fprintf(out, "@%s has no live Transcript here\n", handle)
	case len(cleared) == 0:
		fmt.Fprintln(out, "no agent has a live Transcript here")
	case len(cleared) == 1:
		fmt.Fprintf(out, "cleared @%s's Transcript\n", cleared[0])
	default:
		fmt.Fprintf(out, "cleared the Transcripts of @%s\n", strings.Join(cleared, ", @"))
	}
	return nil
}

// printRuns prints each run: its step, Handle and depth; its output as
// conversation follow labels it, thinking, tool calls and their results
// dimmed; what its sign Tools did; then how it ended, its outcome colored
// as a status.
func printRuns(g *globals, runs []transcript.Run) {
	p := g.out()
	out := g.app.Stdout
	for _, r := range runs {
		fmt.Fprintf(out, "step %d: %s, depth %d\n", r.Step, p.primary("@"+r.Handle), r.Depth)
		at := "@" + r.Handle
		printBlocks(g, "", at, r.Output())
		for _, a := range r.Acted {
			fmt.Fprintf(out, "%s %s on Sign Request %s\n", at, a.Tool, shortChain(a.MsgID))
		}
		switch r.Outcome {
		case transcript.OutcomeSaid:
			fmt.Fprintf(out, "%s said msg_id %s\n", at, shortChain(r.MsgID))
		case transcript.OutcomeSilent:
			fmt.Fprintln(out, at+" stayed silent")
		case transcript.OutcomeError:
			fmt.Fprintf(out, "%s %s: %s\n", at, p.status("failed"), r.Error)
		case transcript.OutcomeCancelled:
			fmt.Fprintf(out, "%s was %s\n", at, p.status("cancelled"))
		default:
			fmt.Fprintf(out, "%s was %s by a stop\n", at, p.status("cut off"))
		}
	}
}

// printBlocks prints each block that shows anything as `<at> (<kind>):
// <text>` after indent, thinking, tool calls and their results dimmed.
func printBlocks(g *globals, indent, at string, blocks []json.RawMessage) {
	p := g.out()
	for _, raw := range blocks {
		kind, text := blockOutput(raw)
		if text == "" {
			continue
		}
		show := p.dim
		if kind == agent.KindText {
			show = func(s string) string { return s }
		}
		fmt.Fprintln(g.app.Stdout, indent+show(at+" ("+kind+"):")+" "+show(text))
	}
}

// blockOutput is an output block as it streamed, or a tool call's result:
// its kind and text, "" for one that shows nothing, such as redacted
// thinking.
func blockOutput(raw json.RawMessage) (kind, text string) {
	var b struct {
		agent.Block
		Content string `json:"content"` // a tool result's
	}
	if json.Unmarshal(raw, &b) != nil {
		return "", ""
	}
	switch b.Type {
	case "tool_result":
		return "tool result", b.Content
	case "text":
		return agent.KindText, b.Text
	case "thinking", "reasoning":
		return agent.KindThinking, b.Thinking
	case "tool_use":
		return agent.KindTool, b.Name + " " + string(b.Input)
	}
	return "", ""
}
