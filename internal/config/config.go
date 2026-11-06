// Package config reads and writes a Workspace's config.toml (docs/spec/mvp.md
// §3.3, docs/adr/0013-group-config-workspaces-and-named-secrets.md) and an
// Agent Profile's (ADR 0029). The Group's config.toml holds this Node's
// tailnet presence and the Workspace sections; a Conversation's holds only
// the Workspace sections, merged over the Group's. A secret-bearing field is
// always a "$NAME" reference.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"defios.com/internal/agent"
	"defios.com/internal/group"
	"defios.com/internal/mention"
	"defios.com/internal/network"
	"defios.com/internal/plugin"
	"defios.com/internal/provider"
	"defios.com/internal/secret"
)

// Config is a Group's config.toml.
type Config struct {
	HeadscaleURL string `toml:"headscale_url"`
	DisplayName  string `toml:"display_name"`
	Hostname     string `toml:"hostname"`
	// TailnetAuthKey is a reference to the tailnet preauth key; group new
	// writes "$TS_AUTHKEY".
	TailnetAuthKey string `toml:"tailnet_auth_key,omitempty"`
	// Prune is this Node's prune policy for the Group: when set, serve
	// destroys the payload of all talk older than it on start, after every
	// snapshot restore and once a day (ADR 0024). Unset, the Node is
	// archival.
	Prune Age `toml:"prune,omitzero"`
	Workspace
}

// Profile is one Agent Profile's config.toml: the Provider alias it runs on
// and its request body (ADR 0018, ADR 0019). It lives beside SYSTEM.md in
// the profile directory, agents/<handle> under the config directory, one per
// Handle on the Node (ADR 0026, ADR 0029); a Workspace's [agent] profiles
// includes it by Handle.
type Profile struct {
	// Provider is the alias of the Provider it calls, resolved at summon.
	Provider string `toml:"provider"`
	// MaxTurns is how many model calls a summon may make; 0 is unset,
	// which is agent.DefaultMaxTurns.
	MaxTurns int `toml:"max_turns,omitzero"`
	// Tools is the whole list of built-in Tools it may call (ADR 0029): nil
	// is unset, say alone; empty grants none.
	Tools []string `toml:"tools,omitempty"`
	// Skills are the Skills it may load, each <plugin>/<skill> (ADR 0030).
	// That each is installed is checked at summon, so a profile naming a
	// Plugin a restore could not fetch still reads.
	Skills []string `toml:"skills,omitempty"`
	// Body is the request's other fields, as TOML nests them: whatever the
	// API shape takes. It merges over its Provider's body, which merges over
	// what DeFiOS writes. ReadFile converts it to JSON values.
	Body map[string]any `toml:"body,omitempty"`
}

// The built-in Tools a profile's tools may grant (ADR 0029).
const (
	ToolSay         = agent.SayName
	ToolSignList    = "sign_list"
	ToolSignInspect = "sign_inspect"
	ToolSignPropose = "sign_propose"
	ToolSignAccept  = "sign_accept"
	ToolSignRefuse  = "sign_refuse"
)

// ToolSkillLoad reads a granted Skill (ADR 0030). It is offered whenever a
// profile's skills is set, after its tools, and is never itself in tools.
const ToolSkillLoad = "skill_load"

// Tools is every built-in Tool, in the order a run offers them.
var Tools = []string{ToolSay, ToolSignList, ToolSignInspect, ToolSignPropose, ToolSignAccept, ToolSignRefuse}

// Granted is the Tools p grants: its tools, or unset, say alone.
func (p Profile) Granted() []string {
	if p.Tools == nil {
		return []string{ToolSay}
	}
	return p.Tools
}

// ValidateTool refuses a name that is no built-in Tool.
func ValidateTool(name string) error {
	if name == ToolSkillLoad {
		return fmt.Errorf("%s is not granted as a Tool: it is offered whenever skills grants a Skill", name)
	}
	if !slices.Contains(Tools, name) {
		return fmt.Errorf("no Tool %q: the Tools are %s", name, strings.Join(Tools, ", "))
	}
	return nil
}

