package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"defios.com/internal/agent"
	"defios.com/internal/config"
	"defios.com/internal/key"
)

// triggerKind is one of the three kinds of Trigger config a Tool writes:
// its noun, the input field holding an entry, and its config.toml table.
type triggerKind struct {
	noun, field, table string
}

var (
	kindJob      = triggerKind{"Job", "job", config.TableJobs}
	kindHook     = triggerKind{"Hook", "hook", config.TableHooks}
	kindSchedule = triggerKind{"Schedule", "schedule", config.TableSchedules}
)

// triggerToolKinds are each Trigger-config Tool's kind and verb.
var triggerToolKinds = map[string]struct {
	kind triggerKind
	verb string
}{
	config.ToolJobList: {kindJob, "list"}, config.ToolJobAdd: {kindJob, "add"}, config.ToolJobEdit: {kindJob, "edit"}, config.ToolJobDelete: {kindJob, "delete"},
	config.ToolHookList: {kindHook, "list"}, config.ToolHookAdd: {kindHook, "add"}, config.ToolHookEdit: {kindHook, "edit"}, config.ToolHookDelete: {kindHook, "delete"},
	config.ToolScheduleList: {kindSchedule, "list"}, config.ToolScheduleAdd: {kindSchedule, "add"}, config.ToolScheduleEdit: {kindSchedule, "edit"}, config.ToolScheduleDelete: {kindSchedule, "delete"},
}

// What each kind's entry holds, for the Tools' descriptions.
var triggerFields = map[triggerKind]string{
	kindJob: "a Job has exactly one of agent (a Handle of your Node's, with prompt, and target: \"group\", a Key's alias or Workspace name, or unset for the event's Conversation), " +
		"exec (an argument list run with no shell, with optional secrets) or webhook (an http(s) URL or Secret reference, with optional headers), and an optional timeout such as \"30s\" for exec and webhook",
	kindHook: "a Hook has jobs (the Jobs it runs in order, by name) and optional filters, each unset matching every event: events (message types such as \"sign.request\", or \"request.<status>\"), " +
		"from (\"self\", \"others\" or Node IDs), handles (Handles, or \"user\" for a person), conversation (\"group\", Key aliases or Workspace names) and text (a regular expression)",
	kindSchedule: "a Schedule has jobs (the Jobs it runs in order, by name) and exactly one of cron (a five-field spec, read in tz, an IANA zone, or else the Node's local zone) " +
		"or at (an RFC 3339 time with an offset, firing once); an agent Job it runs must set its target",
}

// triggerToolDescription is what Tool name, of kind k and verb, tells the
// model.
func triggerToolDescription(k triggerKind, verb string) string {
	const effect = "What you write takes effect at once and runs unattended. "
	switch verb {
	case "list":
		s := fmt.Sprintf(`List this Group's %[1]ss, as JSON: [{"name":"<name>","%[2]s":{...},"by":"<Handle>","aliases":{"<Workspace name>":"<Key alias>"}}], `+
			"each %[2]s with its config.toml keys. by is the agent that last wrote it, absent for a person's. aliases names the Key alias of each Workspace name it gives, for a Key with one.", k.noun, k.field)
		if k == kindSchedule {
			s += ` next is when it next fires, absent if it fires no more.`
		}
		return s + " Read-only."
	case "add":
		return fmt.Sprintf("Add a %[1]s to this Group's config.toml, by a name not already defined: lowercase letters, digits and underscores. "+
			"Give the %[1]s as %[2]s, a JSON object with its config.toml keys: %[3]s. "+
			"by is set to your Handle; do not give it. A Key may be named by its alias or Workspace name, and its Workspace name is written. "+
			effect+"A refusal says what to correct.", k.noun, k.field, triggerFields[k])
	case "edit":
		return fmt.Sprintf("Replace the definition of a %[1]s already defined in this Group's config.toml with %[2]s, whole, as %[3]s_add takes it: "+
			"keys you leave out are unset. by is set to your Handle. "+effect+"A refusal says what to correct.", k.noun, k.field, k.field)
	}
	s := fmt.Sprintf("Delete a %s of this Group's config.toml, by name.", k.noun)
	if k == kindJob {
		s += " A Job a Hook or Schedule still runs is refused, naming them."
	}
	return s
}

// triggerToolSchema is Tool name's input.
func triggerToolSchema(k triggerKind, verb string) json.RawMessage {
	name := fmt.Sprintf(`"name":{"type":"string","description":"The %s's name."}`, k.noun)
	switch verb {
	case "list":
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	case "add", "edit":
		return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%s,"%s":{"type":"object","description":"The %s, with its config.toml keys."}},"required":["name","%s"],"additionalProperties":false}`,
			name, k.field, k.noun, k.field))
	}
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%s},"required":["name"],"additionalProperties":false}`, name))
}

