package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const auditManifest = `name = "audit"
version = "1.0.0"
description = "Audits contracts."

[mcp.scanner]
command = "node"
args = ["server.js", "--stdio"]
env = ["ETHERSCAN_KEY"]
`

const auditSkill = `---
name: review
description: How to review a contract.
---
Read the contract first.
`

// git runs git in dir, failing the test on an error, and returns its output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFiles writes each path under dir, making its directories.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
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

// pluginRepo is a git repository holding the audit Plugin, one commit in;
// it returns its directory and that commit.
func pluginRepo(t *testing.T) (dir, commit string) {
	t.Helper()
	dir = t.TempDir()
	git(t, dir, "init", "--quiet")
	writeFiles(t, dir, map[string]string{"plugin.toml": auditManifest, "skills/review/SKILL.md": auditSkill})
	return dir, commitAll(t, dir, "first")
}

// commitAll commits everything in dir and returns the commit.
func commitAll(t *testing.T, dir, msg string) string {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

type lockEntry struct {
	Source  string `toml:"source"`
	Pin     string `toml:"pin"`
	Version string `toml:"version"`
}

// readLock reads the Node's plugins.lock; a missing one is empty.
func readLock(t *testing.T, dir string) map[string]lockEntry {
	t.Helper()
	var lock struct {
		Plugins map[string]lockEntry `toml:"plugins"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "plugins.lock"))
	if os.IsNotExist(err) {
		return map[string]lockEntry{}
	} else if err != nil {
		t.Fatal(err)
	}
	if _, err := toml.Decode(string(raw), &lock); err != nil {
		t.Fatal(err)
	}
	if lock.Plugins == nil {
		return map[string]lockEntry{}
	}
	return lock.Plugins
}

type listedPlugin struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Source  string   `json:"source"`
	BuiltIn bool     `json:"built_in"`
	Missing bool     `json:"missing"`
	Skills  []string `json:"skills"`
	Servers []string `json:"servers"`
}

func listPlugins(t *testing.T, dir string) map[string]listedPlugin {
	t.Helper()
	r := run(t, nil, "", "--dir", dir, "--json", "plugin", "list")
	if r.code != 0 {
		t.Fatalf("plugin list exit %d: %s", r.code, r.stderr)
	}
	var rows []listedPlugin
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil {
		t.Fatalf("%v: %s", err, r.stdout)
	}
	got := map[string]listedPlugin{}
	for _, row := range rows {
		got[row.Name] = row
	}
	return got
}

func TestPluginInstallListUpdateRemoveFromGit(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	repo, first := pluginRepo(t)
	src := "file://" + repo

	r := run(t, nil, "y\n", "--dir", dir, "plugin", "install", src)
	if r.code != 0 {
		t.Fatalf("install exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"audit", "1.0.0", "review", "scanner", "node server.js --stdio", "ETHERSCAN_KEY"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("install does not show %q:\n%s", want, r.stdout)
		}
	}
	if got := readLock(t, dir)["audit"]; got != (lockEntry{Source: src, Pin: first, Version: "1.0.0"}) {
		t.Fatalf("lock after install = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "audit", "skills", "review", "SKILL.md")); err != nil {
		t.Fatalf("not installed: %v", err)
	}

	listed := listPlugins(t, dir)["audit"]
	if listed.Version != "1.0.0" || listed.Source != src || listed.BuiltIn ||
		strings.Join(listed.Skills, ",") != "review" || strings.Join(listed.Servers, ",") != "scanner" {
		t.Fatalf("list = %+v", listed)
	}

	if r := run(t, nil, "", "--dir", dir, "plugin", "install", "--yes", src); r.code == 0 || !strings.Contains(r.stderr, "plugin update") {
		t.Fatalf("second install exit %d: %s", r.code, r.stderr)
	}

	writeFiles(t, repo, map[string]string{"plugin.toml": strings.Replace(auditManifest, "1.0.0", "1.1.0", 1)})
	second := commitAll(t, repo, "second")

	// Declining leaves the Plugin as it was.
	if r := run(t, nil, "n\n", "--dir", dir, "plugin", "update", "audit"); r.code == 0 {
		t.Fatalf("declined update exit 0: %s", r.stdout)
	}
	if got := readLock(t, dir)["audit"]; got.Pin != first {
		t.Fatalf("declined update moved the pin: %+v", got)
	}

	r = run(t, nil, "y\n", "--dir", dir, "plugin", "update", "audit")
	if r.code != 0 {
		t.Fatalf("update exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, `+version = "1.1.0"`) {
		t.Errorf("update does not show the plugin.toml diff:\n%s", r.stdout)
	}
	if got := readLock(t, dir)["audit"]; got != (lockEntry{Source: src, Pin: second, Version: "1.1.0"}) {
		t.Fatalf("lock after update = %+v", got)
	}
	if v := listPlugins(t, dir)["audit"].Version; v != "1.1.0" {
		t.Fatalf("listed version after update = %s", v)
	}

	if r := run(t, nil, "", "--dir", dir, "plugin", "update", "audit"); r.code != 0 || !strings.Contains(r.stdout, "up to date") {
		t.Fatalf("update at the tip exit %d: %s %s", r.code, r.stdout, r.stderr)
	}

	// In the lock with nothing installed, as a failed restore leaves it:
	// listed missing, and an update at the same pin puts it back.
	if err := os.RemoveAll(filepath.Join(dir, "plugins", "audit")); err != nil {
		t.Fatal(err)
	}
	if !listPlugins(t, dir)["audit"].Missing {
		t.Fatal("audit not listed missing")
	}
	if r := run(t, nil, "y\n", "--dir", dir, "plugin", "update", "audit"); r.code != 0 || strings.Contains(r.stdout, "up to date") {
		t.Fatalf("update of a missing plugin exit %d: %s %s", r.code, r.stdout, r.stderr)
	}
	if got := listPlugins(t, dir)["audit"]; got.Missing || got.Version != "1.1.0" {
		t.Fatalf("after update audit listed as %+v", got)
	}

	if r := run(t, nil, "", "--dir", dir, "plugin", "remove", "audit"); r.code != 0 {
		t.Fatalf("remove exit %d: %s", r.code, r.stderr)
	}
	if lock := readLock(t, dir); len(lock) != 0 {
		t.Fatalf("lock after remove = %+v", lock)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "audit")); !os.IsNotExist(err) {
		t.Fatalf("plugin directory left after remove: %v", err)
	}
	if _, ok := listPlugins(t, dir)["audit"]; ok {
		t.Fatal("removed plugin still listed")
	}
}

func TestPluginInstallAtARef(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	repo, first := pluginRepo(t)
	git(t, repo, "tag", "v1")
	writeFiles(t, repo, map[string]string{"plugin.toml": strings.Replace(auditManifest, "1.0.0", "2.0.0", 1)})
	commitAll(t, repo, "second")

	src := "file://" + repo + "@v1"
	if r := run(t, nil, "", "--dir", dir, "plugin", "install", "--yes", src); r.code != 0 {
		t.Fatalf("install exit %d: %s", r.code, r.stderr)
	}
	if got := readLock(t, dir)["audit"]; got != (lockEntry{Source: src, Pin: first, Version: "1.0.0"}) {
		t.Fatalf("lock = %+v", got)
	}
}

func TestPluginInstallRefusalsWriteNothing(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	repo, _ := pluginRepo(t)
	bad := t.TempDir()
	writeFiles(t, bad, map[string]string{"plugin.toml": "name = \"Bad\"\nversion = \"1\"\ndescription = \"d\"\n"})

	for _, tc := range []struct {
		name, stdin string
		args        []string
		want        string
	}{
		{"declined", "n\n", []string{"file://" + repo}, "not installed"},
		{"no answer", "", []string{"file://" + repo}, "--yes"},
		{"malformed", "", []string{"--yes", bad}, `"Bad"`},
		{"missing path", "", []string{"--yes", filepath.Join(bad, "nope")}, "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshot(t, dir)
			r := run(t, nil, tc.stdin, append([]string{"--dir", dir, "plugin", "install"}, tc.args...)...)
			if r.code == 0 || !strings.Contains(r.stderr, tc.want) {
				t.Fatalf("exit %d, stderr %q, want %q", r.code, r.stderr, tc.want)
			}
			untouched(t, before, snapshot(t, dir))
		})
	}
}

func TestPluginFromALocalPath(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"plugin.toml": auditManifest, "skills/review/SKILL.md": auditSkill})

	if r := run(t, nil, "", "--dir", dir, "plugin", "install", "--yes", src); r.code != 0 {
		t.Fatalf("install exit %d: %s", r.code, r.stderr)
	}
	first := readLock(t, dir)["audit"]
	if first.Source != src || !strings.HasPrefix(first.Pin, "sha256:") || first.Version != "1.0.0" {
		t.Fatalf("lock = %+v", first)
	}

	writeFiles(t, src, map[string]string{"plugin.toml": strings.Replace(auditManifest, "1.0.0", "1.0.1", 1)})
	if r := run(t, nil, "", "--dir", dir, "plugin", "update", "audit"); r.code == 0 || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("unanswered update exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "y\n", "--dir", dir, "plugin", "update", "audit"); r.code != 0 {
		t.Fatalf("update exit %d: %s", r.code, r.stderr)
	}
	second := readLock(t, dir)["audit"]
	if second.Version != "1.0.1" || second.Pin == first.Pin || !strings.HasPrefix(second.Pin, "sha256:") {
		t.Fatalf("lock after update = %+v, was %+v", second, first)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "plugins", "audit", "plugin.toml"))
	if err != nil || !strings.Contains(string(raw), "1.0.1") {
		t.Fatalf("update did not re-copy: %v %s", err, raw)
	}
}

func TestBuiltInPluginIsListedAndCannotBeChanged(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)

	builtIn, ok := listPlugins(t, dir)["defios"]
	if !ok || !builtIn.BuiltIn || builtIn.Source != "built-in" || len(builtIn.Skills) == 0 {
		t.Fatalf("defios listed as %+v, %v", builtIn, ok)
	}
	if r := run(t, nil, "", "--dir", dir, "plugin", "list"); !strings.Contains(r.stdout, "built-in") {
		t.Fatalf("list does not mark it built-in:\n%s", r.stdout)
	}
	if lock := readLock(t, dir); len(lock) != 0 {
		t.Fatalf("lock = %+v", lock)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "defios")); !os.IsNotExist(err) {
		t.Fatalf("built-in written to disk: %v", err)
	}

	for _, verb := range []string{"update", "remove"} {
		if r := run(t, nil, "", "--dir", dir, "plugin", verb, "defios"); r.code == 0 || !strings.Contains(r.stderr, "built-in") {
			t.Errorf("%s defios exit %d: %s", verb, r.code, r.stderr)
		}
	}

	impostor := t.TempDir()
	writeFiles(t, impostor, map[string]string{"plugin.toml": "name = \"defios\"\nversion = \"9\"\ndescription = \"Not it.\"\n"})
	before := snapshot(t, dir)
	if r := run(t, nil, "", "--dir", dir, "plugin", "install", "--yes", impostor); r.code == 0 || !strings.Contains(r.stderr, "built-in") {
		t.Fatalf("install of a built-in's name exit %d: %s", r.code, r.stderr)
	}
	untouched(t, before, snapshot(t, dir))
}

func TestRestoreReinstallsGitPluginsAtTheirPin(t *testing.T) {
	orig := t.TempDir()
	backupNode(t, orig)
	repo, first := pluginRepo(t)
	gone := t.TempDir()
	git(t, gone, "init", "--quiet")
	writeFiles(t, gone, map[string]string{"plugin.toml": "name = \"gone\"\nversion = \"1\"\ndescription = \"Its source goes away.\"\n"})
	commitAll(t, gone, "first")
	local := t.TempDir()
	writeFiles(t, local, map[string]string{"plugin.toml": "name = \"notes\"\nversion = \"1\"\ndescription = \"From a path.\"\n"})
	for _, src := range []string{"file://" + repo, "file://" + gone, local} {
		if r := run(t, nil, "", "--dir", orig, "plugin", "install", "--yes", src); r.code != 0 {
			t.Fatalf("install %s exit %d: %s", src, r.code, r.stderr)
		}
	}
	src := takeBackup(t, orig)
	lock := readLock(t, orig)

	// The source moves on and one goes away; the restore takes the pin.
	writeFiles(t, repo, map[string]string{"plugin.toml": strings.Replace(auditManifest, "1.0.0", "2.0.0", 1)})
	commitAll(t, repo, "second")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "restored")
	r := run(t, withPass, "", "--dir", dir, "init", "--from-backup", src)
	if r.code != 0 {
		t.Fatalf("restore exit %d: %s", r.code, r.stderr)
	}
	if got := readLock(t, dir)["audit"]; got.Pin != first {
		t.Fatalf("audit pinned at %s, want %s", got.Pin, first)
	}
	listed := listPlugins(t, dir)
	if v := listed["audit"].Version; v != "1.0.0" || listed["audit"].Missing {
		t.Fatalf("audit restored as %+v, want its pinned 1.0.0", listed["audit"])
	}
	for _, name := range []string{"notes", "gone"} {
		if !listed[name].Missing {
			t.Errorf("%s listed as %+v, want missing", name, listed[name])
		}
	}
	for _, want := range []string{"audit", "notes", "missing", "gone", "failed"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("restore does not report %q:\n%s", want, r.stdout)
		}
	}
	for _, name := range []string{"notes", "gone"} {
		if _, err := os.Stat(filepath.Join(dir, "plugins", name)); !os.IsNotExist(err) {
			t.Errorf("%s restored: %v", name, err)
		}
	}
	if got := readLock(t, dir); len(got) != 3 || got["notes"] != lock["notes"] || got["gone"] != lock["gone"] || got["audit"] != lock["audit"] {
		t.Fatalf("restored lock = %+v, want %+v", got, lock)
	}
}
