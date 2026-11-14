package shell_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"defios.com/desktop/internal/shell"
)

// writePlugin writes the audit Plugin at version to a new directory, for
// an install from a local path.
func writePlugin(t *testing.T, dir, version string) {
	t.Helper()
	files := map[string]string{
		"plugin.toml": "name = \"audit\"\nversion = \"" + version + "\"\ndescription = \"Audits contracts.\"\n\n" +
			"[mcp.scanner]\ncommand = \"node\"\nargs = [\"server.js\"]\nenv = [\"ETHERSCAN_KEY\"]\n",
		"skills/review/SKILL.md": "---\nname: review\ndescription: How to review a contract.\n---\nRead it first.\n",
	}
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func pluginNamed(v shell.PluginsView, name string) *shell.PluginView {
	for i := range v.Plugins {
		if v.Plugins[i].Name == name {
			return &v.Plugins[i]
		}
	}
	return nil
}

// A Plugin installed from a local path is previewed before Commit, then
// its Skills are grantable on the Agents page; it updates the same way,
// and is removed only once no agent grants an item of it.
func TestAPluginInstalledInTheAppIsGrantableAndRemovedOnceUngranted(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _, ap, _ := formedAt(t)
	src := t.TempDir()
	writePlugin(t, src, "1.0.0")

	v := alice.Plugins()
	if v.ListProblem != nil || len(v.Plugins) == 0 || !v.Plugins[0].BuiltIn {
		t.Fatalf("the built-ins: %+v", v)
	}
	v = alice.StagePluginInstall("  " + src + " ")
	if v.Problem != nil || v.Stage == nil || len(v.Stage.Plugins) != 1 {
		t.Fatalf("stage: %+v %+v", v.Problem, v.Stage)
	}
	staged := v.Stage.Plugins[0]
	if staged.Plugin.Name != "audit" || staged.Plugin.Version != "1.0.0" || len(staged.Plugin.Skills) != 1 ||
		staged.Plugin.Servers[0].CommandLine != "node server.js" || !slices.Equal(staged.Plugin.Servers[0].Env, []string{"ETHERSCAN_KEY"}) {
		t.Fatalf("the preview: %+v", staged)
	}
	if pluginNamed(v, "audit") != nil {
		t.Fatal("installed before Commit")
	}
	if c := alice.CommitPlugins(v.Stage.ID + 1); c.Problem == nil || c.Problem.Code != "stage-gone" {
		t.Fatalf("a stale Commit: %+v", c.Problem)
	}
	v = alice.CommitPlugins(v.Stage.ID)
	if v.Problem != nil || v.Stage != nil || v.Done != "Installed audit 1.0.0." {
		t.Fatalf("commit: %+v %+v %q", v.Problem, v.Stage, v.Done)
	}
	if p := pluginNamed(v, "audit"); p == nil || p.Source != src || p.Pin == "" {
		t.Fatalf("installed: %+v", v.Plugins)
	}
	if cli := defiosCLI(t, ap.Config, "plugin", "list"); !strings.Contains(cli, "audit") {
		t.Fatalf("the CLI's list:\n%s", cli)
	}

	// Its Skill is grantable on the Agents page.
	agentsOK(t, "add provider", alice.SaveProvider(shell.ProviderDraft{New: true, Alias: "anthropic", Preset: "anthropic"}))
	a := agentsOK(t, "add profile", alice.SaveAgentProfile(shell.ProfileDraft{New: true, Handle: "auditor", Provider: "anthropic"}))
	if !slices.ContainsFunc(a.Catalog.Skills, func(s shell.CatalogSkill) bool { return s.Ref == "audit/review" }) {
		t.Fatalf("catalog: %+v", a.Catalog.Skills)
	}
	agentsOK(t, "grant", alice.GrantAgent("auditor", "audit/review", nil))

	// An update all is previewed: the built-ins are not updated, and one
	// already at its pin says so.
	v = alice.StagePluginUpdate("")
	if v.Problem != nil || v.Stage == nil || len(v.Stage.Plugins) != 1 || !v.Stage.Plugins[0].UpToDate {
		t.Fatalf("update all, nothing new: %+v %+v", v.Problem, v.Stage)
	}
	alice.CancelPlugins()
	writePlugin(t, src, "1.1.0")
	v = alice.StagePluginUpdate("audit")
	if v.Problem != nil || v.Stage == nil || v.Stage.Plugins[0].UpToDate || v.Stage.Plugins[0].Plugin.Version != "1.1.0" ||
		v.Stage.Plugins[0].Was.Version != "1.0.0" {
		t.Fatalf("update: %+v %+v", v.Problem, v.Stage)
	}
	if v = alice.CommitPlugins(v.Stage.ID); v.Problem != nil || pluginNamed(v, "audit").Version != "1.1.0" {
		t.Fatalf("commit the update: %+v %+v", v.Problem, v.Plugins)
	}

	// Removed while granted: refused, naming the agent and the item.
	v = alice.RemovePlugin("audit")
	if v.Problem == nil || v.Problem.Code != "plugin-granted" || v.Where != "remove:audit" ||
		!strings.Contains(v.Problem.Message, "@auditor") || !strings.Contains(v.Problem.Message, "audit/review") {
		t.Fatalf("remove while granted: %+v %q", v.Problem, v.Where)
	}
	if pluginNamed(v, "audit") == nil {
		t.Fatal("removed while granted")
	}
	agentsOK(t, "revoke", alice.RevokeAgent("auditor", "audit/review"))
	if v = alice.RemovePlugin("audit"); v.Problem != nil || pluginNamed(v, "audit") != nil || v.Done != "Removed audit." {
		t.Fatalf("remove: %+v %+v", v.Problem, v.Plugins)
	}
}

// A source that is no git URL or path, one installed already and a
// built-in are each refused with their own Problem.
func TestPluginRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _ := formed(t)
	if v := alice.StagePluginInstall(""); v.Problem == nil || v.Field != shell.FieldSource || v.Where != "install" || v.Stage != nil {
		t.Fatalf("no source: %+v %q %q", v.Problem, v.Field, v.Where)
	}
	if v := alice.StagePluginInstall(filepath.Join(t.TempDir(), "nothing-here")); v.Problem == nil || v.Stage != nil {
		t.Fatalf("a path with no Plugin: %+v", v)
	}
	builtIn := alice.Plugins().Plugins[0].Name
	if v := alice.RemovePlugin(builtIn); v.Problem == nil || v.Problem.Code != "built-in" {
		t.Fatalf("remove a built-in: %+v", v.Problem)
	}
	if v := alice.StagePluginUpdate("ghost"); v.Problem == nil || v.Problem.Code != "not-installed" {
		t.Fatalf("update one not installed: %+v", v.Problem)
	}
}
