// Package model reads the Models a Group's future agent may use off its COSL
// replica (docs/adr/0012-recap-on-the-log-pruning-off-it.md). As with
// Networks (docs/adr/0008-networks-on-cosl.md) there is no models table: the
// current set is the fold of model.create / update / remove rows by alias.
// Model identity replicates; credentials never do.
package model

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	coslv1 "defios.com/proto/cosl/v1"
)

var (
	// ErrInvalid is a Model config that fails validation.
	ErrInvalid = errors.New("model: invalid config")
	// ErrExists is an add of an alias that is already live.
	ErrExists = errors.New("model: alias already exists")
	// ErrUnknown is an edit, remove, or lookup of an alias that is not live.
	ErrUnknown = errors.New("model: no such model")
)

// Providers a Model config may name; each fixes the API shape an agent
// speaks to it.
const (
	Anthropic = "anthropic"
	OpenAI    = "openai"
)

// maxModelBytes bounds a provider's model id.
const maxModelBytes = 256

// Model is one live Model config in the folded state.
type Model struct {
	Alias    string
	ConfigID [32]byte // msg_id of the create/update row that defines this config
	Provider string
	ModelID  string // the provider's model id
	BaseURL  string // unset = the provider's
	APIPath  string // unset = the provider's
}

func fromConfig(c *coslv1.ModelConfig, id [32]byte) Model {
	return Model{ConfigID: id, Alias: c.Alias, Provider: c.Provider, ModelID: c.Model, BaseURL: c.BaseUrl, APIPath: c.ApiPath}
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks a config's alias, provider and model id, and that the
// optional endpoint is an http(s) URL and an absolute path. Neither may carry
// userinfo or a query: credentials never ride the log.
func Validate(c *coslv1.ModelConfig) error {
	if err := group.ValidateAlias(c.Alias); err != nil {
		return invalidf("alias %q must be 1–64 chars matching [a-z0-9][a-z0-9_-]*", c.Alias)
	}
	switch c.Provider {
	case Anthropic, OpenAI:
	default:
		return invalidf("provider %q must be anthropic or openai", c.Provider)
	}
	if !validModelID(c.Model) {
		return invalidf("model %q must be 1–%d bytes with no spaces or control characters", c.Model, maxModelBytes)
	}
	if c.BaseUrl != "" {
		u, err := url.Parse(c.BaseUrl)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return invalidf("base url %q must be an http or https URL with a host", c.BaseUrl)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return invalidf("base url %q must not carry userinfo, a query, or a fragment", c.BaseUrl)
		}
	}
	if c.ApiPath != "" {
		u, err := url.Parse(c.ApiPath)
		if err != nil || !strings.HasPrefix(c.ApiPath, "/") || u.Scheme != "" || u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
			return invalidf("api path %q must be an absolute path with no query or fragment", c.ApiPath)
		}
	}
	return nil
}

func validModelID(s string) bool {
	return s != "" && len(s) <= maxModelBytes &&
		strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) < 0
}

// MsgTypes are the rows Fold reads: a reader folding Models asks the replica
// for these only (cosl.Query.MsgTypes, ADR 0014).
var MsgTypes = []uint32{cosl.TypeModelCreate, cosl.TypeModelUpdate, cosl.TypeModelRemove}

// Fold replays rows in order into the live Models, sorted by alias. A
// message that does not apply to the state it meets is skipped, identically
// on every Node: a create of a live alias (the create race), an update of an
// absent alias, an update that changes the provider, a remove of an absent
// alias, and any invalid config. A removed alias may be created again.
func Fold(rows []cosl.Row) ([]Model, error) {
	live := map[string]Model{}
	for _, r := range rows {
		switch r.MsgType {
		case cosl.TypeModelCreate, cosl.TypeModelUpdate, cosl.TypeModelRemove:
		default:
			continue
		}
		env, err := r.Decode()
		if err != nil {
			return nil, err
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			return nil, fmt.Errorf("model: seq %d: %w", r.Seq, err)
		}
		switch p := msg.(type) {
		case *coslv1.ModelConfig:
			if Validate(p) != nil {
				continue
			}
			cur, exists := live[p.Alias]
			if (env.MsgType == cosl.TypeModelCreate && !exists) ||
				(env.MsgType == cosl.TypeModelUpdate && exists && cur.Provider == p.Provider) {
				live[p.Alias] = fromConfig(p, r.MsgID)
			}
		case *coslv1.ModelRemove:
			delete(live, p.Alias)
		}
	}
	out := make([]Model, 0, len(live))
	for _, m := range live {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}

// Get finds a live Model by alias.
func Get(models []Model, alias string) (Model, error) {
	for _, m := range models {
		if m.Alias == alias {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("%w: %q", ErrUnknown, alias)
}

// Check is the writer's precondition for authoring typ for cfg (or removing
// alias) against the live set; Fold would skip a message that fails it.
func Check(models []Model, typ uint32, cfg *coslv1.ModelConfig, alias string) error {
	cur, err := Get(models, alias)
	exists := err == nil
	switch typ {
	case cosl.TypeModelCreate:
		if exists {
			return fmt.Errorf("%w: %q", ErrExists, alias)
		}
	case cosl.TypeModelUpdate:
		if !exists {
			return err
		}
		if cur.Provider != cfg.Provider {
			return invalidf("a Model's provider cannot change (%s is %s); remove it and add it again", alias, cur.Provider)
		}
	case cosl.TypeModelRemove:
		if !exists {
			return err
		}
	}
	return nil
}

// Config is the wire form of m.
func (m Model) Config() *coslv1.ModelConfig {
	return &coslv1.ModelConfig{Alias: m.Alias, Provider: m.Provider, Model: m.ModelID, BaseUrl: m.BaseURL, ApiPath: m.APIPath}
}