// Workspace is the part of a config.toml a Conversation's may override.
type Workspace struct {
	// Networks are this Node's Networks, by alias: the chains it signs for
	// and the endpoints it reaches them by (ADR 0023). ReadFile fills no
	// Alias; network.List does.
	Networks map[string]network.Network `toml:"networks,omitempty"`
	// Providers are the LLM APIs this Node's agents may call, by alias.
	Providers map[string]Provider `toml:"providers,omitempty"`
	Agent     Agent               `toml:"agent,omitempty"`
}

// Provider is the whole of a Provider, this Node's own (ADR 0022): where it
// is called, whose api_path also picks its API Shape (ADR 0019); its
// credential, a Secret reference sent only to that base_url, without which
// no key is sent; and the base request body for every profile that runs on
// it (ADR 0020). A Group's table sets base_url and api_path; a
// Conversation's may override any key of it.
type Provider struct {
	BaseURL string `toml:"base_url,omitempty"`
	APIPath string `toml:"api_path,omitempty"`
	APIKey  string `toml:"api_key,omitempty"`
	// Body is the request fields shared by the profiles that run on this
	// Provider, as TOML nests them; each profile's body merges over it.
	// Reading the config converts it to JSON values.
	Body map[string]any `toml:"body,omitempty"`
}

// Endpoint is where p is called, as the Provider alias.
func (p Provider) Endpoint(alias string) provider.Provider {
	return provider.Provider{Alias: alias, BaseURL: p.BaseURL, APIPath: p.APIPath}
}

// Provider is the Provider alias names in w, a Group's Workspace with any
// Conversation's merged over it: provider.ErrUnknown if w has none, and
// provider.ErrInvalid if a Conversation's table for an alias the Group
// lacks left it incomplete.
func (w Workspace) Provider(alias string) (Provider, error) {
	p, ok := w.Providers[alias]
	if !ok {
		return Provider{}, fmt.Errorf("%w: %q", provider.ErrUnknown, alias)
	}
	if err := provider.Validate(p.Endpoint(alias)); err != nil {
		return Provider{}, fmt.Errorf("[providers.%s]: %w", alias, err)
	}
	return p, nil
}

// Agent is which agents may answer in Group-level talk, or in that
// Conversation, and how deep a chain of them may go.
type Agent struct {
	// Profiles are the Handles a mention may summon here. Set in a
	// Conversation, even empty, it replaces the Group's list.
	Profiles []string `toml:"profiles,omitempty"`
	// MaxDepth is the depth a chain of agents may reach; 0 is unset, and
	// unset everywhere is 1.
	MaxDepth int `toml:"max_depth,omitzero"`
}

// WriteFile writes a Group's config.toml, mode 0600, keeping an [agent]
// profiles set empty as WriteWorkspace does.
func WriteFile(path string, c Config) error {
	return write(path, c, c.Agent)
}

// WriteWorkspace writes a Conversation's config.toml, mode 0600. The encoder
// drops an empty list, so an [agent] profiles set empty, which replaces the
// Group's list with none, is written under the [agent] header by hand.
func WriteWorkspace(path string, w Workspace) error {
	return write(path, w, w.Agent)
}

// agentHeader is the [agent] table's header as the encoder writes it.
var agentHeader = regexp.MustCompile(`(?m)^\[agent\]\n`)

// ErrNotWritten is a write refused because ReadFile or ReadWorkspace would
// refuse what it writes: a config that does not read back stops every
// command and summon that reads it until it is fixed by hand.
var ErrNotWritten = errors.New("not written")

