package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"defios.com/internal/agent"
	"defios.com/internal/config"
	"defios.com/internal/node"
	"defios.com/internal/provider"
)

// The Agents page: this Node's Agent Profiles, and the edit-target
// Group's Providers they run on (ADR 0029, 0022). Every change goes
// through internal/node; a refusal names the field it is about.

// AgentsNode is what the Agents page needs of a Node besides the rest of
// Node: AgentProfiles and Providers.
type AgentsNode interface {
	AgentProfile(ctx context.Context, handle string) (node.AgentProfile, error)
	AddAgentProfile(ctx context.Context, handle string, e node.ProfileEdit) (node.AgentProfile, error)
	EditAgentProfile(ctx context.Context, handle string, e node.ProfileEdit) (node.AgentProfile, error)
	DeleteAgentProfile(ctx context.Context, handle string) error
	AgentSystem(ctx context.Context, handle string) (string, error)
	SetAgentSystem(ctx context.Context, handle, prompt string) error
	Grant(ctx context.Context, handle, item string, secrets []string) (node.GrantKind, node.AgentProfile, error)
	Revoke(ctx context.Context, handle, item string) (node.GrantKind, node.AgentProfile, error)
	Plugins(ctx context.Context) ([]node.Plugin, error)
	Provider(ctx context.Context, group, conv, alias string) (node.Provider, error)
	AddProvider(ctx context.Context, group, conv, alias string, e node.ProviderEdit) (node.Provider, error)
	EditProvider(ctx context.Context, group, conv, alias string, e node.ProviderEdit) (node.Provider, error)
	RemoveProvider(ctx context.Context, group, conv, alias string) error
	Secrets(ctx context.Context, group string, passphrase node.Passphrase) ([]node.Secret, error)
}

// FieldBody is the body field of an Agent Profile or Provider, which the
// app refuses itself when it is not a JSON object.
const FieldBody = "body"

// AgentsView is the Agents page. Each part carries its own Problem, so
// one failing leaves the others shown.
type AgentsView struct {
	// Group is the edit-target Group, whose Providers show, and Served
	// whether it is served now: a change applies the next time it is.
	Group  string
	Served bool

	Profiles        []ProfileSummary
	ProfilesProblem *Problem
	// Open is the Agent Profile shown; nil for none.
	Open *ProfileView

	Providers        []ProviderView
	ProvidersProblem *Problem
	// Presets are what a new Provider may start from.
	Presets []string

	// Catalog is what a profile may be granted.
	Catalog        Catalog
	CatalogProblem *Problem

	// Problem is why the last change was refused, Where the form it is
	// about ("profile", "new-profile", "delete", "grant:<item>",
	// "provider:<alias>" or "new-provider") and Field the field (a
	// node.Field constant or FieldBody).
	Problem *Problem
	Where   string
	Field   string
	// Saved: the last change was written.
	Saved bool
}

// ProfileSummary is one Agent Profile in the list.
type ProfileSummary struct {
	Handle, Provider, Model string
}

// ProfileView is one Agent Profile as its form shows it.
type ProfileView struct {
	Handle, Provider, Model string
	// Body is the request body besides the model, as indented JSON; "" for
	// none. MaxTurns is "" when unset.
	Body, MaxTurns string
	// System is its SYSTEM.md; SystemProblem why it could not be read.
	System        string
	SystemProblem *Problem
	Tools, Skills []string
	// MCP is each MCP Server granted with the Secrets it gets.
	MCP        map[string][]string
	IncludedIn []PlaceView
}

// PlaceView is a place that includes an agent: Where is "Group talk" or
// the Conversation's Workspace name.
type PlaceView struct {
	Group, Where string
}

// ProviderView is one of the edit-target Group's Providers. APIKey is
// the name of the Secret its key reference names, "" for none; Body is
// indented JSON, "" for none.
type ProviderView struct {
	Alias, BaseURL, APIPath, Shape, URL string
	APIKey, Body                        string
}

