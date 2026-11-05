package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"defios.com/internal/agent"
	"defios.com/internal/serve"
	"defios.com/internal/transcript"
)

// runConversationTranscript prints the Transcript of the chain whose
// msg_id starts with prefix, matched as conversation cancel matches it,
// among the chains with a Transcript in the Conversation keyQuery names.
// It reads the files, so it needs no serve.
func runConversationTranscript(g *globals, keyQuery, prefix string) error {
	p, err := chainPrefix(prefix)
	if err != nil {
		return err
	}
	if p == "" {
		return exitUsagef("name the chain: its msg id, or a unique prefix of it")
	}
	alias, err := g.resolveAlias()
	if err != nil {
		return err
	}
	conv, err := queriedConversation(g, keyQuery)
	if err != nil {
		return err
	}
	paths := g.paths()
	chains, err := transcript.List(paths, alias, conv)
	if err != nil {
		return exitOperational(err)
	}
	chain, err := serve.MatchChain(p, chains)
	if errors.Is(err, serve.ErrNotAChain) || errors.Is(err, serve.ErrAmbiguousChain) {
		return exitUsagef("%w", err)
	}
	if err != nil {
		return err
	}
	t, err := transcript.Read(paths, alias, conv, chain)
	if err != nil {
		return exitOperational(err)
	}
	if g.json {
		return emitJSONL(g, t.Lines)
	}
	printRuns(g, t.Runs())
	return nil
}

// printRuns prints each run: its step, Handle, depth and, in an older
// Transcript, attempt; its output as conversation follow labels it,
// thinking, tool calls and their results dimmed; then how it ended, its
// outcome colored as a status.
func printRuns(g *globals, runs []transcript.Run) {
	p := g.out()
	out := g.app.Stdout
	for _, r := range runs {
		fmt.Fprintf(out, "step %d: %s, depth %d", r.Step, p.primary("@"+r.Handle), r.Depth)
		if r.Attempt > 0 {
			fmt.Fprintf(out, ", attempt %d", r.Attempt)
		}
		fmt.Fprintln(out)
		at := "@" + r.Handle
		printBlocks(g, "", at, r.Output())
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
