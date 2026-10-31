package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/provider"
)

// providerFlags are the fields add and edit take.
type providerFlags struct {
	baseURL string
	apiPath string
}

func (f *providerFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.baseURL, "base-url", "", "http(s) base URL, e.g. https://api.anthropic.com, with no userinfo, query or fragment")
	cmd.Flags().StringVar(&f.apiPath, "api-path", "", "API path; its suffix picks the shape: .../messages, .../chat/completions or .../responses")
}

func providerCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "provider",
		Short: "Configure the Providers this Node's agents may call",
		Long: `A Provider is this Node's own config, never shared with the Group: a [providers.<alias>] table in
the Group's config.toml, which a Conversation's config.toml may override key by key. A Provider is an
LLM API, named by a base URL and an API path whose suffix picks the API Shape (/messages,
/chat/completions or /responses). Its api_key is a Secret reference (see secret), sent only to its
base URL, and its body is the base request for every agent on it. The model and every other request
option are set there or per agent, in [agents.<handle>.body]. These commands need no serve.`,
	}

	var add providerFlags
	var preset string
	addCmd := &cobra.Command{
		Use:   "add <alias>",
		Short: "Add a Provider to the Group's config.toml",
		Long: `Add a Provider. An alias that names a preset uses it, as does --preset; --base-url and
--api-path override its fields. Without a preset both are required. Presets: ` + strings.Join(provider.PresetNames(), ", ") + ".",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			alias := args[0]
			var p config.Provider
			name := preset
			if !cmd.Flags().Changed("preset") {
				name = alias
			}
			if pre, ok := provider.Preset(name); ok {
				p.BaseURL, p.APIPath = pre.BaseURL, pre.APIPath
			} else if cmd.Flags().Changed("preset") {
				return exitUsagef("no preset %q; the presets are %s", preset, strings.Join(provider.PresetNames(), ", "))
			}
			if cmd.Flags().Changed("base-url") {
				p.BaseURL = add.baseURL
			}
			if cmd.Flags().Changed("api-path") {
				p.APIPath = add.apiPath
			}
			path, err := writeProviders(g, func(ps map[string]config.Provider) error {
				if _, ok := ps[alias]; ok {
					return fmt.Errorf("%w: %q", provider.ErrExists, alias)
				}
				ps[alias] = p
				return nil
			})
			if err != nil {
				return err
			}
			return providerWritten(g, providerViewOf(alias, p), "added "+alias+" to "+path)
		},
	}
	add.bind(addCmd)
	addCmd.Flags().StringVar(&preset, "preset", "", "start from a preset: "+strings.Join(provider.PresetNames(), ", "))
	bindWriteGroupFlag(g, addCmd)

	var edit providerFlags
	editCmd := &cobra.Command{
		Use:   "edit <alias>",
		Short: "Change a Provider's base URL or API path; unspecified fields keep their value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			alias := args[0]
			var p config.Provider
			path, err := writeProviders(g, func(ps map[string]config.Provider) error {
				cur, ok := ps[alias]
				if !ok {
					return fmt.Errorf("%w: %q", provider.ErrUnknown, alias)
				}
				p = cur
				if cmd.Flags().Changed("base-url") {
					p.BaseURL = edit.baseURL
				}
				if cmd.Flags().Changed("api-path") {
					p.APIPath = edit.apiPath
				}
				if reflect.DeepEqual(p, cur) {
					return fmt.Errorf("%w: %q", provider.ErrUnchanged, alias)
				}
				ps[alias] = p
				return nil
			})
			if err != nil {
				return err
			}
			return providerWritten(g, providerViewOf(alias, p), "edited "+alias+" in "+path)
		},
	}
	edit.bind(editCmd)
	bindWriteGroupFlag(g, editCmd)

	removeCmd := &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a Provider's table, its key reference and body with it",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			alias := args[0]
			path, err := writeProviders(g, func(ps map[string]config.Provider) error {
				if _, ok := ps[alias]; !ok {
					return fmt.Errorf("%w: %q", provider.ErrUnknown, alias)
				}
				delete(ps, alias)
				return nil
			})
			if err != nil {
				return err
			}
			return providerWritten(g, map[string]string{"alias": alias}, "removed "+alias+" from "+path)
		},
	}
	bindWriteGroupFlag(g, removeCmd)

	var keyQuery string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the Providers: alias, URL, shape",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runProviderList(g, keyQuery) },
	}
	bindGroupFlag(g, listCmd)
	listCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)
	showCmd := &cobra.Command{
		Use:   "show <alias>",
		Short: "Show one Provider",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runProviderShow(g, keyQuery, args[0]) },
	}
	bindGroupFlag(g, showCmd)
	showCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)

	root.AddCommand(addCmd, listCmd, showCmd, editCmd, removeCmd)
	return root
}

