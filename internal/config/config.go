// Package config reads and writes a Workspace's config.toml (docs/spec/mvp.md
// §3.3, docs/adr/0013-group-config-workspaces-and-named-secrets.md). The
// Group's config.toml holds this Node's tailnet presence and the Workspace
// sections; a Conversation's holds only the Workspace sections, merged over
// the Group's. A secret-bearing field is always a "$NAME" reference.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"defios.com/internal/agent"
	"defios.com/internal/group"
	"defios.com/internal/mention"
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
	// destroys the payload of all talk a live Recap accounts for, again
	// after every Recap and snapshot restore (ADR 0012). Off by default.
	Prune bool `toml:"prune,omitempty"`
	// Agents are this Node's Agent Profiles for the Group, by Handle (ADR
	// 0015). Only the Group's config.toml may define them; a Workspace's
	// [agent] profiles includes them by Handle.
	Agents map[string]Profile `toml:"agents,omitempty"`
	Workspace
}

// Profile is one Agent Profile: its profile directory, the Provider alias it
// runs on, and its request body (ADR 0018, ADR 0019).
type Profile struct {
	// Dir is the profile directory, holding SYSTEM.md. "~" expands, a
	// relative path is under the config directory, and empty is
	// agents/<handle>.
	Dir string `toml:"dir,omitempty"`
	// Provider is the alias of the Provider it calls, resolved at summon.
	Provider string `toml:"provider"`
	// MaxTurns is how many model calls a summon may make; 0 is unset,
	// which is agent.DefaultMaxTurns.
	MaxTurns int `toml:"max_turns,omitzero"`
	// Body is the request's other fields, as TOML nests them: whatever the
	// API shape takes. It merges over its Provider's body, which merges over
	// what DeFiOS writes. ReadFile converts it to JSON values.
	Body map[string]any `toml:"body,omitempty"`
}

// Workspace is the part of a config.toml a Conversation's may override.
type Workspace struct {
	// Networks are this Node's local RPC endpoints, by Network alias.
	Networks map[string]Network `toml:"networks,omitempty"`
	// Providers hold this Node's credential for a Provider, by its alias.
	Providers map[string]Provider `toml:"providers,omitempty"`
	Agent     Agent               `toml:"agent,omitempty"`
}

// Network is a Network's local RPC endpoints, each a reference.
type Network struct {
	RPCURLs []string `toml:"rpc_urls"`
}

