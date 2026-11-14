package shell

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"defios.com/internal/node"
	"defios.com/internal/plugin"
)

// The Plugins page: this Node's Plugins (ADR 0029, 0030), the Skills and
// MCP Servers each brings, and installing, updating and removing them.
// An install or update is staged first, for the page to preview what it
// brings, and only Commit puts it in place. Every change goes through
// internal/node.

// PluginsNode is what the Plugins page changes; it lists them with
// AgentsNode's Plugins.
type PluginsNode interface {
	InstalledPlugins(ctx context.Context) ([]string, error)
	StagePluginInstall(ctx context.Context, source string) (*node.PluginStage, error)
	StagePluginUpdate(ctx context.Context, name string) (*node.PluginStage, error)
	RemovePlugin(ctx context.Context, name string) error
}

// FieldSource is the Plugins page's source field, which a refused install
// names.
const FieldSource = "source"

// PluginsView is the Plugins page.
type PluginsView struct {
	Plugins     []PluginView
	ListProblem *Problem
	// Stage is the install or update staged, to preview before Commit;
	// nil for none.
	Stage *StageView

	// Problem is why the last change was refused, Where what it is about
	// ("install", "update:<name>", "update-all", "commit" or
	// "remove:<name>") and Field the field ("" or FieldSource).
	Problem *Problem
	Where   string
	Field   string
	// Grants are the agents' grants that refused a remove.
	Grants []node.PluginGrant
	// Done says what the last change did, as "Removed audit.".
	Done string
}

// PluginView is one Plugin and what it brings. Source and Pin are where
// it was installed from and what it is pinned to, "" for a built-in; a
// Missing one is pinned with nothing installed, as a restore leaves one
// it could not fetch.
type PluginView struct {
	Name, Version, Description string
	Source, Pin                string
	BuiltIn, Missing           bool
	Skills                     []node.PluginSkill
	Servers                    []node.PluginServer
}

// StageView is an install or update staged: each Plugin fetched, with
// what it brings. ID names it to CommitPlugins.
type StageView struct {
	ID      uint64
	Install bool
	Plugins []StagedPlugin
}

// StagedPlugin is one Plugin staged. Was is the pin an update replaces;
// UpToDate an update with nothing new. Diff is a git update's change to
// plugin.toml, or the whole new one when DiffWhole. Problem is why it
// could not be staged, in an update of them all, which leaves the rest
// to commit.
type StagedPlugin struct {
	Name      string
	Plugin    PluginView
	Pin, Was  node.PluginPin
	UpToDate  bool
	Diff      string
	DiffWhole bool
	Problem   *Problem
}

// pluginStages holds the stage the page previews, until it is committed
// or dropped. Only one is held: a new one drops the one before.
type pluginStages struct {
	mu     sync.Mutex
	seq    uint64
	id     uint64
	stages []*node.PluginStage
	view   *StageView
}

// hold makes v, of stages, the stage held, dropping the one before.
func (p *pluginStages) hold(stages []*node.PluginStage, v *StageView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked()
	p.seq++
	p.id, p.stages, p.view = p.seq, stages, v
	v.ID = p.id
}

// take hands over the stage held if it is id's, holding none after.
func (p *pluginStages) take(id uint64) ([]*node.PluginStage, *StageView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.view == nil || p.id != id {
		return nil, nil, false
	}
	stages, v := p.stages, p.view
	p.stages, p.view = nil, nil
	return stages, v, true
}

func (p *pluginStages) shown() *StageView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.view
}

// drop drops the stage held, if any.
func (p *pluginStages) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked()
}

func (p *pluginStages) closeLocked() {
	for _, s := range p.stages {
		s.Close()
	}
	p.stages, p.view = nil, nil
}

// Plugins is the Plugins page, with the stage held, if any.
func (s *Shell) Plugins() PluginsView {
	var v PluginsView
	if n := s.node(); n != nil {
		s.fillPlugins(n, &v)
	}
	return v
}

// StagePluginInstall fetches a Plugin from source, a git URL (url@ref for
// a branch, tag or commit) or a local path, for the page to preview. It
// holds no lock while fetching, which may take long.
func (s *Shell) StagePluginInstall(source string) PluginsView {
	v := PluginsView{Where: "install"}
	n := s.node()
	if n == nil {
		return v
	}
	source = strings.TrimSpace(source)
	if source == "" {
		v.Problem, v.Field = &Problem{"invalid", "Give a git URL, url@ref or a path on this device."}, FieldSource
	} else if st, err := n.StagePluginInstall(s.ctx, source); err != nil {
		v.Problem = stageProblem(err)
		if errors.As(err, new(*node.InvalidError)) {
			v.Field = FieldSource
		}
	} else {
		s.plugins.hold([]*node.PluginStage{st}, &StageView{Install: true, Plugins: []StagedPlugin{stagedOf(st)}})
	}
	s.fillPlugins(n, &v)
	return v
}

// StagePluginUpdate fetches the installed Plugin name again from its
// source, or every installed one for "", for the page to preview. In an
// update of them all, one that can't be fetched is shown with its
// Problem and the rest are staged.
func (s *Shell) StagePluginUpdate(name string) PluginsView {
	v := PluginsView{Where: "update:" + name}
	n := s.node()
	if n == nil {
		return v
	}
	if name != "" {
		if st, err := n.StagePluginUpdate(s.ctx, name); err != nil {
			v.Problem = stageProblem(err)
		} else {
			s.plugins.hold([]*node.PluginStage{st}, &StageView{Plugins: []StagedPlugin{stagedOf(st)}})
		}
		s.fillPlugins(n, &v)
		return v
	}
	v.Where = "update-all"
	names, err := n.InstalledPlugins(s.ctx)
	if err != nil {
		v.Problem = problemOf(err)
		s.fillPlugins(n, &v)
		return v
	}
	var stages []*node.PluginStage
	sv := &StageView{Plugins: []StagedPlugin{}}
	for _, name := range names {
		st, err := n.StagePluginUpdate(s.ctx, name)
		if err != nil {
			sv.Plugins = append(sv.Plugins, StagedPlugin{Name: name, Problem: stageProblem(err)})
			continue
		}
		stages = append(stages, st)
		sv.Plugins = append(sv.Plugins, stagedOf(st))
	}
	s.plugins.hold(stages, sv)
	s.fillPlugins(n, &v)
	return v
}

