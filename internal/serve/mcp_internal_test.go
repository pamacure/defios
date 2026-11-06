package serve

import (
	"os"
	"slices"
	"testing"

	"defios.com/internal/plugin"
)

// A built-in Plugin's MCP Server runs as this binary, `defios mcp
// <plugin>/<server>`, whatever its manifest's command; an installed one
// runs its command in its directory. Either gets exactly the env given,
// none of the serve's own even when that is empty.
func TestServerCommandRunsABuiltInAsThisBinary(t *testing.T) {
	srv := plugin.Server{Command: "node", Args: []string{"server.js"}}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := serverCommand(plugin.Plugin{Name: "defios", BuiltIn: true}, "chain", srv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != exe || !slices.Equal(cmd.Args, []string{exe, "mcp", "defios/chain"}) || cmd.Env == nil || len(cmd.Env) != 0 {
		t.Errorf("built-in: path %q, args %q, env %#v", cmd.Path, cmd.Args, cmd.Env)
	}
	env := []string{"PATH=/bin", "TOKEN=t"}
	cmd, err = serverCommand(plugin.Plugin{Name: "tools", Dir: "/plugins/tools"}, "fx", srv, env)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cmd.Args, []string{"node", "server.js"}) || cmd.Dir != "/plugins/tools" || !slices.Equal(cmd.Env, env) {
		t.Errorf("installed: args %q, dir %q, env %#v", cmd.Args, cmd.Dir, cmd.Env)
	}
}
