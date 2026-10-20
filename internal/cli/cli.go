// Package cli is the defios command tree (docs/spec/mvp.md §9).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"defios.com/internal/nodedir"
)

// App is the process environment the CLI runs in. Everything is injected so
// tests can run verbs in-process.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Home   string
}

// globals are the root flags every verb sees.
type globals struct {
	app  App
	dir  string
	json bool
}

// paths resolves the Node layout for this invocation.
func (g *globals) paths() nodedir.Paths {
	return nodedir.Resolve(nodedir.Lookup{Dir: g.dir, Getenv: g.app.Getenv, Home: g.app.Home})
}

// emitJSON writes v as indented JSON to stdout.
func (g *globals) emitJSON(v any) error {
	enc := json.NewEncoder(g.app.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Run executes args and returns the process exit code.
func (a App) Run(ctx context.Context, args []string) int {
	g := &globals{app: a}
	root := a.rootCommand(g)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		code := ExitUsage // cobra's own errors: unknown verb, bad flag, arity
		var exit *ExitError
		if errors.As(err, &exit) {
			code = exit.Code
		}
		fmt.Fprintf(a.Stderr, "defios: %v\n", err)
		return code
	}
	return ExitOK
}

func (a App) rootCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:           "defios",
		Short:         "A self-hosted multi-party computation wallet Node",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(a.Stdin)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	root.PersistentFlags().StringVar(&g.dir, "dir", "", "config and data directory (one tree; overrides env and XDG)")
	root.PersistentFlags().BoolVar(&g.json, "json", false, "machine-readable output")

	root.AddCommand(initCommand(g))
	root.AddCommand(nodeCommand(g))
	root.AddCommand(serveCommand(g))
	return root
}
