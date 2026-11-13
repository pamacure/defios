package node

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/provider"
	"defios.com/internal/secret"
)

// Provider failures a front end tells apart, besides InvalidError (a
// preset that is none, or an API key that is no Secret reference) and
// config.ErrNotWritten: provider.ErrExists, provider.ErrUnknown,
// provider.ErrUnchanged and provider.ErrInvalid (a Provider left with no
// base URL or an API path no API Shape matches).

// The fields of a Provider, as an InvalidError from AddProvider or
// EditProvider names them.
const (
	FieldAlias   = "alias"
	FieldPreset  = "preset"
	FieldBaseURL = "base_url"
	FieldAPIPath = "api_path"
	FieldAPIKey  = "api_key"
)

// Provider is one Provider in force in Group-level talk or a
// Conversation (ADR 0022). Its APIKey is the Secret reference the config
// holds, never the Secret.
type Provider struct {
	Alias   string
	BaseURL string
	APIPath string
	Shape   string // the API Shape its api_path picks
	URL     string // where it is called: base URL and API path
	APIKey  string
	Body    map[string]any // nil if unset
}

// ProviderEdit is what AddProvider sets, or EditProvider changes: a nil
// field is left as it is. Body replaces the body whole, and an empty one
// clears it; an APIKey of "" removes the key reference. In a Conversation
// they set its override of the Group's Provider, and clearing one there
// leaves the Group's in force.
type ProviderEdit struct {
	// Preset, on AddProvider, is the preset to start from; "" is the
	// preset the alias names, if one does. The other fields override it.
	Preset           string
	BaseURL, APIPath *string
	APIKey           *string // a "$NAME" Secret reference
	Body             map[string]any
}

// apply sets e on p.
func (e ProviderEdit) apply(p *config.Provider) error {
	if e.BaseURL != nil {
		p.BaseURL = *e.BaseURL
	}
	if e.APIPath != nil {
		p.APIPath = *e.APIPath
	}
	if e.APIKey != nil {
		if *e.APIKey != "" {
			if _, err := secret.ParseRef(*e.APIKey); err != nil {
				return &InvalidError{Err: fmt.Errorf("API key: %w", err), Field: FieldAPIKey}
			}
		}
		p.APIKey = *e.APIKey
	}
	if e.Body != nil {
		// An empty body is no body, as the file reads back, so clearing
		// one already clear is unchanged.
		p.Body = e.Body
		if len(e.Body) == 0 {
			p.Body = nil
		}
	}
	return nil
}

// PresetNames are the presets AddProvider may start from.
func PresetNames() []string { return provider.PresetNames() }

