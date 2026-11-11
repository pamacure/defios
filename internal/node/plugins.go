package node

import (
	"context"
	"fmt"
	"strings"

	"defios.com/internal/plugin"
)

// Plugin failures a front end tells apart, besides InvalidError (a source
// that is no git URL or path) and PluginGrantedError: plugin.ErrInstalled
// (an install of a name installed already), plugin.ErrNotInstalled (an
// update or remove of one that is not) and plugin.ErrBuiltIn (a change to
// a built-in Plugin, or an install under a built-in's name).

// PluginGrantedError: a remove refused because Agent Profiles grant items
// of the Plugin, as Grants says.
type PluginGrantedError struct {
	Plugin string
	Grants []PluginGrant
}

func (e *PluginGrantedError) Error() string {
	by := make([]string, len(e.Grants))
	for i, g := range e.Grants {
		by[i] = fmt.Sprintf("%s (%s)", g.Handle, strings.Join(g.Items, ", "))
	}
	return fmt.Sprintf("Plugin %s is granted by %s", e.Plugin, strings.Join(by, ", "))
}

// PluginGrant is one Agent Profile's grants of a Plugin's items.
type PluginGrant struct {
	Handle string
	Items  []string // <plugin>/<name>, its Skills then its MCP Servers
}

// Plugin is a Plugin as this Node has it (ADR 0029): built in, installed
// and pinned, or Missing, pinned with nothing installed, as a restore
// leaves one it could not fetch, when only its name and pin are known.
type Plugin struct {
	Name, Version, Description string
	// Source and Pin are where it was installed from and what it is
	// pinned to; "" for a built-in.
	Source, Pin string
	BuiltIn     bool
	Missing     bool
	Skills      []PluginSkill
	Servers     []PluginServer // by name
}

// PluginSkill is one Skill a Plugin brings.
type PluginSkill struct {
	Name, Description string
}

// PluginServer is one MCP Server a Plugin brings: the program it would
// run on this machine, and the variables it asks for.
type PluginServer struct {
	Name        string
	CommandLine string
	Env         []string
}

// PluginPin is a Plugin's entry in plugins.lock.
type PluginPin struct {
	Source, Pin, Version string
}

// PluginStage is a Plugin fetched and read, not yet installed, so a front
// end can show what it brings before Commit puts it in place. Close drops
// it, committed or not.
type PluginStage struct {
	Plugin Plugin
	// Entry is what Commit pins.
	Entry PluginPin
	// Was is the pin an update replaces; zero for an install.
	Was PluginPin
	// UpToDate is an update already at Entry's pin, with nothing to commit.
	UpToDate bool
	// Diff is a git update's change to plugin.toml. When Was's commit is
	// no longer in the source, DiffWhole is set and Diff is the new
	// manifest.
	Diff      string
	DiffWhole bool

	s *plugin.Staged
}

// Commit installs the staged Plugin, in place of any it updates.
func (s *PluginStage) Commit() error { return s.s.Commit() }

// Close drops whatever of the stage is left.
func (s *PluginStage) Close() { s.s.Close() }

// Plugins is every Plugin this Node has: the built-ins, then those
// installed, then each pinned and Missing. None needs serve.
func (h *Host) Plugins(ctx context.Context) ([]Plugin, error) {
	listed, err := plugin.NewStore(h.p).List()
	if err != nil {
		return nil, err
	}
	out := make([]Plugin, len(listed))
	for i, l := range listed {
		out[i] = pluginOf(l.Plugin, l.Entry)
		out[i].Missing = l.Missing
	}
	return out, nil
}

// InstalledPlugins is the name of every Plugin pinned in plugins.lock,
// sorted: what updating them all updates, Missing ones included.
func (h *Host) InstalledPlugins(ctx context.Context) ([]string, error) {
	return plugin.NewStore(h.p).Installed()
}

// StagePluginInstall fetches a Plugin from source, a git URL (url@ref for
// a branch, tag or commit) or a local path, for an install.
func (h *Host) StagePluginInstall(ctx context.Context, source string) (*PluginStage, error) {
	src, err := plugin.ParseSource(source)
	if err != nil {
		return nil, &InvalidError{Err: err}
	}
	s, err := plugin.NewStore(h.p).StageInstall(ctx, src)
	if err != nil {
		return nil, err
	}
	return stageOf(s), nil
}

// StagePluginUpdate fetches the installed Plugin name again from its
// source, at the ref it was installed at.
func (h *Host) StagePluginUpdate(ctx context.Context, name string) (*PluginStage, error) {
	s, err := plugin.NewStore(h.p).StageUpdate(ctx, name)
	if err != nil {
		return nil, err
	}
	return stageOf(s), nil
}

// RemovePlugin removes the installed Plugin name and its pin, refused with
// a PluginGrantedError while an Agent Profile grants an item of it.
func (h *Host) RemovePlugin(ctx context.Context, name string) error {
	// A built-in is refused by Remove itself, granted or not.
	if _, builtIn, _ := plugin.BuiltIn(name); !builtIn {
		grants, err := h.pluginGrants(name)
		if err != nil {
			return err
		}
		if len(grants) > 0 {
			return &PluginGrantedError{Plugin: name, Grants: grants}
		}
	}
	return plugin.NewStore(h.p).Remove(name)
}

// pluginGrants is every grant on this Node of an item of Plugin name, by
// Handle.
func (h *Host) pluginGrants(name string) ([]PluginGrant, error) {
	handles, err := h.profileHandles()
	if err != nil {
		return nil, err
	}
	var out []PluginGrant
	for _, handle := range handles {
		p, err := h.readProfile(handle)
		if err != nil {
			return nil, err
		}
		if items := p.PluginGrants(name); len(items) > 0 {
			out = append(out, PluginGrant{Handle: handle, Items: items})
		}
	}
	return out, nil
}

func stageOf(s *plugin.Staged) *PluginStage {
	return &PluginStage{Plugin: pluginOf(s.Plugin, s.Entry), Entry: pinOf(s.Entry), Was: pinOf(s.Was),
		UpToDate: s.UpToDate, Diff: s.Diff, DiffWhole: s.DiffWhole, s: s}
}

func pinOf(e plugin.LockEntry) PluginPin {
	return PluginPin{Source: e.Source, Pin: e.Pin, Version: e.Version}
}

func pluginOf(p plugin.Plugin, e plugin.LockEntry) Plugin {
	out := Plugin{Name: p.Name, Version: p.Version, Description: p.Description, BuiltIn: p.BuiltIn,
		Skills: []PluginSkill{}, Servers: []PluginServer{}}
	if !p.BuiltIn {
		out.Source, out.Pin = e.Source, e.Pin
	}
	for _, s := range p.Skills {
		out.Skills = append(out.Skills, PluginSkill{Name: s.Name, Description: s.Description})
	}
	for _, name := range p.ServerNames() {
		s := p.Servers[name]
		out.Servers = append(out.Servers, PluginServer{Name: name, CommandLine: s.CommandLine(), Env: s.Env})
	}
	return out
}
