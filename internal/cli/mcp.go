package cli

import (
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"defios.com/internal/builtinmcp"
)

// mcpCommand is `defios mcp <plugin>/<server>`, hidden: the serve runs a
// built-in Plugin's MCP Server as this binary, which serves it over stdio
// (ADR 0030).
func mcpCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp <plugin>/<server>",
		Short:  "Serve a built-in Plugin's MCP Server over stdio, as the serve runs it",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := builtinmcp.Serve(cmd.Context(), args[0], &mcp.StdioTransport{})
			if errors.Is(err, builtinmcp.ErrUnknown) {
				return exitUsagef("%w", err)
			} else if err != nil {
				return exitOperational(err)
			}
			return nil
		},
	}
}
