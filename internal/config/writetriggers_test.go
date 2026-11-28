package config_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"defios.com/internal/config"
	"defios.com/internal/trigger"
	"defios.com/plugins"
)

// writeTriggersSkill is the built-in defios/write-triggers Skill's SKILL.md.
func writeTriggersSkill(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(plugins.FS, "defios/skills/write-triggers/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// backticked is every `code` span of md.
func backticked(md string) []string {
	var out []string
	for _, m := range regexp.MustCompile("`([^`\n]+)`").FindAllStringSubmatch(md, -1) {
		out = append(out, m[1])
	}
	return out
}

// The write-triggers Skill names only Tools that exist, only events a
// Hook may filter on, and every one of those events; and only DEFIOS_*
// variables an exec Job is given (ADR 0046).
func TestWriteTriggersNamesRealToolsEventsAndVariables(t *testing.T) {
	md := writeTriggersSkill(t)
	tools := append(slices.Clone(config.Tools), config.ToolSkillLoad)
	tool := regexp.MustCompile(`^(talk|sign|chain|key|job|hook|schedule|skill)_[a-z_]+$`)
	event := regexp.MustCompile(`^[a-z]+\.[a-z_]+$`)
	variable := regexp.MustCompile(`^DEFIOS_[A-Z0-9_]+$`)
	vars := trigger.Vars(trigger.Event{}, "")
	seen := map[string]bool{}
	for _, s := range backticked(md) {
		seen[s] = true
		switch {
		case tool.MatchString(s):
			if !slices.Contains(tools, s) {
				t.Errorf("`%s` is no Tool", s)
			}
		case event.MatchString(s) && s != "config.toml":
			if err := config.ValidateEvent(s); err != nil {
				t.Errorf("`%s`: %v", s, err)
			}
		case variable.MatchString(s):
			if _, ok := vars[s]; !ok {
				t.Errorf("`%s` is no variable an exec Job is given", s)
			}
		}
	}
	for _, ev := range config.Events() {
		if !seen[ev] {
			t.Errorf("event %s is not named", ev)
		}
	}
	for _, v := range slices.Sorted(maps.Keys(vars)) {
		if !seen[v] {
			t.Errorf("variable %s is not named", v)
		}
	}
}

// Each fields section of the write-triggers Skill lists exactly the
// config.toml keys of its entry, less by, which the Tools set (ADR 0032,
// ADR 0046).
func TestWriteTriggersListsEveryField(t *testing.T) {
	md := writeTriggersSkill(t)
	for heading, v := range map[string]any{"Job": config.Job{}, "Hook": config.Hook{}, "Schedule": config.Schedule{}} {
		var want []string
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			if k, _, _ := strings.Cut(rt.Field(i).Tag.Get("toml"), ","); k != "" && k != "by" {
				want = append(want, k)
			}
		}
		_, section, ok := strings.Cut(md, "### "+heading+" fields\n")
		if !ok {
			t.Errorf("no ### %s fields section", heading)
			continue
		}
		section, _, _ = strings.Cut(section, "\n#")
		var got []string
		for _, m := range regexp.MustCompile("(?m)^- `([a-z_]+)`:").FindAllStringSubmatch(section, -1) {
			got = append(got, m[1])
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s fields listed %v, want %v", heading, got, want)
		}
	}
}

// The write-triggers Skill's examples are Tool inputs of the form the add
// Tools take, and together a config.toml the loader accepts (ADR 0032).
func TestWriteTriggersExamplesLoad(t *testing.T) {
	md := writeTriggersSkill(t)
	tables := map[string]map[string]any{"job": {}, "hook": {}, "schedule": {}}
	var agents []string
	for _, m := range regexp.MustCompile("(?s)```json\n(.*?)\n```").FindAllStringSubmatch(md, -1) {
		var in map[string]json.RawMessage
		if err := json.Unmarshal([]byte(m[1]), &in); err != nil {
			t.Fatalf("example %s: %v", m[1], err)
		}
		var name string
		if err := json.Unmarshal(in["name"], &name); err != nil || len(in) != 2 {
			t.Fatalf("example %s is not {name, <entry>}", m[1])
		}
		found := false
		for field, entries := range tables {
			if raw, ok := in[field]; ok {
				var e map[string]any
				if err := json.Unmarshal(raw, &e); err != nil {
					t.Fatalf("example %s: %v", m[1], err)
				}
				if a, ok := e["agent"].(string); ok {
					agents = append(agents, a)
				}
				entries[name] = e
				found = true
			}
		}
		if !found {
			t.Fatalf("example %s gives no job, hook or schedule", m[1])
		}
	}
	for field, entries := range tables {
		if len(entries) == 0 {
			t.Errorf("no %s example", field)
		}
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(map[string]any{
		"agent": map[string]any{"profiles": agents},
		"jobs":  tables["job"], "hooks": tables["hook"], "schedules": tables["schedule"],
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := config.ReadFile(write(t, buf.String())); err != nil {
		t.Fatalf("the examples do not load: %v\n%s", err, buf.String())
	}
}
