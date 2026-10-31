// Package provider holds the rules for a Provider, an LLM API a Node's
// agents may call. A Provider is wholly this Node's config, the
// [providers.<alias>] table of a Workspace's config.toml
// (docs/adr/0022-members-are-trusted-for-their-own-messages.md): it is never
// on the COSL, so its key only ever goes to an endpoint this Node
// configured. A Provider is a base URL and an API path; the suffix of its
// api_path picks the API Shape (docs/adr/0019-providers-replace-models-on-the-cosl.md).
package provider

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"defios.com/internal/agent"
	"defios.com/internal/group"
)

var (
	// ErrInvalid is a Provider config that fails validation.
	ErrInvalid = errors.New("provider: invalid config")
	// ErrExists is an add of an alias that is already configured.
	ErrExists = errors.New("provider: alias already exists")
	// ErrUnknown is an edit, remove, or lookup of an alias that is not
	// configured.
	ErrUnknown = errors.New("provider: no such provider")
	// ErrUnchanged is an edit that would leave the config as it is.
	ErrUnchanged = errors.New("provider: nothing to change")
)

// Provider is where a Provider is called: its alias, base_url and api_path.
type Provider struct {
	Alias   string
	BaseURL string
	APIPath string
}

// Shape is the API shape p's api_path picks. A valid Provider always has
// one.
func (p Provider) Shape() agent.Shape {
	s, _ := agent.ShapeOf(p.APIPath)
	return s
}

// URL is where p is called: its base_url followed by its api_path.
func (p Provider) URL() string { return strings.TrimRight(p.BaseURL, "/") + p.APIPath }

// presets are the complete Providers `provider add` may start from.
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

// Validate checks p's alias; that base_url is an http(s) URL with a host
// and no userinfo, query or fragment, so no credential hides in it; and
// that api_path is an absolute path with no query or fragment ending in a
// shape's suffix. Both are required, and a Provider wrong in both is told
// of both.
func Validate(p Provider) error { return validate(p, true) }

// ValidateOverride is Validate for a Conversation's table, which overrides
// the Group's key by key: a base_url or api_path it leaves out is not
// checked here, but in the merged Provider.
func ValidateOverride(p Provider) error { return validate(p, false) }

func validate(p Provider, complete bool) error {
	if err := group.ValidateAlias(p.Alias); err != nil {
		return invalidf("alias %q must be 1–64 chars matching [a-z0-9][a-z0-9_-]*", p.Alias)
	}
	var problems []string
	if p.BaseURL != "" || complete {
		if s := baseURLProblem(p.BaseURL); s != "" {
			problems = append(problems, s)
		}
	}
	if p.APIPath != "" || complete {
		if s := apiPathProblem(p.APIPath); s != "" {
			problems = append(problems, s)
		}
	}
	if len(problems) > 0 {
		return invalidf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// baseURLProblem is what is wrong with base, or "".
func baseURLProblem(base string) string {
	if base == "" {
		return "base url is required"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Sprintf("base url %q must be an http or https URL with a host", base)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(base, "#") {
		// Not the URL itself: what it carries may be a credential.
		return fmt.Sprintf("base url for %s must not carry userinfo, a query, or a fragment", u.Host)
	}
	return ""
}

// apiPathProblem is what is wrong with path, or "".
func apiPathProblem(path string) string {
	if path == "" {
		return "api path is required"
	}
	if strings.ContainsAny(path, "?#") {
		// Not the path itself: what it carries may be a credential.
		return "api path must have no query or fragment"
	}
	p, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || p.Scheme != "" || p.Host != "" {
		return fmt.Sprintf("api path %q must be an absolute path", path)
	}
	if _, ok := agent.ShapeOf(path); !ok {
		return fmt.Sprintf("api path %q must end in one of %s", path, strings.Join(agent.ShapeSuffixes(), ", "))
	}
	return ""
}
