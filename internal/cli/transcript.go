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
// msg_id starts with prefix, matched as conversation retry matches it,
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

// printRuns prints each run: its step, Handle, depth and attempt; its
// output as conversation follow labels it, thinking and tool calls
// dimmed; then how it ended.
func printRuns(g *globals, runs []transcript.Run) {
	p := g.out()
	out := g.app.Stdout
	for _, r := range runs {
		fmt.Fprintf(out, "step %d: %s, depth %d, attempt %d\n", r.Step, p.primary("@"+r.Handle), r.Depth, r.Attempt)
		at := "@" + r.Handle
		printBlocks(g, "", at, r.Blocks)
		switch r.Outcome {
		case transcript.OutcomeSaid:
			fmt.Fprintf(out, "%s said msg_id %s\n", at, shortChain(r.MsgID))
		case transcript.OutcomeSilent:
			fmt.Fprintln(out, p.dim(at+" stayed silent"))
		case transcript.OutcomeError:
			fmt.Fprintf(out, "%s failed: %s\n", at, r.Error)
		case transcript.OutcomeCancelled:
			fmt.Fprintln(out, p.dim(at+" was cancelled"))
		default:
			fmt.Fprintln(out, p.dim(at+" was cut off by a stop"))
		}
	}
}

// printBlocks prints each block that streams anything as `<at> (<kind>):
// <text>` after indent, thinking and tool calls dimmed.
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

// blockOutput is an output block as it streamed: its kind and text, ""
// for one that streams nothing, such as redacted thinking.
func blockOutput(raw json.RawMessage) (kind, text string) {
	var b agent.Block
	if json.Unmarshal(raw, &b) != nil {
		return "", ""
	}
	switch b.Type {
	case "text":
		return agent.KindText, b.Text
	case "thinking", "reasoning":
		return agent.KindThinking, b.Thinking
	case "tool_use":
		return agent.KindTool, b.Name + " " + string(b.Input)
	}
	return "", ""
}