// Providers lists the Providers in force in group's Group-level talk, or
// in Conversation conv with its overrides merged over the Group's, by
// alias. The Group need not be served.
func (h *Host) Providers(ctx context.Context, group, conv string) ([]Provider, error) {
	e, err := h.loadConfig(group, conv)
	if err != nil {
		return nil, err
	}
	ws := e.Workspace
	aliases := make([]string, 0, len(ws.Providers))
	for a := range ws.Providers {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	out := make([]Provider, 0, len(aliases))
	for _, a := range aliases {
		p, err := ws.Provider(a)
		if err != nil {
			return nil, err
		}
		out = append(out, providerOf(a, p))
	}
	return out, nil
}

// Provider is the Provider alias in force in group's Group-level talk or
// Conversation conv: provider.ErrUnknown if none is.
func (h *Host) Provider(ctx context.Context, group, conv, alias string) (Provider, error) {
	e, err := h.loadConfig(group, conv)
	if err != nil {
		return Provider{}, err
	}
	p, err := e.Workspace.Provider(alias)
	if err != nil {
		return Provider{}, err
	}
	return providerOf(alias, p), nil
}

// AddProvider adds a Provider to group's config, or to Conversation
// conv's, where it is that Conversation's alone: one in force there
// already is provider.ErrExists. Without a preset, its base URL and API
// path are required.
func (h *Host) AddProvider(ctx context.Context, group, conv, alias string, e ProviderEdit) (Provider, error) {
	var p config.Provider
	name := e.Preset
	if name == "" {
		name = alias
	}
	if pre, ok := provider.Preset(name); ok {
		p.BaseURL, p.APIPath = pre.BaseURL, pre.APIPath
	} else if e.Preset != "" {
		return Provider{}, onField(FieldPreset, invalidf("no preset %q; the presets are %s", e.Preset, strings.Join(provider.PresetNames(), ", ")))
	}
	if err := e.apply(&p); err != nil {
		return Provider{}, err
	}
	err := h.writeProviders(group, conv, alias, func(own, inForce map[string]config.Provider) error {
		if _, ok := inForce[alias]; ok {
			return fmt.Errorf("%w: %q", provider.ErrExists, alias)
		}
		own[alias] = p
		return nil
	})
	if err != nil {
		return Provider{}, err
	}
	return h.Provider(ctx, group, conv, alias)
}

// EditProvider changes the fields of Provider alias e sets: in group's
// config, or in Conversation conv's override of a Provider in force there.
func (h *Host) EditProvider(ctx context.Context, group, conv, alias string, e ProviderEdit) (Provider, error) {
	err := h.writeProviders(group, conv, alias, func(own, inForce map[string]config.Provider) error {
		if _, ok := inForce[alias]; !ok {
			return fmt.Errorf("%w: %q", provider.ErrUnknown, alias)
		}
		cur := own[alias]
		p := cur
		if err := e.apply(&p); err != nil {
			return err
		}
		if reflect.DeepEqual(p, cur) {
			return fmt.Errorf("%w: %q", provider.ErrUnchanged, alias)
		}
		own[alias] = p
		return nil
	})
	if err != nil {
		return Provider{}, err
	}
	return h.Provider(ctx, group, conv, alias)
}

// RemoveProvider removes Provider alias's table, its key reference and
// body with it: group's, or Conversation conv's override, which leaves the
// Group's in force there.
func (h *Host) RemoveProvider(ctx context.Context, group, conv, alias string) error {
	return h.writeProviders(group, conv, alias, func(own, _ map[string]config.Provider) error {
		if _, ok := own[alias]; !ok {
			return fmt.Errorf("%w: %q", provider.ErrUnknown, alias)
		}
		delete(own, alias)
		return nil
	})
}

// writeProviders applies change to the Providers of group's config.toml,
// or of Conversation conv's, and writes it back if every Provider in force
// there is then valid. change is given the file's own Providers, to
// change, and those in force before it. A refusal of Provider alias's own
// table is an InvalidError naming the field it is about.
func (h *Host) writeProviders(group, conv, alias string, change func(own, inForce map[string]config.Provider) error) error {
	if conv == GroupTalk {
		return h.writeGroupConfig(group, func(c *config.Config) error {
			if err := change(c.Providers, maps.Clone(c.Providers)); err != nil {
				return err
			}
			for a, p := range c.Providers {
				if err := provider.Validate(p.Endpoint(a)); err != nil {
					return providerRefused(a, alias, p, true, err)
				}
			}
			return nil
		})
	}
	e, err := h.loadConfig(group, conv)
	if err != nil {
		return err
	}
	return writeConversationConfig(e, func(w *config.Workspace) error {
		if w.Providers == nil {
			w.Providers = map[string]config.Provider{}
		}
		if err := change(w.Providers, maps.Clone(e.Workspace.Providers)); err != nil {
			return err
		}
		for a, p := range w.Providers {
			if err := provider.ValidateOverride(p.Endpoint(a)); err != nil {
				return providerRefused(a, alias, p, false, err)
			}
		}
		merged, err := e.Group.Workspace.Merge(*w)
		if err != nil {
			return err
		}
		for a := range merged.Providers {
			if _, err := merged.Provider(a); err != nil {
				return err
			}
		}
		if len(w.Providers) == 0 {
			w.Providers = nil
		}
		return nil
	})
}

// providerRefused is err, Provider a's table refused: an InvalidError
// naming the field first refused when a is the alias edited, whose
// message tells every problem. complete is whether base_url and api_path
// are required, as in a Group's table.
func providerRefused(a, edited string, p config.Provider, complete bool, err error) error {
	err = fmt.Errorf("[providers.%s]: %w", a, err)
	if a != edited {
		return err
	}
	field := FieldAlias // Validate tells of the alias alone when it is wrong
	switch {
	case groupAliasValid(a) && (complete || p.BaseURL != "") && provider.CheckBaseURL(p.BaseURL) != nil:
		field = FieldBaseURL
	case groupAliasValid(a) && (complete || p.APIPath != "") && provider.CheckAPIPath(p.APIPath) != nil:
		field = FieldAPIPath
	}
	return &InvalidError{Err: err, Field: field}
}

func providerOf(alias string, p config.Provider) Provider {
	at := p.Endpoint(alias)
	return Provider{Alias: alias, BaseURL: p.BaseURL, APIPath: p.APIPath, Shape: at.Shape().Name, URL: at.URL(),
		APIKey: p.APIKey, Body: p.Body}
}

// groupAliasValid: a is a valid alias, as a Provider's must be.
func groupAliasValid(a string) bool { return group.ValidateAlias(a) == nil }
