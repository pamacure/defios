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
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"defios.com/internal/group"
	"defios.com/internal/model"
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

// Profile is one Agent Profile: its profile directory and the Model alias it
// runs on.
type Profile struct {
	// Dir is the profile directory, holding SYSTEM.md. "~" expands, a
	// relative path is under the config directory, and empty is
	// agents/<handle>.
	Dir   string `toml:"dir,omitempty"`
	Model string `toml:"model"`
}

// Workspace is the part of a config.toml a Conversation's may override.
type Workspace struct {
	// Networks are this Node's local RPC endpoints, by Network alias.
	Networks map[string]Network `toml:"networks,omitempty"`
	// Providers hold the credential for every Model of a provider, by
	// provider.
	Providers map[string]Provider `toml:"providers,omitempty"`
	Agent     Agent               `toml:"agent,omitempty"`
}

// Network is a Network's local RPC endpoints, each a reference.
type Network struct {
	RPCURLs []string `toml:"rpc_urls"`
}

// Provider is a provider's credential reference and the endpoint this Node
// trusts with it.
type Provider struct {
	APIKey  string `toml:"api_key,omitempty"`
	BaseURL string `toml:"base_url,omitempty"`
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
	if err := c.validateAgents(); err != nil {
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
	if extra := md.Undecoded(); len(extra) > 0 {
		keys := make([]string, len(extra))
		for i, k := range extra {
			keys[i] = k.String()
		}
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
	for name, p := range w.Providers {
		switch name {
		case model.Anthropic, model.OpenAI:
		default:
			return fmt.Errorf("[providers.%s]: provider must be anthropic or openai", name)
		}
		if p.APIKey != "" {
			if _, err := secret.ParseRef(p.APIKey); err != nil {
				return fmt.Errorf("[providers.%s] api_key: %w", name, err)
			}
		}
		if p.BaseURL != "" {
			if u, err := url.Parse(p.BaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("[providers.%s] base_url %q must be an http or https URL with a host", name, p.BaseURL)
			}
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

// validateAgents checks the Group's Agent Profiles, and that its own [agent]
// profiles includes only Handles it defines.
func (c Config) validateAgents() error {
	for h, p := range c.Agents {
		if err := ValidateHandle(h); err != nil {
			return fmt.Errorf("[agents.%s]: %w", h, err)
		}
		if p.Model == "" {
			return fmt.Errorf("[agents.%s]: model is required: the alias of the Model it runs on", h)
		}
		if err := group.ValidateAlias(p.Model); err != nil {
			return fmt.Errorf("[agents.%s] model: %w", h, err)
		}
	}
	for _, h := range c.Agent.Profiles {
		if _, ok := c.Agents[h]; !ok {
			return fmt.Errorf("[agent] profiles: no [agents.%s] is defined", h)
		}
	}
	return nil
}

// ValidateHandle checks a Handle: the alias rule, ending in a letter or
// digit, since a mention takes a trailing "-" or "_" as punctuation, and not
// "user", which is how a person's talk is shown (ADR 0015).
func ValidateHandle(h string) error {
	if h == "user" {
		return errors.New(`"user" is reserved for a person and cannot be a Handle`)
	}
	if err := group.ValidateAlias(h); err != nil {
		return fmt.Errorf("handle: %w", err)
	}
	if strings.HasSuffix(h, "-") || strings.HasSuffix(h, "_") {
		return fmt.Errorf("handle: %q must end in a letter or digit, so a mention can name it", h)
	}
	return nil
}

// Merge is w with over laid on it table by table: a Network's rpc_urls, a
// provider's api_key or base_url, an [agent] profiles list (even an empty
// one), or a max_depth that over sets wins, and everything over leaves unset
// comes from w. Neither input is changed.
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
			if p.BaseURL != "" {
				cur.BaseURL = p.BaseURL
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

// ResolvedProfile is an included Agent Profile with its directory resolved.
type ResolvedProfile struct {
	Handle string
	Dir    string // absolute
	Model  string // a Model alias
}

// Included resolves the profiles and depth for talk whose Workspace is conv,
// over the Group's: pass an empty Workspace for Group-level talk. A relative
// profile directory is under root, the config directory, and "~" is home.
// Whether the directory exists is for whoever summons it to find out.
func (c Config) Included(conv Workspace, root, home string) (Included, error) {
	a := c.Workspace.Merge(conv).Agent
	out := Included{MaxDepth: a.MaxDepth}
	if out.MaxDepth == 0 {
		out.MaxDepth = 1
	}
	for _, h := range a.Profiles {
		p, ok := c.Agents[h]
		if !ok {
			return Included{}, fmt.Errorf("config: [agent] profiles: no [agents.%s] is defined in the Group config", h)
		}
		out.Profiles = append(out.Profiles, ResolvedProfile{Handle: h, Dir: profileDir(p.Dir, h, root, home), Model: p.Model})
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
