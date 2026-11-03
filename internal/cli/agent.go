package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
	cmd.Flags().StringVar(&f.provider, "provider", "", "the alias of the Provider it runs on, a [providers.<alias>] table in the Group's config.toml")
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
		Long: `An Agent Profile is this Node's own, never shared with the Group: an [agents.<handle>]
table in the Group's config.toml naming the Provider it runs on, its request body and max_turns, and
a system prompt in agents/<handle>/SYSTEM.md under the config root. That directory is named by the
Handle alone, so every Group on this Node that defines the Handle shares the prompt (ADR 0026). An
agent answers only where [agent] profiles includes its Handle. These commands need no serve.`,
	}

	var add agentFlags
	addCmd := &cobra.Command{
		Use:   "add <handle>",
		Short: "Add an Agent Profile to the Group's config.toml and scaffold its SYSTEM.md",
		Long: `Add an Agent Profile on a Provider the Group's config.toml has. Its SYSTEM.md is written with a placeholder
prompt unless one is already there. It is not included anywhere until you include its Handle.`,
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
			var system string
			path, err := writeGroup(g, func(c *config.Config) error {
				if _, ok := c.Agents[handle]; ok {
					return fmt.Errorf("Agent Profile %q already exists; change it with `defios agent edit`", handle)
				}
				if err := checkProvider(c, p); err != nil {
					return err
				}
				c.Agents[handle] = p
				system, err = scaffoldSystem(g, handle)
				return err
			})
			if err != nil {
				return err
			}
			if g.json {
				return g.emitJSON(agentViewOf(g, handle, p, false))
			}
			fmt.Fprintf(g.app.Stdout, "added %s to %s\n%s\ninclude it with: defios agent include %s\n", handle, path, system, handle)
			return nil
		},
	}
	add.bind(addCmd)
	bindWriteGroupFlag(g, addCmd)

	var edit agentFlags
	editCmd := &cobra.Command{
		Use:   "edit <handle>",
		Short: "Change an Agent Profile's Provider, model, body or max_turns; unspecified fields keep their value",
		Long: `Change an Agent Profile. --body replaces its body whole; --model sets only body.model, over the new body
when both are given. A Handle cannot be renamed: add another and remove this one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			var p config.Profile
			var included bool
			path, err := writeGroup(g, func(c *config.Config) error {
				included = contains(c.Agent.Profiles, handle)
				cur, ok := c.Agents[handle]
				if !ok {
					return noProfile(handle)
				}
				p = cur
				if err := edit.apply(g, cmd, &p); err != nil {
					return err
				}
				if reflect.DeepEqual(p, cur) {
					return fmt.Errorf("Agent Profile %q is unchanged: nothing to change", handle)
				}
				c.Agents[handle] = p
				return checkProvider(c, p)
			})
			if err != nil {
				return err
			}
			if g.json {
				return g.emitJSON(agentViewOf(g, handle, p, included))
			}
			fmt.Fprintf(g.app.Stdout, "edited %s in %s\n", handle, path)
			return nil
		},
	}
	edit.bind(editCmd)
	bindWriteGroupFlag(g, editCmd)

	var keyQuery string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the Agent Profiles: Handle, Provider, model, and whether included",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runAgentList(g, keyQuery) },
	}
	bindGroupFlag(g, listCmd)
	listCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)
	showCmd := &cobra.Command{
		Use:   "show <handle>",
		Short: "Show one Agent Profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			return runAgentShow(g, keyQuery, handle)
		},
	}
	bindGroupFlag(g, showCmd)
	showCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)

	removeCmd := &cobra.Command{
		Use:   "remove <handle>",
		Short: "Remove an Agent Profile from the Group's config.toml and every list that includes it",
		Long: `Remove an Agent Profile's [agents.<handle>] table, and take its Handle out of the Group's [agent] profiles
and out of every Conversation config.toml in the Group that lists it. Its profile directory stays:
see delete.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			changed, err := removeAgent(g, handle)
			if err != nil {
				return err
			}
			return agentRemoved(g, handle, changed, "")
		},
	}
	bindWriteGroupFlag(g, removeCmd)

	deleteCmd := &cobra.Command{
		Use:   "delete <handle>",
		Short: "Remove an Agent Profile and delete its profile directory",
		Long: `Remove an Agent Profile as remove does, then delete agents/<handle>/ under the config root, its SYSTEM.md
with it. The directory is shared by every Group on this Node that defines the Handle (ADR 0026), so
delete refuses, changing nothing, while another Group does: remove it there first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle, err := handleArg(args)
			if err != nil {
				return err
			}
			shared, err := groupsDefining(g, handle)
			if err != nil {
				return err
			}
			if len(shared) > 0 {
				return exitUsagef("Agent Profile %q is also defined in Group %s, which shares its profile directory; remove it there, or remove it here and keep the directory",
					handle, strings.Join(shared, ", "))
			}
			changed, err := removeAgent(g, handle)
			if err != nil {
				return err
			}
			dir := g.paths().AgentProfileDir(handle)
			if err := os.RemoveAll(dir); err != nil {
				return exitOperational(err)
			}
			return agentRemoved(g, handle, changed, dir)
		},
	}
	bindWriteGroupFlag(g, deleteCmd)

	includeCmd := &cobra.Command{
		Use:   "include <handle>",
		Short: "Include an agent in Group-level talk, or with --key in that Conversation, and print who is mentionable there",
		Long: `Add a Handle the Group's config.toml defines to [agent] profiles: the Group's, or with --key the Conversation's.
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

	root.AddCommand(addCmd, listCmd, showCmd, editCmd, removeCmd, deleteCmd, includeCmd, excludeCmd)
	return root
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
	if keyQuery == "" {
		_, err = writeGroup(g, func(c *config.Config) error {
			if _, ok := c.Agents[handle]; !ok {
				return noProfile(handle)
			}
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
	e, err := load(g, "")
	if err != nil {
		return nil, err
	}
	if _, ok := e.Group.Agents[handle]; !ok {
		return nil, exitUsagef("%w", noProfile(handle))
	}
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

// groupsDefining is every Group on this Node but the targeted one whose
// config.toml defines handle, sorted.
func groupsDefining(g *globals, handle string) ([]string, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return nil, err
	}
	p := g.paths()
	aliases, err := configGroupAliases(p)
	if err != nil {
		return nil, exitOperational(err)
	}
	var out []string
	for _, a := range aliases {
		if a == alias {
			continue
		}
		c, err := config.ReadFile(p.GroupConfigFile(a))
		if err != nil {
			return nil, exitOperational(err)
		}
		if _, ok := c.Agents[handle]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// removeAgent deletes handle's table from the targeted Group's config.toml
// and its Handle from every [agent] profiles in the Group: each
// Conversation's that lists it, which keeps its list even emptied, then the
// Group's. It reads every file before it writes any, and writes the Group's
// last, so a failed write leaves the Handle included in fewer places and
// remove can run again. It returns the paths it changed.
func removeAgent(g *globals, handle string) ([]string, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return nil, err
	}
	e, err := load(g, "")
	if err != nil {
		return nil, err
	}
	if _, ok := e.Group.Agents[handle]; !ok {
		return nil, exitUsagef("%w", noProfile(handle))
	}
	convs, err := config.ConversationFiles(g.paths(), alias)
	if err != nil {
		return nil, exitOperational(err)
	}
	var changed []string
	over := map[string]config.Workspace{}
	for _, path := range convs {
		w, err := config.ReadWorkspace(path)
		if err != nil {
			return nil, exitOperational(err)
		}
		if contains(w.Agent.Profiles, handle) {
			w.Agent.Profiles = without(w.Agent.Profiles, handle)
			over[path] = w
			changed = append(changed, path)
		}
	}
	for _, path := range changed {
		if err := config.WriteWorkspace(path, over[path]); err != nil {
			return nil, exitOperational(err)
		}
	}
	path, err := writeGroup(g, func(c *config.Config) error {
		if _, ok := c.Agents[handle]; !ok {
			return noProfile(handle)
		}
		delete(c.Agents, handle)
		if c.Agent.Profiles = without(c.Agent.Profiles, handle); len(c.Agent.Profiles) == 0 {
			c.Agent.Profiles = nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return append([]string{path}, changed...), nil
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

// agentRemoved reports a removed agent: the files changed and, when it was
// deleted, its profile directory.
func agentRemoved(g *globals, handle string, changed []string, deleted string) error {
	if g.json {
		return g.emitJSON(struct {
			Handle  string   `json:"handle"`
			Changed []string `json:"changed"`
			Deleted string   `json:"deleted,omitempty"`
		}{handle, changed, deleted})
	}
	fmt.Fprintf(g.app.Stdout, "removed %s\n", handle)
	for _, path := range changed {
		fmt.Fprintf(g.app.Stdout, "changed %s\n", path)
	}
	if deleted != "" {
		fmt.Fprintf(g.app.Stdout, "deleted %s\n", deleted)
	}
	return nil
}

func runAgentList(g *globals, keyQuery string) error {
	e, err := load(g, keyQuery)
	if err != nil {
		return err
	}
	handles := make([]string, 0, len(e.Group.Agents))
	for h := range e.Group.Agents {
		handles = append(handles, h)
	}
	sort.Strings(handles)
	views := make([]agentView, len(handles))
	for i, h := range handles {
		views[i] = agentViewOf(g, h, e.Group.Agents[h], contains(e.Workspace.Agent.Profiles, h))
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

func runAgentShow(g *globals, keyQuery, handle string) error {
	e, err := load(g, keyQuery)
	if err != nil {
		return err
	}
	p, ok := e.Group.Agents[handle]
	if !ok {
		return exitUsagef("%w", noProfile(handle))
	}
	v := agentViewOf(g, handle, p, contains(e.Workspace.Agent.Profiles, handle))
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "handle %s\nprovider %s\n", g.out().primary(v.Handle), v.Provider)
	if v.MaxTurns != 0 {
		fmt.Fprintf(g.app.Stdout, "max_turns %d\n", v.MaxTurns)
	}
	fmt.Fprintf(g.app.Stdout, "profile_dir %s\n", v.Dir)
	if v.Body != nil {
		body, err := json.Marshal(v.Body)
		if err != nil {
			return exitOperational(err)
		}
		fmt.Fprintf(g.app.Stdout, "body %s\n", body)
	}
	fmt.Fprintf(g.app.Stdout, "included %s\n", g.out().yesNo(v.Included))
	return nil
}

// handleArg is the Handle a verb's one argument names, refused unless valid.
func handleArg(args []string) (string, error) {
	if err := config.ValidateHandle(args[0]); err != nil {
		return "", exitUsagef("%w", err)
	}
	return args[0], nil
}

// noProfile is the refusal of a Handle the Group's config.toml does not
// define.
func noProfile(handle string) error {
	return fmt.Errorf("no Agent Profile %q in the Group's config.toml", handle)
}

// checkProvider refuses p unless the Group's config.toml has its Provider.
func checkProvider(c *config.Config, p config.Profile) error {
	if _, ok := c.Providers[p.Provider]; !ok {
		return fmt.Errorf("no Provider %s; add it with `defios provider add`", p.Provider)
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

// agentView is one agent, human or JSON: whether it is included is in the
// Group's [agent] profiles, or the Conversation's merged over it.
type agentView struct {
	Handle   string         `json:"handle"`
	Provider string         `json:"provider"`
	Model    string         `json:"model,omitempty"`
	MaxTurns int            `json:"max_turns,omitempty"`
	Dir      string         `json:"profile_dir"`
	Body     map[string]any `json:"body,omitempty"`
	Included bool           `json:"included"`
}

func agentViewOf(g *globals, handle string, p config.Profile, included bool) agentView {
	model, _ := p.Body["model"].(string)
	return agentView{Handle: handle, Provider: p.Provider, Model: model, MaxTurns: p.MaxTurns,
		Dir: g.paths().AgentProfileDir(handle), Body: p.Body, Included: included}
}
