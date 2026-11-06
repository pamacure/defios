package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
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

// apply sets on p each field whose flag cmd was given: --body replaces the
// body whole, and --model sets body.model over it.
func (f *agentFlags) apply(g *globals, cmd *cobra.Command, p *config.Profile) error {
	fl := cmd.Flags()
	if fl.Changed("provider") {
		p.Provider = f.provider
	}
	if fl.Changed("max-turns") {
		if f.maxTurns < 1 {
			return fmt.Errorf("--max-turns %d must be a positive integer", f.maxTurns)
		}
		p.MaxTurns = f.maxTurns
	}
	if fl.Changed("body") {
		body, err := readBody(g, f.body)
		if err != nil {
			return err
		}
		p.Body = body
	}
	if fl.Changed("model") {
		body := map[string]any{"model": f.model}
		for k, v := range p.Body {
			if k != "model" {
				body[k] = v
			}
		}
		p.Body = body
	}
	return nil
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
			var p config.Profile
			if err := add.apply(g, cmd, &p); err != nil {
				return exitUsagef("%w", err)
			}
			path := g.paths().AgentProfileFile(handle)
			if _, err := os.Stat(path); err == nil {
				return exitUsagef("Agent Profile %q already exists; change it with `defios agent edit`", handle)
			} else if !errors.Is(err, os.ErrNotExist) {
				return exitOperational(err)
			}
			if err := writeProfile(path, p); err != nil {
				return err
			}
			system, err := scaffoldSystem(g, handle)
			if err != nil {
				return err
			}
			if g.json {
				return g.emitJSON(agentViewOf(g, handle, p))
			}
			fmt.Fprintf(g.app.Stdout, "added %s\n%s\ninclude it with: defios agent include %s\n", path, system, handle)
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
			cur, err := readProfile(g, handle)
			if err != nil {
				return err
			}
			p := cur
			if err := edit.apply(g, cmd, &p); err != nil {
				return exitUsagef("%w", err)
			}
			if reflect.DeepEqual(p, cur) {
				return exitUsagef("Agent Profile %q is unchanged: nothing to change", handle)
			}
			path := g.paths().AgentProfileFile(handle)
			if err := writeProfile(path, p); err != nil {
				return err
			}
			if g.json {
				return g.emitJSON(agentViewOf(g, handle, p))
			}
			fmt.Fprintf(g.app.Stdout, "edited %s\n", path)
			return nil
		},
	}
	edit.bind(editCmd)

	var keyQuery string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the Agent Profiles on this Node: Handle, Provider, model, and whether the Group includes it",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runAgentList(g, keyQuery) },
	}
	bindGroupFlag(g, listCmd)
	listCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)
	showCmd := &cobra.Command{
		Use:   "show <handle>",
		Short: "Show one Agent Profile and the Groups and Conversations on this Node that include it",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			return runAgentShow(g, handle)
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete <handle>",
		Short: "Delete an Agent Profile's directory, its config.toml and SYSTEM.md",
		Long: `Delete agents/<handle>/ under the config root. The profile is the whole Node's, so delete refuses, changing
nothing, while any Group or Conversation on this Node includes its Handle: exclude it there first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			dir := g.paths().AgentProfileDir(handle)
			if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
				return exitUsagef("%w", noProfile(handle))
			} else if err != nil {
				return exitOperational(err)
			}
			in, err := includers(g, handle)
			if err != nil {
				return err
			}
			if len(in) > 0 {
				return exitUsagef("Agent Profile %q is included in %s; exclude it there first", handle, strings.Join(in, ", "))
			}
			if err := os.RemoveAll(dir); err != nil {
				return exitOperational(err)
			}
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
		RunE: func(_ *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			return runAgentInclusion(g, keyQuery, handle, true)
		},
	}
	excludeCmd := &cobra.Command{
		Use:   "exclude <handle>",
		Short: "Exclude an agent from Group-level talk, or with --key from that Conversation, and print who is mentionable there",
		Long: `Take a Handle out of [agent] profiles: the Group's, or with --key the Conversation's. Excluding the last
leaves an empty list, so no agent answers there. A Conversation with no list of its own follows the
Group's, and exclude --key refuses it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			return runAgentInclusion(g, keyQuery, handle, false)
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
		RunE: func(_ *cobra.Command, args []string) error { return runAgentGrant(g, args, true, secrets) },
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
		RunE: func(_ *cobra.Command, args []string) error { return runAgentGrant(g, args, false, nil) },
	}

	root.AddCommand(addCmd, listCmd, showCmd, editCmd, deleteCmd, includeCmd, excludeCmd, grantCmd, revokeCmd)
	return root
}