// write encodes v, whose [agent] table is a, to path, once it reads back.
func write(path string, v any, a Agent) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(v); err != nil {
		return fmt.Errorf("config: %w; %w", err, ErrNotWritten)
	}
	out := buf.Bytes()
	if p, ok := v.(Profile); ok && p.Tools != nil && len(p.Tools) == 0 {
		out = append([]byte("tools = []\n"), out...)
	}
	if a.Profiles != nil && len(a.Profiles) == 0 {
		out = agentHeader.ReplaceAll(out, []byte("[agent]\nprofiles = []\n"))
	}
	var err error
	switch v.(type) {
	case Config:
		_, err = parseConfig(path, out)
	case Profile:
		_, err = parseProfile(path, out)
	default:
		_, err = parseWorkspace(path, out)
	}
	if err != nil {
		return fmt.Errorf("%w; %w", err, ErrNotWritten)
	}
	return os.WriteFile(path, out, 0o600)
}

// ReadFile parses and validates a Group's config.toml.
func ReadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return parseConfig(path, data)
}

// parseConfig is ReadFile of data, the contents of path.
func parseConfig(path string, data []byte) (Config, error) {
	var c Config
	md, err := decode(path, data, &c)
	if err != nil {
		return Config{}, err
	}
	if c.TailnetAuthKey != "" {
		if _, err := secret.ParseRef(c.TailnetAuthKey); err != nil {
			return Config{}, fmt.Errorf("config: %s: tailnet_auth_key: %w", path, err)
		}
	}
	if err := c.Workspace.validate(md, true); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return c, nil
}

// WriteProfile writes an Agent Profile's config.toml, mode 0600, once it
// reads back. The encoder drops an empty list, so tools set empty, which
// grants none, is written first by hand.
func WriteProfile(path string, p Profile) error {
	return write(path, p, Agent{})
}

// ReadProfile parses and validates an Agent Profile's config.toml. A missing
// file is an error wrapping os.ErrNotExist.
func ReadProfile(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, fmt.Errorf("config: %w", err)
	}
	return parseProfile(path, data)
}

// parseProfile is ReadProfile of data, the contents of path. md tells an
// explicit max_turns of 0 from an unset one; the body is converted to JSON
// values.
func parseProfile(path string, data []byte) (Profile, error) {
	var p Profile
	md, err := decode(path, data, &p)
	if err != nil {
		return Profile{}, err
	}
	fail := func(format string, a ...any) (Profile, error) {
		return Profile{}, fmt.Errorf("config: %s: "+format, append([]any{path}, a...)...)
	}
	if p.Provider == "" {
		return fail("provider is required: the alias of the Provider it runs on")
	}
	if err := group.ValidateAlias(p.Provider); err != nil {
		return fail("provider: %w", err)
	}
	if md.IsDefined("max_turns") && p.MaxTurns < 1 {
		return fail("max_turns %d must be a positive integer", p.MaxTurns)
	}
	for i, t := range p.Tools {
		if err := ValidateTool(t); err != nil {
			return fail("tools: %w", err)
		}
		if slices.Contains(p.Tools[:i], t) {
			return fail("tools: %q is listed twice", t)
		}
	}
	for i, ref := range p.Skills {
		if _, _, err := plugin.ParseSkillRef(ref); err != nil {
			return fail("skills: %w", err)
		}
		if slices.Contains(p.Skills[:i], ref) {
			return fail("skills: %q is listed twice", ref)
		}
	}
	if _, ok := p.Body[""]; ok {
		return fail("body: a key may not be empty")
	}
	if t := md.Type("body"); t != "" && t != "Hash" {
		return fail("body must be a table")
	}
	body, err := jsonValue(p.Body)
	if err != nil {
		return fail("[body]%w", err)
	}
	if p.Body != nil {
		p.Body = body.(map[string]any)
	}
	return p, nil
}

// ReadWorkspace parses and validates a Conversation's config.toml, which may
// hold only the Workspace sections. A missing file is an empty Workspace.
func ReadWorkspace(path string) (Workspace, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Workspace{}, nil
	} else if err != nil {
		return Workspace{}, fmt.Errorf("config: %w", err)
	}
	return parseWorkspace(path, data)
}