// Catalog is what an Agent Profile may be granted: the built-in Tools,
// and the Skills and MCP Servers of the Plugins this Node has.
type Catalog struct {
	Tools   []string
	Skills  []CatalogSkill
	Servers []CatalogServer
}

// CatalogSkill is a Plugin's Skill, by its <plugin>/<skill> ref.
type CatalogSkill struct {
	Ref, Description string
}

// CatalogServer is a Plugin's MCP Server, by its <plugin>/<server> ref:
// the program it runs and the variables it asks for, each given by the
// Secret of that name.
type CatalogServer struct {
	Ref, CommandLine string
	Env              []string
}

// ProfileDraft is an Agent Profile's form as filled in. New adds one by
// Handle; else Handle names the one to edit. Body is a JSON object, ""
// for none; MaxTurns "" leaves it as it is.
type ProfileDraft struct {
	New                     bool
	Handle, Provider, Model string
	Body, MaxTurns, System  string
}

// ProviderDraft is a Provider's form as filled in. New adds one by Alias,
// from Preset when set; else Alias names the one to edit. APIKey is a
// Secret's name, "" for no key. Body is a JSON object, "" for none.
type ProviderDraft struct {
	New              bool
	Alias, Preset    string
	BaseURL, APIPath string
	APIKey, Body     string
}

// Agents is the Agents page with the Agent Profile open shown; "" shows
// none.
func (s *Shell) Agents(open string) AgentsView {
	v, ok := s.agentsTarget()
	if !ok {
		return v
	}
	s.fillAgents(s.node(), &v, open)
	return v
}

// SaveAgentProfile adds or edits the Agent Profile d is, and writes its
// SYSTEM.md. A refusal writes nothing.
func (s *Shell) SaveAgentProfile(d ProfileDraft) AgentsView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.agentsTarget()
	if !ok {
		return v
	}
	n := s.node()
	v.Where = "profile"
	if d.New {
		v.Where = "new-profile"
	}
	open := d.Handle
	err := s.saveProfile(n, d)
	if err != nil {
		v.Problem, v.Field = problemOf(err), fieldOf(err)
		if d.New {
			open = ""
			if _, made := n.AgentProfile(s.ctx, d.Handle); made == nil {
				// Added, with its SYSTEM.md not written: show it as it is.
				open, v.Where = d.Handle, "profile"
			}
		}
	} else {
		v.Saved = true
	}
	s.fillAgents(n, &v, open)
	return v
}

func (s *Shell) saveProfile(n Node, d ProfileDraft) error {
	var e node.ProfileEdit
	body, err := parseBody(d.Body)
	if err != nil {
		return err
	}
	if d.Model != "" {
		body["model"] = d.Model
	}
	if d.MaxTurns != "" {
		turns, err := strconv.Atoi(strings.TrimSpace(d.MaxTurns))
		if err != nil {
			return &node.InvalidError{Err: errors.New("max turns must be a whole number"), Field: node.FieldMaxTurns}
		}
		e.MaxTurns = &turns
	}
	if d.New {
		e.Provider = &d.Provider
		if len(body) > 0 {
			e.Body = body
		}
		if _, err := n.AddAgentProfile(s.ctx, d.Handle, e); err != nil {
			return err
		}
		if d.System == "" {
			return nil // the placeholder it was given
		}
		return n.SetAgentSystem(s.ctx, d.Handle, d.System)
	}
	cur, err := n.AgentProfile(s.ctx, d.Handle)
	if err != nil {
		return err
	}
	if d.Provider != cur.Provider {
		e.Provider = &d.Provider
	}
	if e.MaxTurns != nil && cur.MaxTurns > 0 && *e.MaxTurns == cur.MaxTurns {
		e.MaxTurns = nil
	}
	if !sameBody(body, cur.Body) {
		e.Body = body
	}
	if e.Provider != nil || e.MaxTurns != nil || e.Body != nil {
		if _, err := n.EditAgentProfile(s.ctx, d.Handle, e); err != nil {
			return err
		}
	}
	sys, err := n.AgentSystem(s.ctx, d.Handle)
	if err != nil || sys == d.System {
		return err
	}
	return n.SetAgentSystem(s.ctx, d.Handle, d.System)
}

