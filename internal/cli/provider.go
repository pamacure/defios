package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/node"
	"defios.com/internal/provider"
	"defios.com/internal/secret"
)

// providerFlags are the fields add and edit take.
type providerFlags struct {
	baseURL string
	apiPath string
	apiKey  string
	body    string
}

func (f *providerFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.baseURL, "base-url", "", "http(s) base URL, e.g. https://api.anthropic.com, with no userinfo, query or fragment")
	cmd.Flags().StringVar(&f.apiPath, "api-path", "", "API path; its suffix picks the shape: .../messages, .../chat/completions or .../responses")
	cmd.Flags().StringVar(&f.apiKey, "api-key", "", `the Secret that holds its key, as a "$NAME" reference (see secret), never the key itself`)
	cmd.Flags().StringVar(&f.body, "body", "", "the base request body for every agent on it, a JSON object: inline, @path to read a file, or - for stdin")
}

// edit is the node.ProviderEdit of each field whose flag cmd was given:
// --body replaces the body whole, and --api-key must be a Secret
// reference.
func (f *providerFlags) edit(g *globals, cmd *cobra.Command) (node.ProviderEdit, error) {
	var e node.ProviderEdit
	fl := cmd.Flags()
	if fl.Changed("base-url") {
		e.BaseURL = &f.baseURL
	}
	if fl.Changed("api-path") {
		e.APIPath = &f.apiPath
	}
	if fl.Changed("api-key") {
		if f.apiKey != "" {
			if _, err := secret.ParseRef(f.apiKey); err != nil {
				return e, exitUsagef("--api-key: %w", err)
			}
		}
		e.APIKey = &f.apiKey
	}
	if fl.Changed("body") {
		body, err := readBody(g, f.body)
		if err != nil {
			return e, exitUsagef("%w", err)
		}
		e.Body = body
	}
	return e, nil
}

// providerError maps a failure of the library's Provider operations: what
// is refused is a usage error, the rest as talk's.
func providerError(err error) error {
	var invalid *node.InvalidError
	switch {
	case errors.Is(err, provider.ErrExists), errors.Is(err, provider.ErrUnknown), errors.Is(err, provider.ErrUnchanged),
		errors.Is(err, provider.ErrInvalid), errors.As(err, &invalid), errors.Is(err, config.ErrNotWritten):
		return exitUsagef("%w", err)
	}
	return talkError(err)
}

// providerReadError maps a failure of the library's Provider reads: an
// alias in force nowhere is a usage error, as is a Key query naming none;
// a config that does not read or validate is operational.
func providerReadError(err error) error {
	var invalid *node.InvalidError
	switch {
	case errors.Is(err, provider.ErrUnknown), errors.As(err, &invalid):
		return exitUsagef("%w", err)
	case errors.Is(err, provider.ErrInvalid):
		return exitOperational(err)
	}
	return talkError(err)
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
option are set there or per agent, in the [body] of its agents/<handle>/config.toml. These commands need no serve.`,
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
			e, err := add.edit(g, cmd)
			if err != nil {
				return err
			}
			e.Preset = preset
			if cmd.Flags().Changed("preset") && preset == "" {
				return exitUsagef("no preset %q; the presets are %s", preset, strings.Join(node.PresetNames(), ", "))
			}
			return writeProvider(g, "added", "to", args[0], func(h *node.Host, group string) (any, error) {
				p, err := h.AddProvider(cmd.Context(), group, node.GroupTalk, args[0], e)
				return providerViewOf(p), err
			})
		},
	}
	add.bind(addCmd)
	addCmd.Flags().StringVar(&preset, "preset", "", "start from a preset: "+strings.Join(provider.PresetNames(), ", "))
	bindWriteGroupFlag(g, addCmd)

	var edit providerFlags
	editCmd := &cobra.Command{
		Use:   "edit <alias>",
		Short: "Change a Provider's base URL, API path, API key or body; unspecified fields keep their value",
		Long: `Change a Provider. --body replaces its body whole, and --body '{}' clears it; --api-key '' removes
its key reference.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := edit.edit(g, cmd)
			if err != nil {
				return err
			}
			return writeProvider(g, "edited", "in", args[0], func(h *node.Host, group string) (any, error) {
				p, err := h.EditProvider(cmd.Context(), group, node.GroupTalk, args[0], e)
				return providerViewOf(p), err
			})
		},
	}
	edit.bind(editCmd)
	bindWriteGroupFlag(g, editCmd)

	removeCmd := &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a Provider's table, its key reference and body with it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeProvider(g, "removed", "from", args[0], func(h *node.Host, group string) (any, error) {
				return map[string]string{"alias": args[0]}, h.RemoveProvider(cmd.Context(), group, node.GroupTalk, args[0])
			})
		},
	}
	bindWriteGroupFlag(g, removeCmd)

	var keyQuery string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the Providers: alias, URL, shape",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runProviderList(cmd.Context(), g, keyQuery) },
	}
	bindGroupFlag(g, listCmd)
	listCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)
	showCmd := &cobra.Command{
		Use:   "show <alias>",
		Short: "Show one Provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderShow(cmd.Context(), g, keyQuery, args[0])
		},
	}
	bindGroupFlag(g, showCmd)
	showCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)

	root.AddCommand(addCmd, listCmd, showCmd, editCmd, removeCmd)
	return root
}

// writeProvider runs write on the targeted Group's Providers and prints
// what it returns as JSON, or "<done> <alias> <prep> <config.toml>".
func writeProvider(g *globals, done, prep, alias string, write func(*node.Host, string) (any, error)) error {
	h, group, _, err := workspaceTarget(g, "")
	if err != nil {
		return err
	}
	v, err := write(h, group)
	if err != nil {
		return providerError(err)
	}
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "%s %s %s %s\n", done, alias, prep, g.paths().GroupConfigFile(group))
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

func providerViewOf(p node.Provider) providerView {
	return providerView{Alias: p.Alias, BaseURL: p.BaseURL, APIPath: p.APIPath, Shape: p.Shape,
		APIKey: p.APIKey, Body: p.Body, url: p.URL}
}

func runProviderList(ctx context.Context, g *globals, keyQuery string) error {
	h, group, conv, err := workspaceTarget(g, keyQuery)
	if err != nil {
		return err
	}
	ps, err := h.Providers(ctx, group, conv)
	if err != nil {
		return providerReadError(err)
	}
	views := make([]providerView, len(ps))
	for i, p := range ps {
		views[i] = providerViewOf(p)
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

func runProviderShow(ctx context.Context, g *globals, keyQuery, alias string) error {
	h, group, conv, err := workspaceTarget(g, keyQuery)
	if err != nil {
		return err
	}
	p, err := h.Provider(ctx, group, conv, alias)
	if err != nil {
		return providerReadError(err)
	}
	v := providerViewOf(p)
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
