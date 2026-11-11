package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"

	"defios.com/internal/config"
	"defios.com/internal/key"
	"defios.com/internal/network"
)

// Agent Profile failures a front end tells apart, besides InvalidError (a
// Handle, Provider alias or max turns refused), config.ErrNotWritten (a
// profile that would not read back) and config.ErrGrantRefused (a grant or
// revoke refused for what it names or gives).
var (
	// ErrUnknownProfile: the Node has no Agent Profile by that Handle.
	ErrUnknownProfile = errors.New("node: no such Agent Profile on this Node")
	// ErrProfileExists: the Node already has an Agent Profile by that
	// Handle.
	ErrProfileExists = errors.New("node: an Agent Profile by that Handle already exists")
	// ErrProfileUnchanged: an edit that would change nothing.
	ErrProfileUnchanged = errors.New("node: the Agent Profile is unchanged")
	// ErrFollowsGroup: an exclude from a Conversation that has no list of
	// its own, so follows the Group's.
	ErrFollowsGroup = errors.New("node: the Conversation follows the Group's list of agents")
)

// ProfileIncludedError: a delete refused because the Agent Profile is
// included where In says.
type ProfileIncludedError struct {
	Handle string
	In     []Place
}

func (e *ProfileIncludedError) Error() string {
	return fmt.Sprintf("Agent Profile %q is included in %d places", e.Handle, len(e.In))
}

// MCPGrant is an Agent Profile's grant of one MCP Server.
type MCPGrant = config.MCPGrant

// GrantKind is what a grant names: a built-in Tool, a Skill or an MCP
// Server.
type GrantKind = config.GrantKind

// The kinds of grant.
const (
	GrantTool  = config.GrantTool
	GrantSkill = config.GrantSkill
	GrantMCP   = config.GrantMCP
)

// AgentProfile is one Agent Profile on this Node (ADR 0029).
type AgentProfile struct {
	Handle   string
	Provider string // the alias of the Provider it runs on
	Model    string // body.model, "" if unset
	MaxTurns int    // 0 is unset: agent.DefaultMaxTurns
	Tools    []string
	Skills   []string            // <plugin>/<skill>
	MCP      map[string]MCPGrant // by <plugin>/<server>
	Body     map[string]any      // nil if unset
	Dir      string              // its directory, config.toml and SYSTEM.md
	// IncludedIn is every place on this Node that includes its Handle,
	// sorted by Group then Workspace.
	IncludedIn []Place
}

// Place is Group-level talk or one Conversation of a Group.
type Place struct {
	Group string
	// Conversation is the hex Conversation ID, GroupTalk for Group-level
	// talk; "" too when the Group has no replica to name it from.
	Conversation string
	// Workspace is the Conversation's Workspace directory name, "" for
	// Group-level talk.
	Workspace string
}

// ProfileEdit is what AddAgentProfile sets, or EditAgentProfile changes:
// a nil field is left as it is. Body replaces the body whole; Model sets
// body.model over it.
type ProfileEdit struct {
	Provider *string
	Model    *string
	Body     map[string]any
	MaxTurns *int // at least 1
}

// apply sets e on p.
func (e ProfileEdit) apply(p *config.Profile) error {
	if e.Provider != nil {
		p.Provider = *e.Provider
	}
	if e.MaxTurns != nil {
		if *e.MaxTurns < 1 {
			return invalidf("max turns %d must be a positive integer", *e.MaxTurns)
		}
		p.MaxTurns = *e.MaxTurns
	}
	if e.Body != nil {
		p.Body = e.Body
	}
	if e.Model != nil {
		body := map[string]any{"model": *e.Model}
		for k, v := range p.Body {
			if k != "model" {
				body[k] = v
			}
		}
		p.Body = body
	}
	return nil
}

// validHandle refuses an invalid Handle.
func validHandle(handle string) error {
	if err := config.ValidateHandle(handle); err != nil {
		return &InvalidError{Err: err}
	}
	return nil
}