// triggerTool is Trigger-config Tool name for handle's run, ok false if
// name is none. It acts on the served Group, for it runs only in
// Group-level talk, as placed has checked.
func (s *server) triggerTool(name, handle string) (agent.Tool, bool) {
	tk, ok := triggerToolKinds[name]
	if !ok {
		return agent.Tool{}, false
	}
	t := agent.Tool{Name: name, Description: triggerToolDescription(tk.kind, tk.verb), Schema: triggerToolSchema(tk.kind, tk.verb)}
	switch tk.verb {
	case "list":
		t.Call = s.triggerListTool(tk.kind)
	case "add", "edit":
		t.Call = s.triggerSetTool(tk.kind, tk.verb == "edit", handle)
	default:
		t.Call = s.triggerDeleteTool(tk.kind)
	}
	return t, true
}

// TriggerEntry is one entry a list Tool returns: its name, the entry
// under its kind's field with its config.toml keys, who last wrote it,
// the Key alias of each Workspace name it gives, and a Schedule's next
// firing.
type TriggerEntry struct {
	Name     string            `json:"name"`
	Job      map[string]any    `json:"job,omitempty"`
	Hook     map[string]any    `json:"hook,omitempty"`
	Schedule map[string]any    `json:"schedule,omitempty"`
	By       string            `json:"by,omitempty"`
	Aliases  map[string]string `json:"aliases,omitempty"`
	Next     *time.Time        `json:"next,omitempty"`
}

// triggerListTool lists k's entries of the Group's config.toml as it is
// when called.
func (s *server) triggerListTool(k triggerKind) func(context.Context, json.RawMessage) (string, error) {
	return func(_ context.Context, input json.RawMessage) (string, error) {
		dec := json.NewDecoder(bytes.NewReader(input))
		dec.DisallowUnknownFields()
		var in struct{}
		if len(bytes.TrimSpace(input)) > 0 && dec.Decode(&in) != nil {
			return "", errors.New("its input must be {}")
		}
		c, err := s.triggers().Config()
		if err != nil {
			return "", err
		}
		places, err := s.keyPlaces()
		if err != nil {
			return "", err
		}
		out := []TriggerEntry{}
		add := func(name string, v any, by string, refs []string) error {
			fields, err := tomlFields(v)
			if err != nil {
				return err
			}
			e := TriggerEntry{Name: name, By: by, Aliases: places.aliasesOf(refs)}
			switch k {
			case kindJob:
				e.Job = fields
			case kindHook:
				e.Hook = fields
			case kindSchedule:
				e.Schedule = fields
			}
			out = append(out, e)
			return nil
		}
		now := time.Now()
		switch k {
		case kindJob:
			for _, name := range slices.Sorted(maps.Keys(c.Jobs)) {
				j := c.Jobs[name]
				if err := add(name, j, j.By, []string{j.Target}); err != nil {
					return "", err
				}
			}
		case kindHook:
			for _, name := range slices.Sorted(maps.Keys(c.Hooks)) {
				h := c.Hooks[name]
				if err := add(name, h, h.By, h.Conversation); err != nil {
					return "", err
				}
			}
		case kindSchedule:
			for _, name := range slices.Sorted(maps.Keys(c.Schedules)) {
				sc := c.Schedules[name]
				if err := add(name, sc, sc.By, nil); err != nil {
					return "", err
				}
				if next := sc.Next(now); !next.IsZero() {
					out[len(out)-1].Next = &next
				}
			}
		}
		return toolJSON(out)
	}
}

// triggerSetTool is k's add Tool, or with edit its edit Tool, for
// handle's run: the entry given, its Keys named by Workspace name and its
// by handle.
func (s *server) triggerSetTool(k triggerKind, edit bool, handle string) func(context.Context, json.RawMessage) (string, error) {
	return func(_ context.Context, input json.RawMessage) (string, error) {
		shape := fmt.Sprintf(`its input must be {"name": "<name>", "%s": {<its config.toml keys>}}`, k.field)
		var in map[string]json.RawMessage
		if err := json.Unmarshal(input, &in); err != nil || in == nil {
			return "", errors.New(shape)
		}
		var name string
		if err := json.Unmarshal(in["name"], &name); err != nil || name == "" || in[k.field] == nil || len(in) != 2 {
			return "", errors.New(shape)
		}
		tc := s.triggers()
		places, err := s.keyPlaces()
		if err != nil {
			return "", err
		}
		verb := "added"
		if edit {
			verb = "edited"
		}
		switch k {
		case kindJob:
			var j config.Job
			if err := fromFields(in[k.field], &j); err != nil {
				return "", fmt.Errorf("job: %w", err)
			}
			if j.Target != "" {
				if j.Target, err = places.workspace(j.Target); err != nil {
					return "", fmt.Errorf("job: target: %w", err)
				}
			}
			j.By = handle
			if edit {
				err = tc.EditJob(name, j)
			} else {
				err = tc.AddJob(name, j)
			}
		case kindHook:
			var hk config.Hook
			if err := fromFields(in[k.field], &hk); err != nil {
				return "", fmt.Errorf("hook: %w", err)
			}
			for i, c := range hk.Conversation {
				if hk.Conversation[i], err = places.workspace(c); err != nil {
					return "", fmt.Errorf("hook: conversation: %w", err)
				}
			}
			hk.By = handle
			if edit {
				err = tc.EditHook(name, hk)
			} else {
				err = tc.AddHook(name, hk)
			}
		case kindSchedule:
			var sc config.Schedule
			if err := fromFields(in[k.field], &sc); err != nil {
				return "", fmt.Errorf("schedule: %w", err)
			}
			sc.By = handle
			if edit {
				err = tc.EditSchedule(name, sc)
			} else {
				err = tc.AddSchedule(name, sc)
			}
		}
		if err != nil {
			return "", toolError(err)
		}
		return fmt.Sprintf("%s %s %s.", k.noun, name, verb), nil
	}
}

