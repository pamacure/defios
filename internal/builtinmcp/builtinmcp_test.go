package builtinmcp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"defios.com/internal/builtinmcp"
	"defios.com/internal/plugin"
)

// Serve runs the built-in server a ref names over the transport it is
// given, and refuses a ref no built-in server has.
func TestServeRunsTheNamedBuiltInServer(t *testing.T) {
	builtinmcp.Servers["test/ping"] = func() *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: "ping", Version: "1"}, nil)
		s.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
		})
		return s
	}
	t.Cleanup(func() { delete(builtinmcp.Servers, "test/ping") })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverT, clientT := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- builtinmcp.Serve(ctx, "test/ping", serverT) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if text, ok := res.Content[0].(*mcp.TextContent); !ok || text.Text != "pong" {
		t.Fatalf("ping answered %#v", res.Content)
	}
	cs.Close()
	<-done

	if err := builtinmcp.Serve(ctx, "defios/nope", serverT); !errors.Is(err, builtinmcp.ErrUnknown) {
		t.Fatalf("Serve of an unknown server = %v", err)
	}
}

// Every MCP Server a built-in Plugin's manifest declares has a built-in
// implementation, and every implementation is declared by one.
func TestEveryBuiltInServerIsDeclaredAndImplemented(t *testing.T) {
	declared := map[string]bool{}
	all, err := plugin.All(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		for _, name := range p.ServerNames() {
			ref := p.Name + "/" + name
			declared[ref] = true
			if _, ok := builtinmcp.Servers[ref]; !ok {
				t.Errorf("built-in Plugin %s declares MCP Server %s, which has no implementation", p.Name, ref)
			}
		}
	}
	for ref := range builtinmcp.Servers {
		if !declared[ref] {
			t.Errorf("built-in MCP Server %s is declared by no built-in Plugin", ref)
		}
	}
}
