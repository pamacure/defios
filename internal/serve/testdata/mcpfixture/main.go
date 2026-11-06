// Command mcpfixture is an MCP Server for serve's tests, over stdio. On
// start it appends its pid to starts.log in its working directory. With
// --long it also offers a tool whose name is too long to offer a model.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	f, err := os.OpenFile("starts.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(f, os.Getpid())
	f.Close()

	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	add := func(name string, do func(args map[string]any) (*mcp.CallToolResult, error)) {
		schema := map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}
		s.AddTool(&mcp.Tool{Name: name, Description: "the fixture's " + name, InputSchema: schema}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, err
				}
			}
			return do(args)
		})
	}
	text := func(s string) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}, nil
	}
	add("echo", func(args map[string]any) (*mcp.CallToolResult, error) {
		return text(fmt.Sprint("echo: ", args["text"]))
	})
	add("env", func(map[string]any) (*mcp.CallToolResult, error) {
		env := os.Environ()
		slices.Sort(env)
		return text(strings.Join(env, "\n"))
	})
	add("cwd", func(map[string]any) (*mcp.CallToolResult, error) {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		return text(wd)
	})
	add("pid", func(map[string]any) (*mcp.CallToolResult, error) { return text(fmt.Sprint(os.Getpid())) })
	add("crash", func(map[string]any) (*mcp.CallToolResult, error) {
		os.Exit(3)
		return nil, nil
	})
	add("fail", func(map[string]any) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "it failed on purpose"}}}, nil
	})
	add("sleep", func(map[string]any) (*mcp.CallToolResult, error) {
		time.Sleep(time.Minute)
		return text("woke")
	})
	if slices.Contains(os.Args[1:], "--long") {
		add(strings.Repeat("x", 60), func(map[string]any) (*mcp.CallToolResult, error) { return text("long") })
	}
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
