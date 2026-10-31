package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/provider"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
)

// providerFlags are the fields add and edit take.
type providerFlags struct {
	baseURL string
	apiPath string
}

func (f *providerFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.baseURL, "base-url", "", "http(s) base URL, e.g. https://api.anthropic.com; replicated to every Node, so no credentials")
	cmd.Flags().StringVar(&f.apiPath, "api-path", "", "API path; its suffix picks the shape: .../messages, .../chat/completions or .../responses")
}

func providerCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "provider",
		Short: "Define the Providers this Group's agents may call",
		Long: `Providers live on the Group's COSL, identical on every Node, and are named by a local alias.
A Provider is an LLM API, named by a base URL and an API path whose suffix picks the API Shape
(/messages, /chat/completions or /responses). The model and every other request option are set per
agent, in [agents.<handle>.body]. Credentials never replicate: each Node names its key for a
Provider in its own config.toml, as [providers.<alias>] api_key = "$NAME", a Secret (see secret).`,
	}

	var add providerFlags
	var preset string
	addCmd := &cobra.Command{
		Use:   "add <alias>",
		Short: "Add a Provider (requires serve)",
		Long: `Add a Provider. An alias that names a preset uses it, as does --preset; --base-url and
--api-path override its fields. Without a preset both are required. Presets: ` + strings.Join(provider.PresetNames(), ", ") + ".",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := &coslv1.ProviderConfig{Alias: args[0]}
			name := preset
			if !cmd.Flags().Changed("preset") {
				name = args[0]
			}
			if p, ok := provider.Preset(name); ok {
				cfg.BaseUrl, cfg.ApiPath = p.BaseURL, p.APIPath
			} else if cmd.Flags().Changed("preset") {
				return exitUsagef("no preset %q; the presets are %s", preset, strings.Join(provider.PresetNames(), ", "))
			}
			if cmd.Flags().Changed("base-url") {
				cfg.BaseUrl = add.baseURL
			}
			if cmd.Flags().Changed("api-path") {
				cfg.ApiPath = add.apiPath
			}
			if err := provider.Validate(cfg); err != nil {
				return exitUsagef("%w", err)
			}
			v, err := serve.Client{Socket: g.paths().Socket}.ProviderAdd(cmd.Context(), cfg)
			return providerWritten(g, "added "+args[0], v, err)
		},
	}
	add.bind(addCmd)
	addCmd.Flags().StringVar(&preset, "preset", "", "start from a preset: "+strings.Join(provider.PresetNames(), ", "))

	var edit providerFlags
	editCmd := &cobra.Command{
		Use:   "edit <alias>",
		Short: "Change a Provider's base URL or API path; unspecified fields keep their value (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cur, err := currentProvider(g, args[0])
			if err != nil {
				return err
			}
			cfg := cur.Config()
			if cmd.Flags().Changed("base-url") {
				cfg.BaseUrl = edit.baseURL
			}
			if cmd.Flags().Changed("api-path") {
				cfg.ApiPath = edit.apiPath
			}
			if err := provider.Validate(cfg); err != nil {
				return exitUsagef("%w", err)
			}
			v, err := serve.Client{Socket: g.paths().Socket}.ProviderEdit(cmd.Context(), cfg)
			return providerWritten(g, "edited "+args[0], v, err)
		},
	}
	edit.bind(editCmd)

	removeCmd := &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a Provider (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := serve.Client{Socket: g.paths().Socket}.ProviderRemove(cmd.Context(), args[0])
			return providerWritten(g, "removed "+args[0], v, err)
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the Group's Providers: alias, URL",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runProviderList(g) },
	}
	bindGroupFlag(g, listCmd)
	showCmd := &cobra.Command{
		Use:   "show <alias>",
		Short: "Show one Provider",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runProviderShow(g, args[0]) },
	}
	bindGroupFlag(g, showCmd)

	root.AddCommand(addCmd, listCmd, showCmd, editCmd, removeCmd)
	return root
}

func providerWritten(g *globals, what string, v serve.SendView, err error) error {
	if err != nil {
		switch {
		case errors.Is(err, provider.ErrInvalid), errors.Is(err, provider.ErrExists), errors.Is(err, provider.ErrUnknown):
			return exitUsagef("%w", err)
		}
		return sendError(err)
	}
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "%s as seq %d (msg_id %s)\n", what, v.Seq, v.MsgID)
	return nil
}

// providerView is one Provider, human or JSON.
type providerView struct {
	Alias   string `json:"alias"`
	BaseURL string `json:"base_url"`
	APIPath string `json:"api_path"`
	Shape   string `json:"shape"`
}

// foldProviders reads the folded Providers of the targeted Group.
func foldProviders(g *globals) ([]provider.Provider, error) {
	store, err := openReplica(g)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	providers, err := provider.Read(store)
	if err != nil {
		return nil, exitOperational(err)
	}
	return providers, nil
}

// currentProvider is the live log-state config of alias, for edit.
func currentProvider(g *globals, alias string) (provider.Provider, error) {
	providers, err := foldProviders(g)
	if err != nil {
		return provider.Provider{}, err
	}
	p, err := provider.Get(providers, alias)
	if err != nil {
		return provider.Provider{}, exitUsagef("%w", err)
	}
	return p, nil
}

func providerViewOf(p provider.Provider) providerView {
	return providerView{Alias: p.Alias, BaseURL: p.BaseURL, APIPath: p.APIPath, Shape: p.Shape().Name}
}

func runProviderList(g *globals) error {
	providers, err := foldProviders(g)
	if err != nil {
		return err
	}
	views := make([]providerView, len(providers))
	for i, p := range providers {
		views[i] = providerViewOf(p)
	}
	if g.json {
		return g.emitJSON(views)
	}
	out := g.out()
	for i, v := range views {
		fmt.Fprintf(g.app.Stdout, "%s  %s  %s\n", out.primary(v.Alias), providers[i].URL(), v.Shape)
	}
	return nil
}

func runProviderShow(g *globals, alias string) error {
	p, err := currentProvider(g, alias)
	if err != nil {
		return err
	}
	v := providerViewOf(p)
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "alias %s\nbase_url %s\napi_path %s\nshape %s\n", g.out().primary(v.Alias), v.BaseURL, v.APIPath, v.Shape)
	return nil
}
