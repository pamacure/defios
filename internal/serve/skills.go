package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"defios.com/internal/agent"
	"defios.com/internal/config"
	"defios.com/internal/plugin"
)

const skillLoadDescription = "Read one of your Skills, named <plugin>/<skill> as your Skills list them: without file, its SKILL.md " +
	"instructions; with file, a file under that Skill's directory that its instructions refer to, by a path relative to it."

var skillLoadSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"name":{"type":"string","description":"The Skill, <plugin>/<skill>."},` +
	`"file":{"type":"string","description":"A path relative to the Skill's directory; unset reads its SKILL.md."}},` +
	`"required":["name"],"additionalProperties":false}`)

// grantedSkill is a Skill a profile grants, found in its Plugin.
type grantedSkill struct {
	Ref         string // <plugin>/<skill>
	Description string
	plugin      plugin.Plugin
	name        string
}

// grantedSkills finds each Skill refs grants among this Node's Plugins,
// sorted by Ref. One that is not installed fails the summon, naming handle
// and the entry.
func (s *server) grantedSkills(handle string, refs []string) ([]grantedSkill, error) {
	var out []grantedSkill
	for _, ref := range refs {
		pname, name, err := plugin.ParseSkillRef(ref)
		if err != nil {
			return nil, fmt.Errorf("@%s's profile: skills: %w", handle, err)
		}
		p, err := plugin.Find(s.opts.Paths.PluginsDir(), pname)
		if errors.Is(err, plugin.ErrNotFound) {
			return nil, fmt.Errorf("@%s's profile: skills: %q: no Plugin %q is installed", handle, ref, pname)
		} else if err != nil {
			return nil, fmt.Errorf("@%s's profile: skills: %q: %w", handle, ref, err)
		}
		sk, ok := p.Skill(name)
		if !ok {
			return nil, fmt.Errorf("@%s's profile: skills: %q: Plugin %s has no Skill %q", handle, ref, pname, name)
		}
		// One line each in the preamble, whatever the frontmatter's folding.
		desc := strings.Join(strings.Fields(sk.Description), " ")
		out = append(out, grantedSkill{Ref: ref, Description: desc, plugin: p, name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// skillLoadTool is skill_load over skills, the granted Skills alone.
func skillLoadTool(skills []grantedSkill) agent.Tool {
	call := func(_ context.Context, input json.RawMessage) (string, error) {
		var in struct {
			Name string  `json:"name"`
			File *string `json:"file"`
		}
		if err := json.Unmarshal(input, &in); err != nil || in.Name == "" {
			return "", errors.New(`its input must be {"name": "<plugin>/<skill>", "file": "<optional path in the Skill>"}`)
		}
		i := slices.IndexFunc(skills, func(g grantedSkill) bool { return g.Ref == in.Name })
		if i < 0 {
			refs := make([]string, len(skills))
			for j, g := range skills {
				refs[j] = g.Ref
			}
			return "", fmt.Errorf("Skill %q is not granted to you: your Skills are %s", in.Name, strings.Join(refs, ", "))
		}
		g := skills[i]
		if in.File == nil {
			return g.plugin.SkillBody(g.name)
		}
		return g.plugin.SkillFile(g.name, *in.File)
	}
	return agent.Tool{Name: config.ToolSkillLoad, Description: skillLoadDescription, Schema: skillLoadSchema, Call: call}
}