// AgentProfiles lists the Agent Profiles on this Node, by Handle.
func (h *Host) AgentProfiles(ctx context.Context) ([]AgentProfile, error) {
	handles, err := h.profileHandles()
	if err != nil {
		return nil, err
	}
	in, err := h.includers()
	if err != nil {
		return nil, err
	}
	out := make([]AgentProfile, 0, len(handles))
	for _, handle := range handles {
		p, err := h.readProfile(handle)
		if err != nil {
			return nil, err
		}
		out = append(out, h.profileOf(handle, p, in[handle]))
	}
	return out, nil
}

// AgentProfile is the Agent Profile handle names.
func (h *Host) AgentProfile(ctx context.Context, handle string) (AgentProfile, error) {
	p, err := h.readProfile(handle)
	if err != nil {
		return AgentProfile{}, err
	}
	in, err := h.includers()
	if err != nil {
		return AgentProfile{}, err
	}
	return h.profileOf(handle, p, in[handle]), nil
}

// AddAgentProfile adds an Agent Profile running on e.Provider, which is
// required, and scaffolds its SYSTEM.md with a placeholder prompt unless
// one is there. It is included nowhere until IncludeAgent.
func (h *Host) AddAgentProfile(ctx context.Context, handle string, e ProfileEdit) (AgentProfile, error) {
	if err := validHandle(handle); err != nil {
		return AgentProfile{}, err
	}
	if e.Provider == nil {
		return AgentProfile{}, invalidf("a Provider is required: the alias of the Provider it runs on")
	}
	var p config.Profile
	if err := e.apply(&p); err != nil {
		return AgentProfile{}, err
	}
	path := h.p.AgentProfileFile(handle)
	if _, err := os.Stat(path); err == nil {
		return AgentProfile{}, fmt.Errorf("%w: %q", ErrProfileExists, handle)
	} else if !errors.Is(err, os.ErrNotExist) {
		return AgentProfile{}, err
	}
	if err := writeProfile(path, p); err != nil {
		return AgentProfile{}, err
	}
	if err := h.scaffoldSystem(handle); err != nil {
		return AgentProfile{}, err
	}
	return h.AgentProfile(ctx, handle)
}

// EditAgentProfile changes the fields of handle's Agent Profile e sets. A
// Handle is never renamed.
func (h *Host) EditAgentProfile(ctx context.Context, handle string, e ProfileEdit) (AgentProfile, error) {
	cur, err := h.readProfile(handle)
	if err != nil {
		return AgentProfile{}, err
	}
	p := cur
	if err := e.apply(&p); err != nil {
		return AgentProfile{}, err
	}
	if reflect.DeepEqual(p, cur) {
		return AgentProfile{}, fmt.Errorf("%w: %q", ErrProfileUnchanged, handle)
	}
	if err := writeProfile(h.p.AgentProfileFile(handle), p); err != nil {
		return AgentProfile{}, err
	}
	return h.AgentProfile(ctx, handle)
}

// DeleteAgentProfile deletes handle's Agent Profile directory. It refuses,
// with a *ProfileIncludedError and changing nothing, while any Group or
// Conversation on this Node includes it.
func (h *Host) DeleteAgentProfile(ctx context.Context, handle string) error {
	if err := validHandle(handle); err != nil {
		return err
	}
	dir := h.p.AgentProfileDir(handle)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %q", ErrUnknownProfile, handle)
	} else if err != nil {
		return err
	}
	in, err := h.includers()
	if err != nil {
		return err
	}
	if len(in[handle]) > 0 {
		return &ProfileIncludedError{Handle: handle, In: in[handle]}
	}
	return os.RemoveAll(dir)
}

// AgentSystem is handle's system prompt, its SYSTEM.md; "" if it has
// none.
func (h *Host) AgentSystem(ctx context.Context, handle string) (string, error) {
	if _, err := h.readProfile(handle); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(h.systemFile(handle))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(raw), err
}

// SetAgentSystem writes prompt as handle's SYSTEM.md. A summon reads it
// each time, so it takes effect at the next.
func (h *Host) SetAgentSystem(ctx context.Context, handle, prompt string) error {
	if _, err := h.readProfile(handle); err != nil {
		return err
	}
	return writePrivate(h.systemFile(handle), []byte(prompt))
}