// Provider is this Node's settings for a Provider: its credential, a Secret
// reference sent to the Provider's base_url on the log (ADR 0019), without
// which no key is sent; and the base request body for every profile that
// runs on it.
type Provider struct {
	APIKey string `toml:"api_key,omitempty"`
	// Body is the request fields shared by the profiles that run on this
	// Provider, as TOML nests them; each profile's body merges over it.
	// Reading the config converts it to JSON values.
	Body map[string]any `toml:"body,omitempty"`
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

// WriteFile writes config.toml, mode 0600.
func WriteFile(path string, c Config) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// ReadFile parses and validates a Group's config.toml.
func ReadFile(path string) (Config, error) {
	var c Config
	md, err := decode(path, &c)
	if err != nil {
		return Config{}, err
	}
	if c.TailnetAuthKey != "" {
		if _, err := secret.ParseRef(c.TailnetAuthKey); err != nil {
			return Config{}, fmt.Errorf("config: %s: tailnet_auth_key: %w", path, err)
		}
	}
	if err := c.Workspace.validate(md); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	if err := c.validateAgents(md); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return c, nil
}

// ReadWorkspace parses and validates a Conversation's config.toml, which may
// hold only the Workspace sections. A missing file is an empty Workspace.
func ReadWorkspace(path string) (Workspace, error) {
	var w Workspace
	md, err := decode(path, &w)
	if errors.Is(err, os.ErrNotExist) {
		return Workspace{}, nil
	} else if err != nil {
		return Workspace{}, err
	}
	if err := w.validate(md); err != nil {
		return Workspace{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return w, nil
}

// decode refuses any key that into does not define, so a misplaced or misspelt
// secret is an error rather than ignored.
func decode(path string, into any) (toml.MetaData, error) {
	md, err := toml.DecodeFile(path, into)
	if err != nil {
		return md, fmt.Errorf("config: %w", err)
	}
	_, isGroup := into.(*Config)
	var keys []string
	for _, k := range md.Undecoded() {
		// A profile's or Provider's body is free-form: whatever it holds is
		// decoded into it, though the decoder does not mark nested keys as
		// such.
		if len(k) > 3 && k[2] == "body" && (k[0] == "providers" || isGroup && k[0] == "agents") {
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
func (w Workspace) validate(md toml.MetaData) error {
	for alias, n := range w.Networks {
		if err := group.ValidateAlias(alias); err != nil {
			return fmt.Errorf("[networks.%s]: %w", alias, err)
		}
		for _, u := range n.RPCURLs {
			if _, err := secret.ParseRef(u); err != nil {
				return fmt.Errorf("[networks.%s] rpc_urls: %w", alias, err)
			}
		}
	}
	for alias, p := range w.Providers {
		if err := group.ValidateAlias(alias); err != nil {
			return fmt.Errorf("[providers.%s]: %w", alias, err)
		}
		if p.APIKey != "" {
			if _, err := secret.ParseRef(p.APIKey); err != nil {
				return fmt.Errorf("[providers.%s] api_key: %w", alias, err)
			}
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

// validateAgents checks the Group's Agent Profiles, converting each body to
// JSON values, and that its own [agent] profiles includes only Handles it
// defines; md tells an explicit max_turns of 0 from an unset one.
func (c Config) validateAgents(md toml.MetaData) error {
	for h, p := range c.Agents {
		if err := ValidateHandle(h); err != nil {
			return fmt.Errorf("[agents.%s]: %w", h, err)
		}
		if p.Provider == "" {
			return fmt.Errorf("[agents.%s]: provider is required: the alias of the Provider it runs on", h)
		}
		if err := group.ValidateAlias(p.Provider); err != nil {
			return fmt.Errorf("[agents.%s] provider: %w", h, err)
		}
		if md.IsDefined("agents", h, "max_turns") && p.MaxTurns < 1 {
			return fmt.Errorf("[agents.%s] max_turns %d must be a positive integer", h, p.MaxTurns)
		}
		if t := md.Type("agents", h, "body"); t != "" && t != "Hash" {
			return fmt.Errorf("[agents.%s] body must be a table", h)
		}
		body, err := jsonValue(p.Body)
		if err != nil {
			return fmt.Errorf("[agents.%s.body]%w", h, err)
		}
		if p.Body != nil {
			p.Body = body.(map[string]any)
			c.Agents[h] = p
		}
	}
	for _, h := range c.Agent.Profiles {
		if _, ok := c.Agents[h]; !ok {
			return fmt.Errorf("[agent] profiles: no [agents.%s] is defined", h)
		}
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

// Merge is w with over laid on it table by table: a Network's rpc_urls, a
// Provider's api_key, an [agent] profiles list (even an empty
// one), or a max_depth that over sets wins, and everything over leaves unset
// comes from w. A Provider's body in over merges over w's as a profile's
// body does. Neither input is changed.
func (w Workspace) Merge(over Workspace) Workspace {
	out := Workspace{Agent: w.Agent}
	if len(w.Networks)+len(over.Networks) > 0 {
		out.Networks = map[string]Network{}
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
	return out
}

// Included is what may answer in one place, Group-level talk or one
// Conversation: its profiles in the order [agent] profiles lists them, and
// the depth a chain of them may reach.
type Included struct {
	Profiles []ResolvedProfile
	MaxDepth int
}

// ResolvedProfile is an included Agent Profile with its directory resolved,
// max_turns defaulted, and its body merged over its Provider's.
type ResolvedProfile struct {
	Handle   string
	Dir      string // absolute
	Provider string // a Provider alias
	MaxTurns int
	Body     map[string]any // JSON values; nil when neither body is set
}

// Included resolves the profiles and depth for talk whose Workspace is conv,
// over the Group's: pass an empty Workspace for Group-level talk. A relative
// profile directory is under root, the config directory, and "~" is home.
// Whether the directory exists is for whoever summons it to find out.
func (c Config) Included(conv Workspace, root, home string) (Included, error) {
	ws := c.Workspace.Merge(conv)
	a := ws.Agent
	out := Included{MaxDepth: a.MaxDepth}
	if out.MaxDepth == 0 {
		out.MaxDepth = 1
	}
	for _, h := range a.Profiles {
		p, ok := c.Agents[h]
		if !ok {
			return Included{}, fmt.Errorf("config: [agent] profiles: no [agents.%s] is defined in the Group config", h)
		}
		r := ResolvedProfile{Handle: h, Dir: profileDir(p.Dir, h, root, home), Provider: p.Provider,
			MaxTurns: p.MaxTurns, Body: p.Body}
		if base := ws.Providers[p.Provider].Body; base != nil {
			r.Body = agent.MergeBody(base, p.Body)
		}
		if r.MaxTurns == 0 {
			r.MaxTurns = agent.DefaultMaxTurns
		}
		out.Profiles = append(out.Profiles, r)
	}
	return out, nil
}

func profileDir(dir, handle, root, home string) string {
	switch {
	case dir == "":
		return filepath.Join(root, "agents", handle)
	case dir == "~" || strings.HasPrefix(dir, "~/"):
		return filepath.Join(home, dir[1:])
	case filepath.IsAbs(dir):
		return filepath.Clean(dir)
	default:
		return filepath.Join(root, dir)
	}
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