// DeleteAgentProfile deletes handle's Agent Profile; refused, with where
// it is included, while anywhere includes it.
func (s *Shell) DeleteAgentProfile(handle string) AgentsView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.agentsTarget()
	if !ok {
		return v
	}
	n := s.node()
	v.Where = "delete"
	open := ""
	if err := n.DeleteAgentProfile(s.ctx, handle); err != nil {
		v.Problem, open = problemOf(err), handle
	} else {
		v.Saved = true
	}
	s.fillAgents(n, &v, open)
	return v
}

// GrantAgent grants handle a Tool, Skill or MCP Server, an MCP Server
// with secrets, the Secrets it gets; granting a granted MCP Server again
// sets its Secrets.
func (s *Shell) GrantAgent(handle, item string, secrets []string) AgentsView {
	if len(secrets) == 0 {
		secrets = nil // a Tool or Skill takes none
	}
	return s.grant(handle, item, func(n Node) error {
		_, _, err := n.Grant(s.ctx, handle, item, secrets)
		return err
	})
}

// RevokeAgent takes a Tool, Skill or MCP Server out of handle's grants.
func (s *Shell) RevokeAgent(handle, item string) AgentsView {
	return s.grant(handle, item, func(n Node) error {
		_, _, err := n.Revoke(s.ctx, handle, item)
		return err
	})
}

func (s *Shell) grant(handle, item string, change func(Node) error) AgentsView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.agentsTarget()
	if !ok {
		return v
	}
	n := s.node()
	v.Where = "grant:" + item
	if err := change(n); err != nil {
		v.Problem = problemOf(err)
	} else {
		v.Saved = true
	}
	s.fillAgents(n, &v, handle)
	return v
}

// SaveProvider adds or edits the edit-target Group's Provider d is.
func (s *Shell) SaveProvider(d ProviderDraft) AgentsView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.agentsTarget()
	if !ok {
		return v
	}
	n := s.node()
	v.Where = "provider:" + d.Alias
	if d.New {
		v.Where = "new-provider"
	}
	if err := s.saveProvider(n, v.Group, d); err != nil {
		v.Problem, v.Field = providerProblem(err), fieldOf(err)
	} else {
		v.Saved = true
	}
	s.fillAgents(n, &v, "")
	return v
}

func (s *Shell) saveProvider(n Node, group string, d ProviderDraft) error {
	body, err := parseBody(d.Body)
	if err != nil {
		return err
	}
	key := ""
	if d.APIKey != "" {
		key = "$" + d.APIKey
	}
	if d.New {
		e := node.ProviderEdit{Preset: d.Preset, APIKey: &key}
		if len(body) > 0 {
			e.Body = body
		}
		// A preset fills what is left empty.
		if d.BaseURL != "" || d.Preset == "" {
			e.BaseURL = &d.BaseURL
		}
		if d.APIPath != "" || d.Preset == "" {
			e.APIPath = &d.APIPath
		}
		_, err := n.AddProvider(s.ctx, group, node.GroupTalk, d.Alias, e)
		return err
	}
	cur, err := n.Provider(s.ctx, group, node.GroupTalk, d.Alias)
	if err != nil {
		return err
	}
	var e node.ProviderEdit
	changed := false
	if d.BaseURL != cur.BaseURL {
		e.BaseURL, changed = &d.BaseURL, true
	}
	if d.APIPath != cur.APIPath {
		e.APIPath, changed = &d.APIPath, true
	}
	if key != cur.APIKey {
		e.APIKey, changed = &key, true
	}
	if !sameBody(body, cur.Body) {
		e.Body, changed = body, true
	}
	if !changed {
		return nil
	}
	_, err = n.EditProvider(s.ctx, group, node.GroupTalk, d.Alias, e)
	return err
}