// Grant grants handle a built-in Tool, or a Skill or MCP Server of a
// Plugin installed or built in as <plugin>/<name>, with secrets for an MCP
// Server (config.Profile.Grant), and returns what item is and the profile
// after. Granting what it has changes nothing.
func (h *Host) Grant(ctx context.Context, handle, item string, secrets []string) (GrantKind, AgentProfile, error) {
	return h.grant(ctx, handle, func(p config.Profile) (config.Profile, GrantKind, bool, error) {
		return p.Grant(h.p.PluginsDir(), item, secrets)
	})
}

// Revoke takes a Tool, Skill or MCP Server out of handle's grants
// (config.Profile.Revoke), and returns what item is and the profile after.
func (h *Host) Revoke(ctx context.Context, handle, item string) (GrantKind, AgentProfile, error) {
	return h.grant(ctx, handle, func(p config.Profile) (config.Profile, GrantKind, bool, error) {
		return p.Revoke(h.p.PluginsDir(), item)
	})
}

func (h *Host) grant(ctx context.Context, handle string, change func(config.Profile) (config.Profile, GrantKind, bool, error)) (GrantKind, AgentProfile, error) {
	p, err := h.readProfile(handle)
	if err != nil {
		return 0, AgentProfile{}, err
	}
	p, kind, changed, err := change(p)
	if err != nil {
		return kind, AgentProfile{}, err
	}
	if changed {
		if err := writeProfile(h.p.AgentProfileFile(handle), p); err != nil {
			return kind, AgentProfile{}, err
		}
	}
	a, err := h.AgentProfile(ctx, handle)
	return kind, a, err
}

// Inclusion is the list of Handles in force in one place after an include
// or exclude.
type Inclusion struct {
	Handles []string // never nil
	// OwnList: the Conversation followed the Group's list and has its own
	// now, so no longer follows the Group's.
	OwnList bool
}

// IncludeAgent adds handle, an Agent Profile on this Node, to the [agent]
// profiles of group's Group-level talk, or of Conversation conv. A
// Conversation with no list of its own follows the Group's; including one
// there gives it a list of that Handle alone. The Group need not be
// served.
func (h *Host) IncludeAgent(ctx context.Context, group, conv, handle string) (Inclusion, error) {
	if _, err := h.readProfile(handle); err != nil {
		return Inclusion{}, err
	}
	return h.editInclusion(group, conv, func(list []string, _ bool) ([]string, error) {
		if slices.Contains(list, handle) {
			return list, nil
		}
		return append(list, handle), nil
	})
}

// ExcludeAgent takes handle out of the [agent] profiles of group's
// Group-level talk, or of Conversation conv. Excluding the last leaves an
// empty list, so no agent answers there. A Conversation with no list of
// its own is ErrFollowsGroup.
func (h *Host) ExcludeAgent(ctx context.Context, group, conv, handle string) (Inclusion, error) {
	if err := validHandle(handle); err != nil {
		return Inclusion{}, err
	}
	return h.editInclusion(group, conv, func(list []string, following bool) ([]string, error) {
		if following {
			return nil, ErrFollowsGroup
		}
		out := []string{}
		for _, x := range list {
			if x != handle {
				out = append(out, x)
			}
		}
		return out, nil
	})
}

// IncludedAgents is the list of Handles in force in group's Group-level
// talk or Conversation conv: the Conversation's own, or the Group's it
// follows.
func (h *Host) IncludedAgents(ctx context.Context, group, conv string) ([]string, error) {
	e, err := h.loadConfig(group, conv)
	if err != nil {
		return nil, err
	}
	if e.Workspace.Agent.Profiles == nil {
		return []string{}, nil
	}
	return e.Workspace.Agent.Profiles, nil
}