// parseWorkspace is ReadWorkspace of data, the contents of path.
func parseWorkspace(path string, data []byte) (Workspace, error) {
	var w Workspace
	md, err := decode(path, data, &w)
	if err != nil {
		return Workspace{}, err
	}
	if err := w.validate(md, false); err != nil {
		return Workspace{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return w, nil
}

// decode refuses any key that into does not define, so a misplaced or misspelt
// secret is an error rather than ignored.
func decode(path string, data []byte, into any) (toml.MetaData, error) {
	md, err := toml.Decode(string(data), into)
	if err != nil {
		return md, fmt.Errorf("config: %w", err)
	}
	_, isProfile := into.(*Profile)
	var keys []string
	for _, k := range md.Undecoded() {
		// A profile's or Provider's body is free-form: whatever it holds is
		// decoded into it, though the decoder does not mark nested keys as
		// such.
		if (isProfile && len(k) > 1 && k[0] == "body") || (len(k) > 3 && k[2] == "body" && k[0] == "providers") {
			continue
		}
		keys = append(keys, k.String())
	}
	if len(keys) > 0 {
		return md, fmt.Errorf("config: %s: unknown key %s", path, strings.Join(keys, ", "))
	}
	return md, nil
}

// validate checks w; md tells an explicit max_depth = 0 from an unset one.
// A Group's Workspace is complete: each Provider sets its base_url and
// api_path. A Conversation's need not be, for its tables override the
// Group's key by key. A Network table is always whole.
func (w Workspace) validate(md toml.MetaData, complete bool) error {
	if _, err := network.List(w.Networks); err != nil {
		return fmt.Errorf("[networks]: %w", err)
	}
	for alias, p := range w.Providers {
		check := provider.ValidateOverride
		if complete {
			check = provider.Validate
		}
		if err := check(p.Endpoint(alias)); err != nil {
			return fmt.Errorf("[providers.%s]: %w", alias, err)
		}
		if p.APIKey != "" {
			if _, err := secret.ParseRef(p.APIKey); err != nil {
				return fmt.Errorf("[providers.%s] api_key: %w", alias, err)
			}
		}
		if _, ok := p.Body[""]; ok {
			return fmt.Errorf("[providers.%s] body: a key may not be empty", alias)
		}
		if t := md.Type("providers", alias, "body"); t != "" && t != "Hash" {
			return fmt.Errorf("[providers.%s] body must be a table", alias)
		}
		body, err := jsonValue(p.Body)
		if err != nil {
			return fmt.Errorf("[providers.%s.body]%w", alias, err)
		}
		if p.Body != nil {
			p.Body = body.(map[string]any)
			w.Providers[alias] = p
		}
	}
	listed := map[string]bool{}
	for _, h := range w.Agent.Profiles {
		if err := ValidateHandle(h); err != nil {
			return fmt.Errorf("[agent] profiles: %w", err)
		}
		if listed[h] {
			return fmt.Errorf("[agent] profiles: %q is listed twice", h)
		}
		listed[h] = true
	}
	if md.IsDefined("agent", "max_depth") && w.Agent.MaxDepth < 1 {
		return fmt.Errorf("[agent] max_depth %d must be at least 1", w.Agent.MaxDepth)
	}
	return nil
}

// ValidateHandle checks a Handle against ADR 0016's grammar, so every Handle
// a config defines can be mentioned, and that it is not "user", which is how
// a person's talk is shown.
func ValidateHandle(h string) error {
	if h == "user" {
		return errors.New(`"user" is reserved for a person and cannot be a Handle`)
	}
	if !mention.ValidHandle(h) {
		return fmt.Errorf("handle: %q is not a valid Handle; use 1–64 chars of [a-z0-9] runs joined by single - or _", h)
	}
	return nil
}

// Merge is w with over laid on it table by table: a whole Network table, a
// Provider's base_url, api_path or api_key, an [agent] profiles list (even
// an empty one), or a max_depth that over sets wins, and everything over
// leaves unset comes from w. A Provider's body in over merges over w's as a
// profile's body does. Neither input is changed. Merged Networks that name
// one chain twice, an alias from each, are refused as either file's would
// be (ADR 0023).
func (w Workspace) Merge(over Workspace) (Workspace, error) {
	out := Workspace{Agent: w.Agent}
	if len(w.Networks)+len(over.Networks) > 0 {
		out.Networks = map[string]network.Network{}
		for a, n := range w.Networks {
			out.Networks[a] = n
		}
		for a, n := range over.Networks {
			out.Networks[a] = n
		}
	}
	if len(w.Providers)+len(over.Providers) > 0 {
		out.Providers = map[string]Provider{}
		for name, p := range w.Providers {
			out.Providers[name] = p
		}
		for name, p := range over.Providers {
			cur := out.Providers[name]
			if p.BaseURL != "" {
				cur.BaseURL = p.BaseURL
			}
			if p.APIPath != "" {
				cur.APIPath = p.APIPath
			}
			if p.APIKey != "" {
				cur.APIKey = p.APIKey
			}
			if p.Body != nil {
				cur.Body = agent.MergeBody(cur.Body, p.Body)
			}
			out.Providers[name] = cur
		}
	}
	if over.Agent.Profiles != nil {
		out.Agent.Profiles = over.Agent.Profiles
	}
	if over.Agent.MaxDepth != 0 {
		out.Agent.MaxDepth = over.Agent.MaxDepth
	}
	if _, err := network.List(out.Networks); err != nil {
		return Workspace{}, fmt.Errorf("[networks]: %w", err)
	}
	return out, nil
}

// Included is what may answer in one place, Group-level talk or one
// Conversation: its profiles in the order [agent] profiles lists them, and
// the depth a chain of them may reach.
type Included struct {
	Profiles []ResolvedProfile
	MaxDepth int
}

// Summons reports whether a message at depth, 0 for a person's, summons
// agents under in: an agent answers at most at MaxDepth.
func (in Included) Summons(depth uint32) bool { return int(depth) < in.MaxDepth }

// ResolvedProfile is an included Agent Profile with its directory resolved,
// max_turns and tools defaulted, and its body merged over its Provider's.
type ResolvedProfile struct {
	Handle   string
	Dir      string // absolute
	Provider string // a Provider alias
	MaxTurns int
	Tools    []string       // the Tools it grants, never nil
	Skills   []string       // the Skills it grants, <plugin>/<skill>
	Body     map[string]any // JSON values; nil when neither body is set
}

// jsonValue is v, a value TOML decoded, as a JSON value: a table is an
// object, an array of tables an array of objects. A date-time, date or time
// has no JSON form and is an error, as is a NaN or infinite float; the
// error is the path to the value.
func jsonValue(v any) (any, error) {
	switch x := v.(type) {
	case nil, string, bool, int64:
		return x, nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf(": %v has no JSON form", x)
		}
		return x, nil
	case time.Time:
		return nil, fmt.Errorf(": a TOML date or time has no JSON form; quote it as a string")
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			j, err := jsonValue(e)
			if err != nil {
				return nil, fmt.Errorf(".%s%w", k, err)
			}
			out[k] = j
		}
		return out, nil
	case []map[string]any:
		a := make([]any, len(x))
		for i, e := range x {
			a[i] = e
		}
		return jsonValue(a)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			j, err := jsonValue(e)
			if err != nil {
				return nil, fmt.Errorf("[%d]%w", i, err)
			}
			out[i] = j
		}
		return out, nil
	}
	return nil, fmt.Errorf(": a %T has no JSON form", v)
}