// RemoveProvider removes the edit-target Group's Provider alias.
func (s *Shell) RemoveProvider(alias string) AgentsView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.agentsTarget()
	if !ok {
		return v
	}
	n := s.node()
	v.Where = "provider:" + alias
	if err := n.RemoveProvider(s.ctx, v.Group, node.GroupTalk, alias); err != nil {
		v.Problem = problemOf(err)
	} else {
		v.Saved = true
	}
	s.fillAgents(n, &v, "")
	return v
}

// agentsTarget is the view's Group and whether it is served; ok false,
// with nothing to show, while no Group is served in this app.
func (s *Shell) agentsTarget() (AgentsView, bool) {
	st := s.State()
	v := AgentsView{Group: st.EditGroup, Served: st.EditGroupServed}
	return v, s.node() != nil && st.View == ViewServed && v.Group != ""
}

// fillAgents adds the profiles, open's, the Providers and the Catalog.
func (s *Shell) fillAgents(n Node, v *AgentsView, open string) {
	if list, err := n.AgentProfiles(s.ctx); err != nil {
		v.ProfilesProblem = problemOf(err)
	} else {
		v.Profiles = make([]ProfileSummary, len(list))
		for i, p := range list {
			v.Profiles[i] = ProfileSummary{Handle: p.Handle, Provider: p.Provider, Model: p.Model}
		}
	}
	if open != "" {
		if p, err := n.AgentProfile(s.ctx, open); err != nil {
			if v.Problem == nil {
				v.Problem, v.Where = problemOf(err), "profile"
			}
		} else {
			v.Open = profileView(p)
			if v.Open.System, err = n.AgentSystem(s.ctx, open); err != nil {
				v.Open.SystemProblem = problemOf(err)
			}
		}
	}
	if list, err := n.Providers(s.ctx, v.Group, node.GroupTalk); err != nil {
		v.ProvidersProblem = problemOf(err)
	} else {
		v.Providers = make([]ProviderView, len(list))
		for i, p := range list {
			v.Providers[i] = ProviderView{Alias: p.Alias, BaseURL: p.BaseURL, APIPath: p.APIPath, Shape: p.Shape, URL: p.URL,
				APIKey: authKeySecret(p.APIKey), Body: bodyText(p.Body)}
		}
	}
	v.Presets = node.PresetNames()
	v.Catalog.Tools = config.Tools
	if plugins, err := n.Plugins(s.ctx); err != nil {
		v.CatalogProblem = problemOf(err)
	} else {
		for _, p := range plugins {
			for _, sk := range p.Skills {
				v.Catalog.Skills = append(v.Catalog.Skills, CatalogSkill{Ref: p.Name + "/" + sk.Name, Description: sk.Description})
			}
			for _, sv := range p.Servers {
				v.Catalog.Servers = append(v.Catalog.Servers, CatalogServer{Ref: p.Name + "/" + sv.Name, CommandLine: sv.CommandLine, Env: sv.Env})
			}
		}
	}
}

func profileView(p node.AgentProfile) *ProfileView {
	body := map[string]any{}
	for k, val := range p.Body {
		if k != "model" {
			body[k] = val
		}
	}
	o := &ProfileView{Handle: p.Handle, Provider: p.Provider, Model: p.Model, Body: bodyText(body),
		Tools: p.Tools, Skills: p.Skills, MCP: map[string][]string{}, IncludedIn: []PlaceView{}}
	if p.MaxTurns > 0 {
		o.MaxTurns = strconv.Itoa(p.MaxTurns)
	}
	for ref, g := range p.MCP {
		o.MCP[ref] = g.Secrets
	}
	for _, pl := range p.IncludedIn {
		o.IncludedIn = append(o.IncludedIn, placeView(pl))
	}
	return o
}

