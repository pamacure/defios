package builtinmcp_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"defios.com/internal/builtinmcp"
	"defios.com/internal/network"
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

// Every built-in Plugin but defios is a chain Plugin, named exactly as a
// Network type and offering the MCP Server tx, and every Network type has
// one (ADR 0042), offering the Skills review-sign-request and propose.
func TestEveryChainPluginOffersTheSameItems(t *testing.T) {
	all, err := plugin.All(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]bool{}
	for _, typ := range network.Types() {
		types[typ] = false
	}
	for _, p := range all {
		if p.Name == "defios" {
			continue
		}
		if _, ok := types[p.Name]; !ok {
			t.Errorf("built-in Plugin %s is named after no Network type", p.Name)
			continue
		}
		types[p.Name] = true
		if !slices.Contains(p.ServerNames(), "tx") {
			t.Errorf("built-in Plugin %s offers MCP Servers %v, missing tx", p.Name, p.ServerNames())
		}
		skills := skillNames(p)
		for _, want := range []string{"propose", "review-sign-request"} {
			if !slices.Contains(skills, want) {
				t.Errorf("built-in Plugin %s offers Skills %v, missing %s", p.Name, skills, want)
			}
		}
	}
	for typ, ok := range types {
		if !ok {
			t.Errorf("Network type %s has no built-in Plugin", typ)
		}
	}
}

// defios offers work-in-a-group alone, the Skill for what no chain decides:
// reviewing a Sign Request is each chain Plugin's (ADR 0042).
func TestDefiosOffersWorkInAGroupAlone(t *testing.T) {
	all, err := plugin.All(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if p.Name != "defios" {
			continue
		}
		if skills := skillNames(p); !slices.Equal(skills, []string{"work-in-a-group"}) {
			t.Fatalf("built-in Plugin defios offers Skills %v, want [work-in-a-group]", skills)
		}
		return
	}
	t.Fatal("no built-in Plugin defios")
}

// skillNames is the names of the Skills p offers.
func skillNames(p plugin.Plugin) []string {
	var names []string
	for _, s := range p.Skills {
		names = append(names, s.Name)
	}
	return names
}