// runAgentGrant grants the item args[1] to the Agent Profile args[0], or
// revokes it, with secrets for an MCP Server, and prints what it is granted
// of that kind (config.Profile.Grant and Revoke).
func runAgentGrant(g *globals, args []string, grant bool, secrets []string) error {
	handle, err := handleArg(args)
	if err != nil {
		return err
	}
	p, err := readProfile(g, handle)
	if err != nil {
		return err
	}
	var kind config.GrantKind
	var changed bool
	if grant {
		p, kind, changed, err = p.Grant(g.paths().PluginsDir(), args[1], secrets)
	} else {
		p, kind, changed, err = p.Revoke(g.paths().PluginsDir(), args[1])
	}
	if errors.Is(err, config.ErrGrantRefused) {
		return exitUsagef("%w", err)
	} else if err != nil {
		return exitOperational(err)
	}
	if changed {
		if err := writeProfile(g.paths().AgentProfileFile(handle), p); err != nil {
			return err
		}
	}
	switch kind {
	case config.GrantSkill:
		return printList(g, p.Skills)
	case config.GrantMCP:
		return printMCP(g, p.MCP)
	}
	return printList(g, p.Granted())
}

// printMCP prints the MCP Server grants mcp, one a line as
// "<plugin>/<server>" with its Secrets in parentheses, or as a JSON object
// as the profile holds them.
func printMCP(g *globals, mcp map[string]config.MCPGrant) error {
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
func mcpLines(mcp map[string]config.MCPGrant, paren func(string) string) []string {
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
func mcpView(mcp map[string]config.MCPGrant) map[string]mcpGrantView {
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

// pluginGrants is every grant on this Node of a Skill or MCP Server of
// Plugin name, sorted by Handle, as "<handle> (<plugin>/<name>, ...)".
func pluginGrants(g *globals, name string) ([]string, error) {
	handles, err := profileHandles(g)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, h := range handles {
		p, err := readProfile(g, h)
		if err != nil {
			return nil, err
		}
		if refs := p.PluginGrants(name); len(refs) > 0 {
			out = append(out, fmt.Sprintf("%s (%s)", h, strings.Join(refs, ", ")))
		}
	}
	return out, nil
}

// runAgentInclusion includes handle in, or excludes it from, the Group's
// [agent] profiles, or with keyQuery the Conversation's, and prints the
// list that results.
func runAgentInclusion(g *globals, keyQuery, handle string, include bool) error {
	edit := func(list []string) []string {
		if !include {
			return without(list, handle)
		}
		if contains(list, handle) {
			return list
		}
		return append(list, handle)
	}
	var list []string
	var err error
	if include {
		if _, err := readProfile(g, handle); err != nil {
			return err
		}
	}
	if keyQuery == "" {
		_, err = writeGroup(g, func(c *config.Config) error {
			c.Agent.Profiles = edit(c.Agent.Profiles)
			list = c.Agent.Profiles
			return nil
		})
	} else {
		list, err = editConversationList(g, keyQuery, handle, include, edit)
	}
	if err != nil {
		return err
	}
	if list == nil {
		list = []string{}
	}
	if g.json {
		return g.emitJSON(list)
	}
	for _, h := range list {
		fmt.Fprintln(g.app.Stdout, h)
	}
	return nil
}

// editConversationList applies edit to the [agent] profiles of the
// Conversation keyQuery names, keeping the file's other sections, and
// returns the list. A Conversation with no list follows the Group's: an
// include gives it its own, and warns so; an exclude is refused.
func editConversationList(g *globals, keyQuery, handle string, include bool, edit func([]string) []string) ([]string, error) {
	path, err := conversationConfigFile(g, keyQuery)
	if err != nil {
		return nil, err
	}
	w, err := config.ReadWorkspace(path)
	if err != nil {
		return nil, exitOperational(err)
	}
	following := w.Agent.Profiles == nil
	if following && !include {
		return nil, exitUsagef("%s follows the Group's list; exclude it there, or give %s its own list with include --key", keyQuery, keyQuery)
	}
	w.Agent.Profiles = edit(w.Agent.Profiles)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, exitOperational(err)
	}
	if err := config.WriteWorkspace(path, w); err != nil {
		return nil, exitOperational(err)
	}
	if following {
		fmt.Fprintf(g.app.Stderr, "%s %s now has its own list of agents and no longer follows the Group's\n",
			paletteFor(g.app.Stderr, g.app.Getenv).caution("warning:"), keyQuery)
	}
	return w.Agent.Profiles, nil
}

// includers is every place on this Node that includes handle, sorted: each
// Group whose [agent] profiles lists it, by alias, and each Conversation
// whose own list does, as <alias>/<workspace>.
func includers(g *globals, handle string) ([]string, error) {
	p := g.paths()
	aliases, err := groupAliases(p)
	if err != nil {
		return nil, exitOperational(err)
	}
	var out []string
	for _, a := range aliases {
		c, err := config.ReadFile(p.GroupConfigFile(a))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, exitOperational(err)
		}
		if contains(c.Agent.Profiles, handle) {
			out = append(out, a)
		}
		convs, err := config.ConversationFiles(p, a)
		if err != nil {
			return nil, exitOperational(err)
		}
		for _, path := range convs {
			w, err := config.ReadWorkspace(path)
			if err != nil {
				return nil, exitOperational(err)
			}
			if contains(w.Agent.Profiles, handle) {
				out = append(out, a+"/"+filepath.Base(filepath.Dir(path)))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// without is list less every h, never nil when list is not.
func without(list []string, h string) []string {
	if list == nil {
		return nil
	}
	out := []string{}
	for _, x := range list {
		if x != h {
			out = append(out, x)
		}
	}
	return out
}

// profileHandles is the Handle of every Agent Profile on this Node, sorted:
// each directory under agents/ named by a valid Handle and holding a
// config.toml.
func profileHandles(g *globals) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(g.paths().AgentProfilesDir(), "*", "config.toml"))
	if err != nil {
		return nil, exitOperational(err)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if h := filepath.Base(filepath.Dir(m)); config.ValidateHandle(h) == nil {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out, nil
}

func runAgentList(g *globals, keyQuery string) error {
	e, err := load(g, keyQuery)
	if err != nil {
		return err
	}
	handles, err := profileHandles(g)
	if err != nil {
		return err
	}
	type listed struct {
		agentView
		Included bool `json:"included"`
	}
	views := make([]listed, len(handles))
	for i, h := range handles {
		p, err := readProfile(g, h)
		if err != nil {
			return err
		}
		views[i] = listed{agentViewOf(g, h, p), contains(e.Workspace.Agent.Profiles, h)}
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

func runAgentShow(g *globals, handle string) error {
	p, err := readProfile(g, handle)
	if err != nil {
		return err
	}
	in, err := includers(g, handle)
	if err != nil {
		return err
	}
	if in == nil {
		in = []string{}
	}
	v := struct {
		agentView
		IncludedIn []string `json:"included_in"`
	}{agentViewOf(g, handle, p), in}
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
	if lines := mcpLines(p.MCP, g.out().dim); len(lines) > 0 {
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

// noProfile is the refusal of a Handle with no Agent Profile on this Node.
func noProfile(handle string) error {
	return fmt.Errorf("no Agent Profile %q on this Node; add it with `defios agent add`", handle)
}

// readProfile is handle's Agent Profile config.toml, a usage error when it
// has none.
func readProfile(g *globals, handle string) (config.Profile, error) {
	p, err := config.ReadProfile(g.paths().AgentProfileFile(handle))
	if errors.Is(err, os.ErrNotExist) {
		return config.Profile{}, exitUsagef("%w", noProfile(handle))
	} else if err != nil {
		return config.Profile{}, exitOperational(err)
	}
	return p, nil
}

// writeProfile writes p as the Agent Profile config.toml at path, making
// its directory and removing it again, if it made it, when the write is
// refused; a profile that would not read back is a usage error.
func writeProfile(path string, p config.Profile) error {
	dir := filepath.Dir(path)
	_, statErr := os.Stat(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return exitOperational(err)
	}
	err := config.WriteProfile(path, p)
	if err != nil && errors.Is(statErr, os.ErrNotExist) {
		os.Remove(dir)
	}
	if errors.Is(err, config.ErrNotWritten) {
		return exitUsagef("%w", err)
	} else if err != nil {
		return exitOperational(err)
	}
	return nil
}

// scaffoldSystem writes handle's SYSTEM.md with a placeholder prompt unless
// one is there, and returns its path.
func scaffoldSystem(g *globals, handle string) (string, error) {
	dir := g.paths().AgentProfileDir(handle)
	path := filepath.Join(dir, "SYSTEM.md")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", exitOperational(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return path, nil
	} else if err != nil {
		return "", exitOperational(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "# @%s\n\nYou are @%s, an agent in a DeFiOS Group's talk.\nDescribe your purpose here.\n", handle, handle); err != nil {
		return "", exitOperational(err)
	}
	return path, f.Close()
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

func agentViewOf(g *globals, handle string, p config.Profile) agentView {
	model, _ := p.Body["model"].(string)
	skills := p.Skills
	if skills == nil {
		skills = []string{}
	}
	return agentView{Handle: handle, Provider: p.Provider, Model: model, MaxTurns: p.MaxTurns, Tools: p.Granted(), Skills: skills,
		MCP: mcpView(p.MCP), Dir: g.paths().AgentProfileDir(handle), Body: p.Body}
}
