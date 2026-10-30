// Package provider reads the Providers a Group's agents may call off its
// COSL replica (docs/adr/0019-providers-replace-models-on-the-cosl.md). As
// with Networks (docs/adr/0008-networks-on-cosl.md) there is no providers
// table: the current set is the fold of provider.create / update / remove
// rows by alias. A Provider is an endpoint and nothing more; the suffix of
// its api_path picks the API shape. Credentials never replicate.
package provider

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"defios.com/internal/agent"
	"defios.com/internal/cosl"
	"defios.com/internal/group"
	coslv1 "defios.com/proto/cosl/v1"
)

var (
	// ErrInvalid is a Provider config that fails validation.
	ErrInvalid = errors.New("provider: invalid config")
	// ErrExists is an add of an alias that is already live.
	ErrExists = errors.New("provider: alias already exists")
	// ErrUnknown is an edit, remove, or lookup of an alias that is not live.
	ErrUnknown = errors.New("provider: no such provider")
)

// Provider is one live Provider config in the folded state.
type Provider struct {
	Alias    string
	ConfigID [32]byte // msg_id of the create/update row that defines this config
	BaseURL  string
	APIPath  string
}

// Shape is the API shape p's api_path picks. A folded Provider always has
// one.
func (p Provider) Shape() agent.Shape {
	s, _ := agent.ShapeOf(p.APIPath)
	return s
}

// URL is where p is called: its base_url followed by its api_path.
func (p Provider) URL() string { return strings.TrimRight(p.BaseURL, "/") + p.APIPath }

// Config is the wire form of p.
func (p Provider) Config() *coslv1.ProviderConfig {
	return &coslv1.ProviderConfig{Alias: p.Alias, BaseUrl: p.BaseURL, ApiPath: p.APIPath}
}

func fromConfig(c *coslv1.ProviderConfig, id [32]byte) Provider {
	return Provider{ConfigID: id, Alias: c.Alias, BaseURL: c.BaseUrl, APIPath: c.ApiPath}
}

// presets are the complete rows `provider add` may start from.
var presets = map[string]Provider{
	"anthropic": {Alias: "anthropic", BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages"},
	"openai":    {Alias: "openai", BaseURL: "https://api.openai.com", APIPath: "/v1/responses"},
}

// Preset is the preset named name.
func Preset(name string) (Provider, bool) {
	p, ok := presets[name]
	return p, ok
}

// PresetNames lists the presets, sorted.
func PresetNames() []string {
	out := make([]string, 0, len(presets))
	for n := range presets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks a config's alias; that base_url is an http(s) URL with a
// host and no userinfo, query or fragment, so credentials never ride the
// log; and that api_path is an absolute path with no query or fragment
// ending in a shape's suffix. Both are required.
func Validate(c *coslv1.ProviderConfig) error {
	if err := group.ValidateAlias(c.Alias); err != nil {
		return invalidf("alias %q must be 1–64 chars matching [a-z0-9][a-z0-9_-]*", c.Alias)
	}
	if c.BaseUrl == "" {
		return invalidf("base url is required")
	}
	u, err := url.Parse(c.BaseUrl)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return invalidf("base url %q must be an http or https URL with a host", c.BaseUrl)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(c.BaseUrl, "#") {
		return invalidf("base url %q must not carry userinfo, a query, or a fragment", c.BaseUrl)
	}
	if c.ApiPath == "" {
		return invalidf("api path is required")
	}
	p, err := url.Parse(c.ApiPath)
	if err != nil || !strings.HasPrefix(c.ApiPath, "/") || strings.HasPrefix(c.ApiPath, "//") || p.Scheme != "" || p.Host != "" ||
		strings.ContainsAny(c.ApiPath, "?#") {
		return invalidf("api path %q must be an absolute path with no query or fragment", c.ApiPath)
	}
	if _, ok := agent.ShapeOf(c.ApiPath); !ok {
		return invalidf("api path %q must end in one of %s", c.ApiPath, strings.Join(agent.ShapeSuffixes(), ", "))
	}
	return nil
}

// MsgTypes are the rows Fold reads: a reader folding Providers asks the
// replica for these only (cosl.Query.MsgTypes, ADR 0014).
var MsgTypes = []uint32{cosl.TypeProviderCreate, cosl.TypeProviderUpdate, cosl.TypeProviderRemove}

// Fold replays rows in order into the live Providers, sorted by alias. A
// message that does not apply to the state it meets is skipped, identically
// on every Node: a create of a live alias (the create race), an update of an
// absent alias, a remove of an absent alias, and any invalid config, such
// as an api_path no shape matches. A removed alias may be created again.
func Fold(rows []cosl.Row) ([]Provider, error) {
	live := map[string]Provider{}
	for _, r := range rows {
		switch r.MsgType {
		case cosl.TypeProviderCreate, cosl.TypeProviderUpdate, cosl.TypeProviderRemove:
		default:
			continue
		}
		env, err := r.Decode()
		if err != nil {
			return nil, err
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			return nil, fmt.Errorf("provider: seq %d: %w", r.Seq, err)
		}
		switch p := msg.(type) {
		case *coslv1.ProviderConfig:
			if Validate(p) != nil {
				continue
			}
			_, exists := live[p.Alias]
			if (env.MsgType == cosl.TypeProviderCreate && !exists) || (env.MsgType == cosl.TypeProviderUpdate && exists) {
				live[p.Alias] = fromConfig(p, r.MsgID)
			}
		case *coslv1.ProviderRemove:
			delete(live, p.Alias)
		}
	}
	out := make([]Provider, 0, len(live))
	for _, p := range live {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}

// Get finds a live Provider by alias.
func Get(providers []Provider, alias string) (Provider, error) {
	for _, p := range providers {
		if p.Alias == alias {
			return p, nil
		}
	}
	return Provider{}, fmt.Errorf("%w: %q", ErrUnknown, alias)
}

// Check is the writer's precondition for authoring typ for alias against
// the live set; Fold would skip a message that fails it.
func Check(providers []Provider, typ uint32, alias string) error {
	_, err := Get(providers, alias)
	exists := err == nil
	switch typ {
	case cosl.TypeProviderCreate:
		if exists {
			return fmt.Errorf("%w: %q", ErrExists, alias)
		}
	case cosl.TypeProviderUpdate, cosl.TypeProviderRemove:
		if !exists {
			return err
		}
	}
	return nil
}