// triggerDeleteTool is k's delete Tool.
func (s *server) triggerDeleteTool(k triggerKind) func(context.Context, json.RawMessage) (string, error) {
	return func(_ context.Context, input json.RawMessage) (string, error) {
		var in struct {
			Name string `json:"name"`
		}
		dec := json.NewDecoder(bytes.NewReader(input))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || in.Name == "" {
			return "", errors.New(`its input must be {"name": "<name>"}`)
		}
		tc := s.triggers()
		var err error
		switch k {
		case kindJob:
			err = tc.DeleteJob(in.Name)
		case kindHook:
			err = tc.DeleteHook(in.Name)
		case kindSchedule:
			err = tc.DeleteSchedule(in.Name)
		}
		if err != nil {
			return "", toolError(err)
		}
		return fmt.Sprintf("%s %s deleted.", k.noun, in.Name), nil
	}
}

// triggers is the served Group's Trigger config, as the Trigger-config
// Tools write it: checked as a person's save is, but keeping the by the
// Tool sets (ADR 0046).
func (s *server) triggers() config.Triggers {
	return config.Triggers{Paths: s.opts.Paths, Group: s.opts.Alias}
}

// keyPlaces names this Group's Keys' Conversations, as the Trigger-config
// Tools take and give them: by Workspace name, the name written, and by
// Key alias.
type keyPlaces struct {
	keys    []key.Key
	names   map[[32]byte]string
	aliases map[string]config.KeyAlias
}

// keyPlaces reads the Group's Keys and their aliases as they are now.
func (s *server) keyPlaces() (keyPlaces, error) {
	_, store, running := s.current()
	if !running {
		return keyPlaces{}, ErrNoReplica
	}
	keys, err := key.Read(store)
	if err != nil {
		return keyPlaces{}, err
	}
	return keyPlaces{keys: keys, names: key.WorkspaceNames(keys), aliases: config.ReadKeyAliases(s.opts.Paths, s.opts.Alias, keys)}, nil
}

// workspace is the Workspace name ref names: "group" as itself, else a
// Key's by Workspace name or Key alias.
func (kp keyPlaces) workspace(ref string) (string, error) {
	if ref == config.ConversationGroup {
		return ref, nil
	}
	for _, n := range kp.names {
		if n == ref {
			return n, nil
		}
	}
	k, err := config.ResolveKey(kp.keys, kp.aliases, ref)
	if errors.Is(err, key.ErrUnknown) {
		return "", fmt.Errorf("%q is neither %q, a Key alias nor the Workspace name of a Key of this Group", ref, config.ConversationGroup)
	}
	if err != nil {
		return "", err
	}
	return kp.names[key.ConversationID(k.Curve, k.PublicKey)], nil
}

// aliasesOf are the Key aliases of the Workspace names among refs, of
// those Keys that have one; nil for none.
func (kp keyPlaces) aliasesOf(refs []string) map[string]string {
	var out map[string]string
	for _, k := range kp.keys {
		name := kp.names[key.ConversationID(k.Curve, k.PublicKey)]
		alias := kp.aliases[k.Ref()].Alias
		if alias == "" || !slices.Contains(refs, name) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[name] = alias
	}
	return out
}

// tomlFields is v, a Job, Hook or Schedule, as the JSON values of its
// config.toml keys, without its by.
func tomlFields(v any) (map[string]any, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	out := map[string]any{}
	if _, err := toml.Decode(buf.String(), &out); err != nil {
		return nil, err
	}
	delete(out, "by")
	return out, nil
}

// fromFields decodes raw, a JSON object of config.toml keys, into v, a
// Job, Hook or Schedule, as config.toml would read it: a key it does not
// know, or by, is refused.
func fromFields(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return errors.New("must be a JSON object of its config.toml keys")
	}
	if _, ok := m["by"]; ok {
		return errors.New("by is set to your Handle: leave it out")
	}
	fields, err := tomlValue(m)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(fields); err != nil {
		return err
	}
	md, err := toml.Decode(buf.String(), v)
	if err != nil {
		return err
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return fmt.Errorf("unknown key %s", strings.Join(keys, ", "))
	}
	return nil
}

// tomlValue is JSON value v as TOML encodes it: a whole number as an
// integer, and a null refused, for TOML has none.
func tomlValue(v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, errors.New("null is no value: leave the key out")
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		return x.Float64()
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			var err error
			if out[k], err = tomlValue(e); err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			var err error
			if out[i], err = tomlValue(e); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return v, nil
}
