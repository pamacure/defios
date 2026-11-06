package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"defios.com/internal/plugin"
	"defios.com/internal/secret"
)

// GrantKind is what a profile grants: a built-in Tool, or a Plugin's Skill
// or MCP Server (ADR 0029, ADR 0030).
type GrantKind int

const (
	GrantTool  GrantKind = iota // in tools
	GrantSkill                  // in skills, <plugin>/<skill>
	GrantMCP                    // in mcp, <plugin>/<server>
)

// ErrGrantRefused is a grant or revoke refused for what it names or gives.
// Any other error from Grant or Revoke is one reading this Node's Plugins.
var ErrGrantRefused = errors.New("config: grant refused")

// refusal is a grant or revoke refused, saying why.
type refusal struct{ msg string }

func (r refusal) Error() string        { return r.msg }
func (r refusal) Is(target error) bool { return target == ErrGrantRefused }

func refused(format string, a ...any) error { return refusal{fmt.Sprintf(format, a...)} }

// Grant is p granting item, a Tool, or a Skill or MCP Server of a Plugin
// installed under pluginsDir or built in, as <plugin>/<name>; secrets are
// the Secrets an MCP Server's process gets, each one it asks for, and are
// for nothing else. Granting a server again sets its Secrets to secrets.
// It returns what item is and whether p changed: granting what p has
// changes nothing. An unset tools is say alone, so a Tool granted into it
// joins say. A name that is both a Skill and an MCP Server is ambiguous.
func (p Profile) Grant(pluginsDir, item string, secrets []string) (Profile, GrantKind, bool, error) {
	if !strings.Contains(item, "/") {
		if secrets != nil {
			return p, GrantTool, false, refused("Secrets are only for an MCP Server, not the Tool %q", item)
		}
		if err := ValidateTool(item); err != nil {
			return p, GrantTool, false, refusal{err.Error()}
		}
		tools := p.Granted()
		if slices.Contains(tools, item) {
			return p, GrantTool, false, nil
		}
		p.Tools = append(slices.Clone(tools), item)
		return p, GrantTool, true, nil
	}
	kind, srv, err := pluginItem(pluginsDir, item)
	if err != nil {
		return p, kind, false, err
	}
	if kind == GrantSkill {
		if secrets != nil {
			return p, kind, false, refused("Secrets are only for an MCP Server, and %q is a Skill", item)
		}
		if slices.Contains(p.Skills, item) {
			return p, kind, false, nil
		}
		p.Skills = append(slices.Clone(p.Skills), item)
		return p, kind, true, nil
	}
	for i, name := range secrets {
		if err := secret.ValidateName(name); err != nil {
			return p, kind, false, refusal{err.Error()}
		}
		if slices.Contains(secrets[:i], name) {
			return p, kind, false, refused("Secret %s is given twice", name)
		}
	}
	if err := srv.CheckSecrets(secrets); err != nil {
		return p, kind, false, refused("MCP Server %s: %v", item, err)
	}
	if old, ok := p.MCP[item]; ok && slices.Equal(old.Secrets, secrets) {
		return p, kind, false, nil
	}
	p.MCP = maps.Clone(p.MCP)
	if p.MCP == nil {
		p.MCP = map[string]MCPGrant{}
	}
	p.MCP[item] = MCPGrant{Secrets: slices.Clone(secrets)}
	return p, kind, true, nil
}

// Revoke is p without item, a Tool, or a Skill or MCP Server as
// <plugin>/<name>, and whether p changed: revoking what p lacks changes
// nothing. One p grants needs no Plugin, so one whose Plugin is gone can
// be revoked; one it does not is looked up under pluginsDir only to tell
// its kind. Revoking every Tool leaves tools empty, not unset.
func (p Profile) Revoke(pluginsDir, item string) (Profile, GrantKind, bool, error) {
	if !strings.Contains(item, "/") {
		if err := ValidateTool(item); err != nil {
			return p, GrantTool, false, refusal{err.Error()}
		}
		tools := p.Granted()
		if !slices.Contains(tools, item) {
			return p, GrantTool, false, nil
		}
		p.Tools = slices.DeleteFunc(slices.Clone(tools), func(t string) bool { return t == item })
		return p, GrantTool, true, nil
	}
	_, hasMCP := p.MCP[item]
	hasSkill := slices.Contains(p.Skills, item)
	if !hasSkill && !hasMCP {
		kind, _, err := pluginItem(pluginsDir, item)
		return p, kind, false, err
	}
	if hasSkill {
		p.Skills = slices.DeleteFunc(slices.Clone(p.Skills), func(s string) bool { return s == item })
		if len(p.Skills) == 0 {
			p.Skills = nil
		}
	}
	if hasMCP {
		p.MCP = maps.Clone(p.MCP)
		delete(p.MCP, item)
		if len(p.MCP) == 0 {
			p.MCP = nil
		}
	}
	kind := GrantSkill
	if !hasSkill {
		kind = GrantMCP
	}
	return p, kind, true, nil
}

// PluginGrants are the Skills, then the MCP Servers, that p grants of
// Plugin name, each <plugin>/<name>, the servers sorted.
func (p Profile) PluginGrants(name string) []string {
	var out []string
	for _, ref := range append(slices.Clone(p.Skills), slices.Sorted(maps.Keys(p.MCP))...) {
		if pname, _, _ := strings.Cut(ref, "/"); pname == name {
			out = append(out, ref)
		}
	}
	return out
}

// pluginItem is what ref, <plugin>/<name>, names in a Plugin installed
// under pluginsDir or built in: a Skill, or an MCP Server and that server.
func pluginItem(pluginsDir, ref string) (GrantKind, plugin.Server, error) {
	pname, name, err := plugin.ParseRef(ref)
	if err != nil {
		return GrantSkill, plugin.Server{}, refusal{err.Error()}
	}
	p, err := plugin.Find(pluginsDir, pname)
	if errors.Is(err, plugin.ErrNotFound) {
		return GrantSkill, plugin.Server{}, refused("no Plugin %q is installed or built in", pname)
	} else if err != nil {
		return GrantSkill, plugin.Server{}, err
	}
	_, skill := p.Skill(name)
	srv, server := p.Servers[name]
	switch {
	case skill && server:
		return GrantSkill, plugin.Server{}, refused("%q is both a Skill and an MCP Server of Plugin %s, so which to grant is ambiguous", ref, pname)
	case server:
		return GrantMCP, srv, nil
	case !skill:
		return GrantSkill, plugin.Server{}, refused("%q: Plugin %s has no Skill or MCP Server %q", ref, pname, name)
	}
	return GrantSkill, plugin.Server{}, nil
}