// editInclusion applies edit to the [agent] profiles of group's
// Group-level talk or Conversation conv and writes it back, keeping the
// file's other sections and never merging the Conversation's over the
// Group's. edit is told whether a Conversation follows the Group's list.
func (h *Host) editInclusion(group, conv string, edit func(list []string, following bool) ([]string, error)) (Inclusion, error) {
	if conv == GroupTalk {
		var list []string
		err := h.writeGroupConfig(group, func(c *config.Config) error {
			l, err := edit(c.Agent.Profiles, false)
			if err != nil {
				return err
			}
			c.Agent.Profiles, list = l, l
			return nil
		})
		if err != nil {
			return Inclusion{}, err
		}
		if list == nil {
			list = []string{}
		}
		return Inclusion{Handles: list}, nil
	}
	path, err := h.conversationConfigFile(group, conv)
	if err != nil {
		return Inclusion{}, err
	}
	w, err := config.ReadWorkspace(path)
	if err != nil {
		return Inclusion{}, err
	}
	following := w.Agent.Profiles == nil
	l, err := edit(w.Agent.Profiles, following)
	if err != nil {
		return Inclusion{}, err
	}
	w.Agent.Profiles = l
	if err := mkdirPrivate(filepath.Dir(path)); err != nil {
		return Inclusion{}, err
	}
	if err := config.WriteWorkspace(path, w); err != nil {
		return Inclusion{}, err
	}
	return Inclusion{Handles: l, OwnList: following}, nil
}

// profileHandles is the Handle of every Agent Profile on this Node,
// sorted: each directory under agents/ named by a valid Handle and holding
// a config.toml.
func (h *Host) profileHandles() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(h.p.AgentProfilesDir(), "*", "config.toml"))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if handle := filepath.Base(filepath.Dir(m)); config.ValidateHandle(handle) == nil {
			out = append(out, handle)
		}
	}
	sort.Strings(out)
	return out, nil
}

// readProfile is handle's Agent Profile config.toml.
func (h *Host) readProfile(handle string) (config.Profile, error) {
	if err := validHandle(handle); err != nil {
		return config.Profile{}, err
	}
	p, err := config.ReadProfile(h.p.AgentProfileFile(handle))
	if errors.Is(err, os.ErrNotExist) {
		return config.Profile{}, fmt.Errorf("%w: %q", ErrUnknownProfile, handle)
	}
	return p, err
}

func (h *Host) profileOf(handle string, p config.Profile, in []Place) AgentProfile {
	model, _ := p.Body["model"].(string)
	skills := p.Skills
	if skills == nil {
		skills = []string{}
	}
	mcp := map[string]MCPGrant{}
	for ref, g := range p.MCP {
		if g.Secrets == nil {
			g.Secrets = []string{}
		}
		mcp[ref] = g
	}
	if in == nil {
		in = []Place{}
	}
	return AgentProfile{Handle: handle, Provider: p.Provider, Model: model, MaxTurns: p.MaxTurns, Tools: p.Granted(),
		Skills: skills, MCP: mcp, Body: p.Body, Dir: h.p.AgentProfileDir(handle), IncludedIn: in}
}

// writeProfile writes p as the Agent Profile config.toml at path, making
// its directory and removing it again, if it made it, when the write is
// refused.
func writeProfile(path string, p config.Profile) error {
	dir := filepath.Dir(path)
	_, statErr := os.Stat(dir)
	if err := mkdirPrivate(dir); err != nil {
		return err
	}
	err := config.WriteProfile(path, p)
	if err != nil && errors.Is(statErr, os.ErrNotExist) {
		os.Remove(dir)
	}
	return err
}

func (h *Host) systemFile(handle string) string {
	return filepath.Join(h.p.AgentProfileDir(handle), "SYSTEM.md")
}

// scaffoldSystem writes handle's SYSTEM.md with a placeholder prompt unless
// one is there.
func (h *Host) scaffoldSystem(handle string) error {
	f, err := os.OpenFile(h.systemFile(handle), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "# @%s\n\nYou are @%s, an agent in a DeFiOS Group's talk.\nDescribe your purpose here.\n", handle, handle); err != nil {
		return err
	}
	return f.Close()
}