// writeProviders applies change to the Providers of the targeted Group's
// config.toml and writes it back if every Provider is then valid; it
// returns the file's path.
func writeProviders(g *globals, change func(map[string]config.Provider) error) (string, error) {
	return writeGroupConfig(g, func(ws *config.Workspace) error {
		if err := change(ws.Providers); err != nil {
			return err
		}
		for a, p := range ws.Providers {
			if err := provider.Validate(p.Endpoint(a)); err != nil {
				return fmt.Errorf("[providers.%s]: %w", a, err)
			}
		}
		return nil
	})
}

func providerWritten(g *globals, v any, what string) error {
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintln(g.app.Stdout, what)
	return nil
}

// providerView is one Provider, human or JSON. Its api_key is the Secret
// reference the config holds, never the Secret.
type providerView struct {
	Alias   string         `json:"alias"`
	BaseURL string         `json:"base_url"`
	APIPath string         `json:"api_path"`
	Shape   string         `json:"shape"`
	APIKey  string         `json:"api_key,omitempty"`
	Body    map[string]any `json:"body,omitempty"`
	url     string
}

func providerViewOf(alias string, p config.Provider) providerView {
	at := p.Endpoint(alias)
	return providerView{Alias: alias, BaseURL: p.BaseURL, APIPath: p.APIPath, Shape: at.Shape().Name,
		APIKey: p.APIKey, Body: p.Body, url: at.URL()}
}

func runProviderList(g *globals, keyQuery string) error {
	ws, err := readWorkspace(g, keyQuery)
	if err != nil {
		return err
	}
	aliases := make([]string, 0, len(ws.Providers))
	for a := range ws.Providers {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	views := make([]providerView, len(aliases))
	for i, a := range aliases {
		p, err := ws.Provider(a)
		if err != nil {
			return exitOperational(err)
		}
		views[i] = providerViewOf(a, p)
	}
	if g.json {
		return g.emitJSON(views)
	}
	out := g.out()
	for _, v := range views {
		fmt.Fprintf(g.app.Stdout, "%s  %s  %s\n", out.primary(v.Alias), v.url, v.Shape)
	}
	return nil
}

func runProviderShow(g *globals, keyQuery, alias string) error {
	ws, err := readWorkspace(g, keyQuery)
	if err != nil {
		return err
	}
	p, err := ws.Provider(alias)
	if errors.Is(err, provider.ErrUnknown) {
		return exitUsagef("%w", err)
	} else if err != nil {
		return exitOperational(err)
	}
	v := providerViewOf(alias, p)
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "alias %s\nbase_url %s\napi_path %s\nshape %s\n", g.out().primary(v.Alias), v.BaseURL, v.APIPath, v.Shape)
	if v.APIKey != "" {
		fmt.Fprintf(g.app.Stdout, "api_key %s\n", v.APIKey)
	}
	if v.Body != nil {
		body, err := json.Marshal(v.Body)
		if err != nil {
			return exitOperational(err)
		}
		fmt.Fprintf(g.app.Stdout, "body %s\n", body)
	}
	return nil
}