func placeView(p node.Place) PlaceView {
	switch {
	case p.Conversation == node.GroupTalk:
		return PlaceView{Group: p.Group, Where: "Group talk"}
	case p.Workspace != "":
		return PlaceView{Group: p.Group, Where: p.Workspace}
	}
	return PlaceView{Group: p.Group, Where: "a conversation"}
}

func placesText(in []node.Place) string {
	names := make([]string, len(in))
	for i, p := range in {
		pv := placeView(p)
		names[i] = fmt.Sprintf("%s (%s)", pv.Group, pv.Where)
	}
	return strings.Join(names, ", ")
}

// parseBody reads a body field: a JSON object, "" for none. Numbers
// read as int64 where whole, as a config reads them back.
func parseBody(text string) (map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil || body == nil || dec.More() {
		return nil, &node.InvalidError{Err: errors.New("the body must be a JSON object, such as {\"max_tokens\": 1024}"), Field: FieldBody}
	}
	return numbers(body).(map[string]any), nil
}

func numbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = numbers(val)
		}
	case []any:
		for i, val := range x {
			x[i] = numbers(val)
		}
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	}
	return v
}

// sameBody: a and b are the same body, none being one empty.
func sameBody(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// bodyText is body as indented JSON, "" for none.
func bodyText(body map[string]any) string {
	if len(body) == 0 {
		return ""
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// fieldOf is the field a refusal names, "" for none.
func fieldOf(err error) string {
	if inv := (*node.InvalidError)(nil); errors.As(err, &inv) {
		return inv.Field
	}
	switch {
	case errors.Is(err, node.ErrProfileExists):
		return node.FieldHandle
	case errors.Is(err, provider.ErrExists):
		return node.FieldAlias
	}
	return ""
}

// providerProblem is problemOf for a Provider's form: a refused base
// URL, API path or name says what the field takes.
func providerProblem(err error) *Problem {
	switch fieldOf(err) {
	case node.FieldAlias:
		return &Problem{"invalid", "A Provider's alias is 1–64 lowercase letters, digits, - and _, starting with a letter or digit."}
	case node.FieldBaseURL:
		return &Problem{"invalid", "Enter an http:// or https:// URL with a host, and no user name, query or fragment."}
	case node.FieldAPIPath:
		return &Problem{"invalid", "Enter an absolute path ending in " + strings.Join(agent.ShapeSuffixes(), ", ") + ", with no query or fragment."}
	}
	return problemOf(err)
}

// agentsProblem maps the Agents page's errors: Agent Profiles, grants
// and Providers; nil for another error.
func agentsProblem(err error) *Problem {
	var included *node.ProfileIncludedError
	switch {
	case errors.As(err, &included):
		return &Problem{"profile-included", fmt.Sprintf("@%s can't be deleted while it is included in %s. Exclude it there first, on Talk.", included.Handle, placesText(included.In))}
	case errors.Is(err, node.ErrProfileExists):
		return &Problem{"profile-exists", "This device already has an agent by that Handle."}
	case errors.Is(err, config.ErrGrantRefused):
		// The detail is shown as the library words it, not read.
		return &Problem{"grant-refused", sentence(err.Error())}
	case errors.Is(err, config.ErrNotWritten):
		return &Problem{"not-written", "That would leave a config this device can't read, so nothing was written (" + err.Error() + ")."}
	case errors.Is(err, provider.ErrExists):
		return &Problem{"provider-exists", "This Group already has a Provider by that alias."}
	case errors.Is(err, provider.ErrUnknown):
		return &Problem{"unknown-provider", "This Group has no Provider by that alias."}
	case errors.Is(err, provider.ErrInvalid):
		return &Problem{"invalid-provider", "That Provider is not valid (" + err.Error() + ")."}
	}
	return nil
}
