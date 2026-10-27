package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"defios.com/internal/cosl"
	"defios.com/internal/model"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
)

// modelFlags are the fields add and edit take.
type modelFlags struct {
	provider string
	model    string
	baseURL  string
	apiPath  string
}

func (f *modelFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.provider, "provider", "", "anthropic | openai; fixed for the alias's life")
	cmd.Flags().StringVar(&f.model, "model", "", "the provider's model id, e.g. claude-opus-5-5")
	cmd.Flags().StringVar(&f.baseURL, "base-url", "", "http(s) endpoint, optional (default: the provider's); replicated to every Node, so no credentials")
	cmd.Flags().StringVar(&f.apiPath, "api-path", "", "API path, optional, e.g. /v1/messages or /v1/chat/completions (default: the provider's)")
}

func modelCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "model",
		Short: "Define the Models this Group's agent may use",
		Long: `Model configs live on the Group's COSL, identical on every Node, and are named by a local alias.
They name a provider, a model id and optionally an endpoint; credentials never replicate.
Each Node names its credential per provider in its own config.toml, as
[providers.<provider>] api_key = "$NAME", a Secret (see secret).`,
	}

	var add modelFlags
	addCmd := &cobra.Command{
		Use:   "add <alias>",
		Short: "Add a Model (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := &coslv1.ModelConfig{Alias: args[0], Provider: add.provider, Model: add.model, BaseUrl: add.baseURL, ApiPath: add.apiPath}
			if err := model.Validate(cfg); err != nil {
				return exitUsagef("%w", err)
			}
			v, err := serve.Client{Socket: g.paths().Socket}.ModelAdd(cmd.Context(), cfg)
			return modelWritten(g, "added "+args[0], v, err)
		},
	}
	add.bind(addCmd)

	var edit modelFlags
	editCmd := &cobra.Command{
		Use:   "edit <alias>",
		Short: "Change fields of a Model; unspecified fields keep their value (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cur, err := currentModel(g, args[0])
			if err != nil {
				return err
			}
			cfg := cur.Config()
			fl := cmd.Flags()
			if fl.Changed("provider") {
				cfg.Provider = edit.provider
			}
			if fl.Changed("model") {
				cfg.Model = edit.model
			}
			if fl.Changed("base-url") {
				cfg.BaseUrl = edit.baseURL
			}
			if fl.Changed("api-path") {
				cfg.ApiPath = edit.apiPath
			}
			if err := model.Validate(cfg); err != nil {
				return exitUsagef("%w", err)
			}
			v, err := serve.Client{Socket: g.paths().Socket}.ModelEdit(cmd.Context(), cfg)
			return modelWritten(g, "edited "+args[0], v, err)
		},
	}
	edit.bind(editCmd)

	removeCmd := &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a Model (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := serve.Client{Socket: g.paths().Socket}.ModelRemove(cmd.Context(), args[0])
			return modelWritten(g, "removed "+args[0], v, err)
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the Group's Models: alias, provider, model id",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runModelList(g) },
	}
	bindGroupFlag(g, listCmd)
	showCmd := &cobra.Command{
		Use:   "show <alias>",
		Short: "Show one Model",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runModelShow(g, args[0]) },
	}
	bindGroupFlag(g, showCmd)

	root.AddCommand(addCmd, listCmd, showCmd, editCmd, removeCmd)
	return root
}

func modelWritten(g *globals, what string, v serve.SendView, err error) error {
	if err != nil {
		switch {
		case errors.Is(err, model.ErrInvalid), errors.Is(err, model.ErrExists), errors.Is(err, model.ErrUnknown):
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

// modelView is one Model, human or JSON.
type modelView struct {
	Alias    string `json:"alias"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url,omitempty"`
	APIPath  string `json:"api_path,omitempty"`
}

// foldModels reads the folded Models of the targeted Group.
func foldModels(g *globals) ([]model.Model, error) {
	store, err := openReplica(g)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		return nil, exitOperational(err)
	}
	models, err := model.Fold(rows)
	if err != nil {
		return nil, exitOperational(err)
	}
	return models, nil
}

// currentModel is the live log-state config of alias, for edit.
func currentModel(g *globals, alias string) (model.Model, error) {
	models, err := foldModels(g)
	if err != nil {
		return model.Model{}, err
	}
	m, err := model.Get(models, alias)
	if err != nil {
		return model.Model{}, exitUsagef("%w", err)
	}
	return m, nil
}

func modelViewOf(m model.Model) modelView {
	return modelView{Alias: m.Alias, Provider: m.Provider, Model: m.ModelID, BaseURL: m.BaseURL, APIPath: m.APIPath}
}

func runModelList(g *globals) error {
	models, err := foldModels(g)
	if err != nil {
		return err
	}
	views := make([]modelView, len(models))
	for i, m := range models {
		views[i] = modelViewOf(m)
	}
	if g.json {
		return g.emitJSON(views)
	}
	for _, v := range views {
		fmt.Fprintf(g.app.Stdout, "%s  %s  %s\n", v.Alias, v.Provider, v.Model)
	}
	return nil
}

func runModelShow(g *globals, alias string) error {
	m, err := currentModel(g, alias)
	if err != nil {
		return err
	}
	v := modelViewOf(m)
	if g.json {
		return g.emitJSON(v)
	}
	out := g.app.Stdout
	fmt.Fprintf(out, "alias %s\nprovider %s\nmodel %s\n", v.Alias, v.Provider, v.Model)
	if v.BaseURL != "" {
		fmt.Fprintf(out, "base_url %s\n", v.BaseURL)
	}
	if v.APIPath != "" {
		fmt.Fprintf(out, "api_path %s\n", v.APIPath)
	}
	return nil
}
