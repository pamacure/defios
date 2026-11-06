package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"defios.com/internal/agent"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/nodedir"
)

// Effective is the settings in force in one place, Group-level talk or one
// Conversation (GLOSSARY, Workspace): the Group's config.toml with the
// Conversation's Workspace merged over it, read and merged once.
type Effective struct {
	// Group is the Group's config.toml as read, empty if there is none.
	Group Config
	// Workspace is the Group's Workspace with the Conversation's merged
	// over it.
	Workspace Workspace

	paths     nodedir.Paths
	groupFile string
	convFile  string
	conv      Workspace // the Conversation's file as read
}

// Load reads the settings in force in Group alias, for the Conversation
// whose ID is conv, or Group-level talk when conv is nil. A Group with no
// config.toml is empty; the Conversation's config.toml is found by its Key's
// Workspace name in keys, and a Conversation with none, or with no Key in
// keys, follows the Group's. It fails only when a file does not read or
// parse, or the merge is refused, naming the files. Whether [agent]
// profiles names defined profiles is for Included to find out.
func Load(p nodedir.Paths, alias string, conv []byte, keys []key.Key) (Effective, error) {
	e := Effective{paths: p, groupFile: p.GroupConfigFile(alias)}
	var err error
	e.Group, err = ReadFile(e.groupFile)
	if errors.Is(err, os.ErrNotExist) {
		e.Group = Config{}
	} else if err != nil {
		return Effective{}, err
	}
	e.Workspace = e.Group.Workspace
	var id [32]byte
	if len(conv) != len(id) {
		return e, nil
	}
	copy(id[:], conv)
	name, ok := key.WorkspaceNames(keys)[id]
	if !ok {
		return e, nil
	}
	e.convFile = p.ConversationConfigFile(alias, name)
	if e.conv, err = ReadWorkspace(e.convFile); err != nil {
		return Effective{}, err
	}
	if e.Workspace, err = e.Group.Workspace.Merge(e.conv); err != nil {
		return Effective{}, fmt.Errorf("config: %s over %s: %w", e.convFile, e.groupFile, err)
	}
	return e, nil
}

// ConversationFiles is every Conversation Workspace config.toml in Group
// alias, found on disk.
func ConversationFiles(p nodedir.Paths, alias string) ([]string, error) {
	return filepath.Glob(filepath.Join(p.GroupConfigDir(alias), "*", "config.toml"))
}

// GroupFile is the path of the Group's config.toml, read or absent.
func (e Effective) GroupFile() string { return e.groupFile }

// ConversationFile is the path of the Conversation's config.toml, present
// or not; empty for Group-level talk or a Conversation with no Key.
func (e Effective) ConversationFile() string { return e.convFile }

// Networks are the Networks in force here: the chains this Node signs for
// (ADR 0023).
func (e Effective) Networks() ([]network.Network, error) {
	return network.List(e.Workspace.Networks)
}

// Included is what may answer here: the profiles [agent] profiles lists, in
// its order, each read from its config.toml, and the depth a chain of them
// may reach. Whether a profile's SYSTEM.md exists is for whoever summons it
// to find out. A listed Handle with no profile config.toml is an error
// saying which file to fix.
func (e Effective) Included() (Included, error) {
	a := e.Workspace.Agent
	out := Included{MaxDepth: a.MaxDepth}
	if out.MaxDepth == 0 {
		out.MaxDepth = 1
	}
	for _, h := range a.Profiles {
		file := e.paths.AgentProfileFile(h)
		p, err := ReadProfile(file)
		if errors.Is(err, os.ErrNotExist) {
			err := fmt.Errorf("config: [agent] profiles: no Agent Profile %q", h)
			if e.conv.Agent.Profiles != nil {
				return Included{}, fmt.Errorf("%w: fix [agent] profiles in %s, or add %s", err, e.convFile, file)
			}
			return Included{}, fmt.Errorf("%w: fix [agent] profiles in %s, or add %s", err, e.groupFile, file)
		} else if err != nil {
			return Included{}, err
		}
		r := ResolvedProfile{Handle: h, Dir: e.paths.AgentProfileDir(h), Provider: p.Provider,
			MaxTurns: p.MaxTurns, Tools: p.Granted(), Skills: p.Skills, Body: p.Body}
		if base := e.Workspace.Providers[p.Provider].Body; base != nil {
			r.Body = agent.MergeBody(base, p.Body)
		}
		if r.MaxTurns == 0 {
			r.MaxTurns = agent.DefaultMaxTurns
		}
		out.Profiles = append(out.Profiles, r)
	}
	return out, nil
}
