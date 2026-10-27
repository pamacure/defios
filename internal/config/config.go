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
	Workspace
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

// Agent is what an agent runs on. Nothing reads it yet.
type Agent struct {
	Model       string `toml:"model,omitempty"`
	TokenWindow int    `toml:"token_window,omitempty"`
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
	if err := decode(path, &c); err != nil {
		return Config{}, err
	}
	if c.TailnetAuthKey != "" {
		if _, err := secret.ParseRef(c.TailnetAuthKey); err != nil {
			return Config{}, fmt.Errorf("config: %s: tailnet_auth_key: %w", path, err)
		}
	}
	if err := c.Workspace.validate(); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return c, nil
}

// ReadWorkspace parses and validates a Conversation's config.toml, which may
// hold only the Workspace sections. A missing file is an empty Workspace.
func ReadWorkspace(path string) (Workspace, error) {
	var w Workspace
	if err := decode(path, &w); errors.Is(err, os.ErrNotExist) {
		return Workspace{}, nil
	} else if err != nil {
		return Workspace{}, err
	}
	if err := w.validate(); err != nil {
		return Workspace{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return w, nil
}

// decode refuses any key that into does not define, so a misplaced or misspelt
// secret is an error rather than ignored.
func decode(path string, into any) error {
	md, err := toml.DecodeFile(path, into)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		keys := make([]string, len(extra))
		for i, k := range extra {
			keys[i] = k.String()
		}
		return fmt.Errorf("config: %s: unknown key %s", path, strings.Join(keys, ", "))
	}
	return nil
}

func (w Workspace) validate() error {
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
	if w.Agent.Model != "" {
		if err := group.ValidateAlias(w.Agent.Model); err != nil {
			return fmt.Errorf("[agent] model: %w", err)
		}
	}
	if w.Agent.TokenWindow < 0 {
		return fmt.Errorf("[agent] token_window %d must not be negative", w.Agent.TokenWindow)
	}
	return nil
}

// Merge is w with over laid on it table by table: a Network's rpc_urls, a
// provider's api_key or base_url, or an agent field that over sets wins, and
// everything over leaves unset comes from w. Neither input is changed.
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
	if over.Agent.Model != "" {
		out.Agent.Model = over.Agent.Model
	}
	if over.Agent.TokenWindow != 0 {
		out.Agent.TokenWindow = over.Agent.TokenWindow
	}
	return out
}
