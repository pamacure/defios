package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/key"
	"defios.com/internal/node"
)

// agentFlags are the fields add and edit take.
type agentFlags struct {
	provider string
	model    string
	body     string
	maxTurns int
}

func (f *agentFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.provider, "provider", "", "the alias of the Provider it runs on, a [providers.<alias>] table each Group including it must have")
	cmd.Flags().StringVar(&f.model, "model", "", "the model it asks for: body.model, over --body")
	cmd.Flags().StringVar(&f.body, "body", "", "its request body, a JSON object: inline, @path to read a file, or - for stdin")
	cmd.Flags().IntVar(&f.maxTurns, "max-turns", 0, "model calls a summon may make (default 8)")
}

// edit is the node.ProfileEdit of each field whose flag cmd was given.
func (f *agentFlags) edit(g *globals, cmd *cobra.Command) (node.ProfileEdit, error) {
	var e node.ProfileEdit
	fl := cmd.Flags()
	if fl.Changed("provider") {
		e.Provider = &f.provider
	}
	if fl.Changed("max-turns") {
		if f.maxTurns < 1 {
			return e, exitUsagef("--max-turns %d must be a positive integer", f.maxTurns)
		}
		e.MaxTurns = &f.maxTurns
	}
	if fl.Changed("body") {
		body, err := readBody(g, f.body)
		if err != nil {
			return e, exitUsagef("%w", err)
		}
		e.Body = body
	}
	if fl.Changed("model") {
		e.Model = &f.model
	}
	return e, nil
}

// agentError maps a failure of the library's Agent Profile operations on
// handle: what is refused is a usage error with the CLI's own hint, the
// rest operational.
func agentError(handle string, err error) error {
	var exit *ExitError
	var invalid *node.InvalidError
	var included *node.ProfileIncludedError
	switch {
	case errors.As(err, &exit):
		return err
	case errors.Is(err, node.ErrUnknownProfile):
		return exitUsagef("no Agent Profile %q on this Node; add it with `defios agent add`", handle)
	case errors.Is(err, node.ErrProfileExists):
		return exitUsagef("Agent Profile %q already exists; change it with `defios agent edit`", handle)
	case errors.Is(err, node.ErrProfileUnchanged):
		return exitUsagef("Agent Profile %q is unchanged: nothing to change", handle)
	case errors.As(err, &included):
		return exitUsagef("Agent Profile %q is included in %s; exclude it there first", handle, strings.Join(placeNames(included.In), ", "))
	case errors.As(err, &invalid), errors.Is(err, config.ErrNotWritten), errors.Is(err, config.ErrGrantRefused),
		errors.Is(err, key.ErrUnknown), errors.Is(err, key.ErrAmbiguous):
		return exitUsagef("%w", err)
	}
	return talkError(err)
}

// placeNames names each place as the CLI prints it: a Group by its alias,
// a Conversation as <alias>/<workspace>.
func placeNames(in []node.Place) []string {
	out := make([]string, len(in))
	for i, pl := range in {
		out[i] = pl.Group
		if pl.Workspace != "" {
			out[i] += "/" + pl.Workspace
		}
	}
	return out
}

func agentCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "agent",
		Short: "Configure the Agent Profiles this Node can summon into a Group's talk",
		Long: `An Agent Profile is this Node's own, never shared with the Group: a directory agents/<handle>/ under the
config root holding its system prompt, SYSTEM.md, and its config.toml naming the Provider it runs on,
its request body and max_turns (ADR 0029). It is the whole Node's, named by its Handle alone, and
answers only where a Group's or Conversation's [agent] profiles includes its Handle, on that Group's
Provider of its alias. These commands need no serve.`,
	}

	var add agentFlags
	addCmd := &cobra.Command{
		Use:   "add <handle>",
		Short: "Add an Agent Profile: write agents/<handle>/config.toml and scaffold its SYSTEM.md",
		Long: `Add an Agent Profile running on a Provider alias. Its SYSTEM.md is written with a placeholder prompt unless
one is already there. It is not included anywhere until you include its Handle, and each Group that
includes it must define its Provider.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("provider") {
				return exitUsagef("--provider is required: the alias of the Provider it runs on")
			}
			e, err := add.edit(g, cmd)
			if err != nil {
				return err
			}
			h, err := openNode(g)
			if err != nil {
				return err
			}
			a, err := h.AddAgentProfile(cmd.Context(), handle, e)
			if err != nil {
				return agentError(handle, err)
			}
			if g.json {
				return g.emitJSON(agentViewOf(a))
			}
			fmt.Fprintf(g.app.Stdout, "added %s\n%s\ninclude it with: defios agent include %s\n",
				filepath.Join(a.Dir, "config.toml"), filepath.Join(a.Dir, "SYSTEM.md"), handle)
			return nil
		},
	}
	add.bind(addCmd)

	var edit agentFlags
	editCmd := &cobra.Command{
		Use:   "edit <handle>",
		Short: "Change an Agent Profile's Provider, model, body or max_turns; unspecified fields keep their value",
		Long: `Change an Agent Profile's config.toml. --body replaces its body whole; --model sets only body.model, over
the new body when both are given. A Handle cannot be renamed: add another and delete this one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			e, err := edit.edit(g, cmd)
			if err != nil {
				return err
			}
			h, err := openNode(g)
			if err != nil {
				return err
			}
			a, err := h.EditAgentProfile(cmd.Context(), handle, e)
			if err != nil {
				return agentError(handle, err)
			}
			if g.json {
				return g.emitJSON(agentViewOf(a))
			}
			fmt.Fprintf(g.app.Stdout, "edited %s\n", filepath.Join(a.Dir, "config.toml"))
			return nil
		},
	}
	edit.bind(editCmd)

	var keyQuery string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the Agent Profiles on this Node: Handle, Provider, model, and whether the Group includes it",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runAgentList(cmd.Context(), g, keyQuery) },
	}
	bindGroupFlag(g, listCmd)
	listCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)
	showCmd := &cobra.Command{
		Use:   "show <handle>",
		Short: "Show one Agent Profile and the Groups and Conversations on this Node that include it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			return runAgentShow(cmd.Context(), g, handle)
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete <handle>",
		Short: "Delete an Agent Profile's directory, its config.toml and SYSTEM.md",
		Long: `Delete agents/<handle>/ under the config root. The profile is the whole Node's, so delete refuses, changing
nothing, while any Group or Conversation on this Node includes its Handle: exclude it there first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			h, err := openNode(g)
			if err != nil {
				return err
			}
			if err := h.DeleteAgentProfile(cmd.Context(), handle); err != nil {
				return agentError(handle, err)
			}
			dir := g.paths().AgentProfileDir(handle)
			if g.json {
				return g.emitJSON(struct {
					Handle  string `json:"handle"`
					Deleted string `json:"deleted"`
				}{handle, dir})
			}
			fmt.Fprintf(g.app.Stdout, "deleted %s\n", dir)
			return nil
		},
	}

	includeCmd := &cobra.Command{
		Use:   "include <handle>",
		Short: "Include an agent in Group-level talk, or with --key in that Conversation, and print who is mentionable there",
		Long: `Add the Handle of an Agent Profile on this Node to [agent] profiles: the Group's, or with --key the Conversation's.
A Conversation with no list of its own follows the Group's; including one there gives it a list of
that Handle alone, and it no longer follows the Group's until its profiles line is deleted by hand.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			return runAgentInclusion(cmd.Context(), g, keyQuery, handle, true)
		},
	}
	excludeCmd := &cobra.Command{
		Use:   "exclude <handle>",
		Short: "Exclude an agent from Group-level talk, or with --key from that Conversation, and print who is mentionable there",
		Long: `Take a Handle out of [agent] profiles: the Group's, or with --key the Conversation's. Excluding the last
leaves an empty list, so no agent answers there. A Conversation with no list of its own follows the
Group's, and exclude --key refuses it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			return runAgentInclusion(cmd.Context(), g, keyQuery, handle, false)
		},
	}
	for _, cmd := range []*cobra.Command{includeCmd, excludeCmd} {
		bindWriteGroupFlag(g, cmd)
		cmd.Flags().StringVar(&keyQuery, "key", "", keyEditFlagHelp)
	}

	var secrets []string
	grantCmd := &cobra.Command{
		Use:   "grant <handle> <tool | plugin/skill | plugin/server> [--secret NAME]...",
		Short: "Grant an Agent Profile a built-in Tool, a Plugin's Skill or MCP Server, and print what it is granted of that kind",
		Long: `Add a Tool to an Agent Profile's tools, the whole list of built-in Tools it may call (ADR 0029), a
<plugin>/<skill> to its skills, the Skills it may read with skill_load, or a <plugin>/<server> to its mcp,
the MCP Servers whose tools it may call (ADR 0030). An unset tools is say alone, so granting into it writes
say with the new Tool. The Tools are ` + strings.Join(config.Tools, ", ") + `; the sign Tools are offered
only in a Key's Conversation, never in Group-level talk. A Skill or MCP Server must be of a Plugin installed
or built in, and a name that is both is refused as ambiguous.

An MCP Server runs with PATH, HOME and nothing else of the serve's environment but each --secret, a Secret
its server asks for, set under its own name. Granting a server again sets its Secrets to those given.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentGrant(cmd.Context(), g, args, true, secrets)
		},
	}
	grantCmd.Flags().StringArrayVar(&secrets, "secret", nil, "a Secret the MCP Server's process gets, by NAME; repeatable")
	revokeCmd := &cobra.Command{
		Use:   "revoke <handle> <tool | plugin/skill | plugin/server>",
		Short: "Revoke a built-in Tool, a Skill or an MCP Server from an Agent Profile, and print what it is granted of that kind",
		Long: `Take a Tool out of an Agent Profile's tools, a <plugin>/<skill> out of its skills, or a <plugin>/<server>
out of its mcp. Revoking say leaves an agent that cannot post to the talk; revoking every Tool leaves an
empty list, an agent that can only end its turn. A Skill or MCP Server it grants can be revoked though its
Plugin is no longer installed.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentGrant(cmd.Context(), g, args, false, nil)
		},
	}

	root.AddCommand(addCmd, listCmd, showCmd, editCmd, deleteCmd, includeCmd, excludeCmd, grantCmd, revokeCmd)
	return root
}

// runAgentGrant grants the item args[1] to the Agent Profile args[0], or
// revokes it, with secrets for an MCP Server, and prints what it is granted
// of that kind.
func runAgentGrant(ctx context.Context, g *globals, args []string, grant bool, secrets []string) error {
	handle, err := handleArg(args)
	if err != nil {
		return err
	}
	h, err := openNode(g)
	if err != nil {
		return err
	}
	var kind node.GrantKind
	var a node.AgentProfile
	if grant {
		kind, a, err = h.Grant(ctx, handle, args[1], secrets)
	} else {
		kind, a, err = h.Revoke(ctx, handle, args[1])
	}
	if err != nil {
		return agentError(handle, err)
	}
	switch kind {
	case node.GrantSkill:
		return printList(g, a.Skills)
	case node.GrantMCP:
		return printMCP(g, a.MCP)
	}
	return printList(g, a.Tools)
}

// printMCP prints the MCP Server grants mcp, one a line as
// "<plugin>/<server>" with its Secrets in parentheses, or as a JSON object
// as the profile holds them.
func printMCP(g *globals, mcp map[string]node.MCPGrant) error {
	if g.json {
		return g.emitJSON(mcpView(mcp))
	}
	for _, line := range mcpLines(mcp, func(s string) string { return s }) {
		fmt.Fprintln(g.app.Stdout, line)
	}
	return nil
}

// mcpLines are the MCP Server grants mcp, sorted, each "<plugin>/<server>"
// with its Secrets in parentheses, styled by paren.
func mcpLines(mcp map[string]node.MCPGrant, paren func(string) string) []string {
	var out []string
	for _, ref := range slices.Sorted(maps.Keys(mcp)) {
		if s := mcp[ref].Secrets; len(s) > 0 {
			ref += " " + paren("("+strings.Join(s, ", ")+")")
		}
		out = append(out, ref)
	}
	return out
}

// mcpGrantView is one MCP Server grant as JSON.
type mcpGrantView struct {
	Secrets []string `json:"secrets"`
}

// mcpView is the MCP Server grants mcp as JSON, never null.
func mcpView(mcp map[string]node.MCPGrant) map[string]mcpGrantView {
	out := map[string]mcpGrantView{}
	for ref, gr := range mcp {
		s := gr.Secrets
		if s == nil {
			s = []string{}
		}
		out[ref] = mcpGrantView{Secrets: s}
	}
	return out
}

// printList prints list one a line, or as a JSON array.
func printList(g *globals, list []string) error {
	if list == nil {
		list = []string{}
	}
	if g.json {
		return g.emitJSON(list)
	}
	for _, x := range list {
		fmt.Fprintln(g.app.Stdout, x)
	}
	return nil
}

// runAgentInclusion includes handle in, or excludes it from, the Group's
// [agent] profiles, or with keyQuery the Conversation's, and prints the
// list that results.
func runAgentInclusion(ctx context.Context, g *globals, keyQuery, handle string, include bool) error {
	h, err := openNode(g)
	if err != nil {
		return err
	}
	alias, err := g.resolveAlias()
	if err != nil {
		return err
	}
	if include {
		// An unknown Handle is refused before the Key query is.
		if _, err := h.AgentProfile(ctx, handle); err != nil {
			return agentError(handle, err)
		}
	}
	conv, err := queriedConversation(g, h, keyQuery)
	if err != nil {
		return err
	}
	var in node.Inclusion
	if include {
		in, err = h.IncludeAgent(ctx, alias, conv, handle)
	} else {
		in, err = h.ExcludeAgent(ctx, alias, conv, handle)
	}
	if errors.Is(err, node.ErrFollowsGroup) {
		return exitUsagef("%s follows the Group's list; exclude it there, or give %s its own list with include --key", keyQuery, keyQuery)
	} else if err != nil {
		return agentError(handle, err)
	}
	if in.OwnList {
		fmt.Fprintf(g.app.Stderr, "%s %s now has its own list of agents and no longer follows the Group's\n",
			paletteFor(g.app.Stderr, g.app.Getenv).caution("warning:"), keyQuery)
	}
	if g.json {
		return g.emitJSON(in.Handles)
	}
	for _, x := range in.Handles {
		fmt.Fprintln(g.app.Stdout, x)
	}
	return nil
}

func runAgentList(ctx context.Context, g *globals, keyQuery string) error {
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
	included, err := h.IncludedAgents(ctx, alias, conv)
	if err != nil {
		return agentError("", err)
	}
	profiles, err := h.AgentProfiles(ctx)
	if err != nil {
		return agentError("", err)
	}
	type listed struct {
		agentView
		Included bool `json:"included"`
	}
	views := make([]listed, len(profiles))
	for i, a := range profiles {
		views[i] = listed{agentViewOf(a), slices.Contains(included, a.Handle)}
	}
	if g.json {
		return g.emitJSON(views)
	}
	out := g.out()
	for _, v := range views {
		model := v.Model
		if model == "" {
			model = "-"
		}
		fmt.Fprintf(g.app.Stdout, "%s  %s  %s", out.primary(v.Handle), v.Provider, model)
		if v.Included {
			fmt.Fprint(g.app.Stdout, "  [included]")
		}
		fmt.Fprintln(g.app.Stdout)
	}
	return nil
}

func runAgentShow(ctx context.Context, g *globals, handle string) error {
	h, err := openNode(g)
	if err != nil {
		return err
	}
	a, err := h.AgentProfile(ctx, handle)
	if err != nil {
		return agentError(handle, err)
	}
	in := placeNames(a.IncludedIn)
	v := struct {
		agentView
		IncludedIn []string `json:"included_in"`
	}{agentViewOf(a), in}
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "handle %s\nprovider %s\n", g.out().primary(v.Handle), v.Provider)
	if v.MaxTurns != 0 {
		fmt.Fprintf(g.app.Stdout, "max_turns %d\n", v.MaxTurns)
	}
	tools := g.out().caution("none")
	if len(v.Tools) > 0 {
		tools = strings.Join(v.Tools, ", ")
	}
	skills := g.out().caution("none")
	if len(v.Skills) > 0 {
		skills = strings.Join(v.Skills, ", ")
	}
	mcp := g.out().caution("none")
	if lines := mcpLines(a.MCP, g.out().dim); len(lines) > 0 {
		mcp = strings.Join(lines, ", ")
	}
	fmt.Fprintf(g.app.Stdout, "tools %s\nskills %s\nmcp %s\nprofile_dir %s\n", tools, skills, mcp, v.Dir)
	if v.Body != nil {
		body, err := json.Marshal(v.Body)
		if err != nil {
			return exitOperational(err)
		}
		fmt.Fprintf(g.app.Stdout, "body %s\n", body)
	}
	out := g.out()
	where := out.caution("none")
	if len(in) > 0 {
		names := make([]string, len(in))
		for i, x := range in {
			names[i] = out.primary(x)
		}
		where = strings.Join(names, ", ")
	}
	fmt.Fprintf(g.app.Stdout, "included_in %s\n", where)
	return nil
}

// handleArg is the Handle a verb's one argument names, refused unless valid.
func handleArg(args []string) (string, error) {
	if err := config.ValidateHandle(args[0]); err != nil {
		return "", exitUsagef("%w", err)
	}
	return args[0], nil
}

// readBody is the JSON object arg holds inline, in the file @path names, or
// on stdin for -. Its numbers are int64 where whole, as TOML reads them.
func readBody(g *globals, arg string) (map[string]any, error) {
	raw := []byte(arg)
	var err error
	switch {
	case arg == "-":
		raw, err = io.ReadAll(g.app.Stdin)
	case len(arg) > 0 && arg[0] == '@':
		raw, err = os.ReadFile(arg[1:])
	}
	if err != nil {
		return nil, fmt.Errorf("--body: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil, errors.New("--body must be a JSON object")
	}
	body, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("--body must be a JSON object")
	}
	return wholeNumbers(body).(map[string]any), nil
}

// wholeNumbers is v with each json.Number an int64 where it is whole, else
// a float64.
func wholeNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = wholeNumbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = wholeNumbers(e)
		}
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		f, _ := x.Float64()
		return f
	}
	return v
}

// agentView is one Agent Profile, human or JSON.
type agentView struct {
	Handle   string                  `json:"handle"`
	Provider string                  `json:"provider"`
	Model    string                  `json:"model,omitempty"`
	MaxTurns int                     `json:"max_turns,omitempty"`
	Tools    []string                `json:"tools"`  // the Tools it is granted
	Skills   []string                `json:"skills"` // the Skills it is granted, <plugin>/<skill>
	MCP      map[string]mcpGrantView `json:"mcp"`    // the MCP Servers it is granted, by <plugin>/<server>
	Dir      string                  `json:"profile_dir"`
	Body     map[string]any          `json:"body,omitempty"`
}

func agentViewOf(a node.AgentProfile) agentView {
	return agentView{Handle: a.Handle, Provider: a.Provider, Model: a.Model, MaxTurns: a.MaxTurns, Tools: a.Tools, Skills: a.Skills,
		MCP: mcpView(a.MCP), Dir: a.Dir, Body: a.Body}
}