// writePrivate writes data to path, mode 0600, through a temporary file
// renamed over it.
func writePrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// includers is, by Handle, every place on this Node that includes it:
// each Group whose [agent] profiles lists it, and each Conversation whose
// own list does.
func (h *Host) includers() (map[string][]Place, error) {
	aliases, err := h.aliases()
	if err != nil {
		return nil, err
	}
	out := map[string][]Place{}
	for _, a := range aliases {
		c, err := config.ReadFile(h.p.GroupConfigFile(a))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, handle := range c.Agent.Profiles {
			out[handle] = append(out[handle], Place{Group: a, Conversation: GroupTalk})
		}
		files, err := config.ConversationFiles(h.p, a)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			continue
		}
		ids := h.workspaceIDs(a)
		for _, path := range files {
			w, err := config.ReadWorkspace(path)
			if err != nil {
				return nil, err
			}
			name := filepath.Base(filepath.Dir(path))
			for _, handle := range w.Agent.Profiles {
				out[handle] = append(out[handle], Place{Group: a, Conversation: ids[name], Workspace: name})
			}
		}
	}
	for _, in := range out {
		sort.Slice(in, func(i, j int) bool {
			if in[i].Group != in[j].Group {
				return in[i].Group < in[j].Group
			}
			return in[i].Workspace < in[j].Workspace
		})
	}
	return out, nil
}

// workspaceIDs is each of group's Conversations' hex ID by its Workspace
// name; empty when the Group has no replica to read its Keys from.
func (h *Host) workspaceIDs(group string) map[string]string {
	keys, err := h.readKeys(group)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for id, name := range key.WorkspaceNames(keys) {
		out[name] = hex.EncodeToString(id[:])
	}
	return out
}

// workspace is the Keys of group and the Workspace name of its
// Conversation conv. A Conversation is its Key's, so one no Key has is
// key.ErrUnknown.
func (h *Host) workspace(group, conv string) (name string, keys []key.Key, err error) {
	id, err := conversationID(conv)
	if err != nil {
		return "", nil, err
	}
	if keys, err = h.readKeys(group); err != nil {
		return "", nil, err
	}
	name, ok := key.WorkspaceNames(keys)[[32]byte(id)]
	if !ok {
		return "", nil, fmt.Errorf("%w: no Key's Conversation is %s", key.ErrUnknown, conv)
	}
	return name, keys, nil
}

// conversationConfigFile is the path of the config.toml of group's
// Conversation conv, there or not.
func (h *Host) conversationConfigFile(group, conv string) (string, error) {
	name, _, err := h.workspace(group, conv)
	if err != nil {
		return "", err
	}
	return h.p.ConversationConfigFile(group, name), nil
}

// loadConfig is the config in force in group's Group-level talk or
// Conversation conv. A Group with no config.toml is empty.
func (h *Host) loadConfig(group, conv string) (config.Effective, error) {
	if err := h.knownGroup(group); err != nil {
		return config.Effective{}, err
	}
	if conv == GroupTalk {
		return config.Load(h.p, group, nil, nil)
	}
	_, keys, err := h.workspace(group, conv)
	if err != nil {
		return config.Effective{}, err
	}
	id, _ := conversationID(conv)
	return config.Load(h.p, group, id, keys)
}

// writeGroupConfig applies change to group's config.toml, an absent one
// being empty, and writes it back. change sees non-nil Network and
// Provider maps; a change refused writes nothing.
func (h *Host) writeGroupConfig(group string, change func(*config.Config) error) error {
	if err := h.knownGroup(group); err != nil {
		return err
	}
	path := h.p.GroupConfigFile(group)
	cfg, err := config.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if cfg.Networks == nil {
		cfg.Networks = map[string]network.Network{}
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.Provider{}
	}
	if err := change(&cfg); err != nil {
		return err
	}
	if len(cfg.Networks) == 0 {
		cfg.Networks = nil
	}
	if len(cfg.Providers) == 0 {
		cfg.Providers = nil
	}
	if err := mkdirPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	return config.WriteFile(path, cfg)
}