// CommitPlugins puts the stage id in place: each Plugin staged that is not
// up to date. A stage no longer held, dropped or replaced, commits nothing.
func (s *Shell) CommitPlugins(id uint64) PluginsView {
	s.op.Lock()
	defer s.op.Unlock()
	v := PluginsView{Where: "commit"}
	n := s.node()
	if n == nil {
		return v
	}
	stages, sv, ok := s.plugins.take(id)
	if !ok {
		v.Problem = &Problem{"stage-gone", "That preview is no longer current, so nothing was installed. Start again."}
		s.fillPlugins(n, &v)
		return v
	}
	var did []string
	for _, st := range stages {
		if !st.UpToDate {
			if err := st.Commit(); err != nil && v.Problem == nil {
				v.Problem = problemOf(err)
			} else if err == nil {
				did = append(did, committed(st, sv.Install))
			}
		}
		st.Close()
	}
	switch {
	case len(did) > 0:
		v.Done = strings.Join(did, " ")
	case v.Problem == nil:
		v.Done = "Everything is up to date."
	}
	s.fillPlugins(n, &v)
	return v
}

// CancelPlugins drops the stage held, installing nothing.
func (s *Shell) CancelPlugins() PluginsView {
	s.plugins.drop()
	return s.Plugins()
}

// RemovePlugin removes the installed Plugin name: refused, naming them,
// while agents grant items of it.
func (s *Shell) RemovePlugin(name string) PluginsView {
	s.op.Lock()
	defer s.op.Unlock()
	v := PluginsView{Where: "remove:" + name}
	n := s.node()
	if n == nil {
		return v
	}
	if err := n.RemovePlugin(s.ctx, name); err != nil {
		v.Problem = problemOf(err)
		var granted *node.PluginGrantedError
		if errors.As(err, &granted) {
			v.Grants = granted.Grants
		}
	} else {
		v.Done = fmt.Sprintf("Removed %s.", name)
	}
	s.fillPlugins(n, &v)
	return v
}

// fillPlugins adds the Plugins and the stage held.
func (s *Shell) fillPlugins(n Node, v *PluginsView) {
	v.Stage = s.plugins.shown()
	list, err := n.Plugins(s.ctx)
	if err != nil {
		v.ListProblem = problemOf(err)
		return
	}
	v.Plugins = make([]PluginView, len(list))
	for i, p := range list {
		v.Plugins[i] = pluginView(p)
	}
}

func pluginView(p node.Plugin) PluginView {
	return PluginView{Name: p.Name, Version: p.Version, Description: p.Description, Source: p.Source, Pin: p.Pin,
		BuiltIn: p.BuiltIn, Missing: p.Missing, Skills: orEmpty(p.Skills), Servers: orEmpty(p.Servers)}
}

func stagedOf(st *node.PluginStage) StagedPlugin {
	return StagedPlugin{Name: st.Plugin.Name, Plugin: pluginView(st.Plugin), Pin: st.Entry, Was: st.Was,
		UpToDate: st.UpToDate, Diff: st.Diff, DiffWhole: st.DiffWhole}
}

// committed says what committing st did.
func committed(st *node.PluginStage, install bool) string {
	if install {
		return fmt.Sprintf("Installed %s %s.", st.Plugin.Name, st.Plugin.Version)
	}
	return fmt.Sprintf("Updated %s to %s.", st.Plugin.Name, st.Plugin.Version)
}

// stageProblem is problemOf for fetching a Plugin: what is not one of
// internal/node's typed errors is a source that could not be fetched or
// read, whose detail is shown as the library words it.
func stageProblem(err error) *Problem {
	if p := problemOf(err); p.Code != "error" {
		return p
	}
	return &Problem{"not-fetched", "No Plugin could be fetched and read from there (" + err.Error() + ")."}
}

// pluginsProblem maps the Plugins page's errors; nil for another.
func pluginsProblem(err error) *Problem {
	var granted *node.PluginGrantedError
	switch {
	case errors.As(err, &granted):
		by := make([]string, len(granted.Grants))
		for i, g := range granted.Grants {
			by[i] = fmt.Sprintf("@%s (%s)", g.Handle, strings.Join(g.Items, ", "))
		}
		verb := "grant"
		if len(by) == 1 {
			verb = "grants"
		}
		return &Problem{"plugin-granted", fmt.Sprintf("%s can't be removed while %s %s items of it. Revoke them on the Agents page first.",
			granted.Plugin, strings.Join(by, " and "), verb)}
	case errors.Is(err, plugin.ErrBuiltIn):
		return &Problem{"built-in", "That is a built-in Plugin: it comes with DeFiOS, so it can't be installed over, updated or removed."}
	case errors.Is(err, plugin.ErrInstalled):
		return &Problem{"installed", "A Plugin by that name is installed already. Update it instead, or remove it first."}
	case errors.Is(err, plugin.ErrNotInstalled):
		return &Problem{"not-installed", "No Plugin by that name is installed on this device."}
	}
	return nil
}
