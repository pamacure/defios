package cli_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
)

// usageJSON is the slice of `node usage --json` these tests read.
type usageJSON struct {
	Binary struct {
		Path  string `json:"path"`
		Bytes int64  `json:"bytes"`
	} `json:"binary"`
	Data struct {
		Identity    int64 `json:"identity_bytes"`
		VaultShared int64 `json:"vault_shared_bytes"`
		Bytes       int64 `json:"bytes"`
	} `json:"data"`
	Config struct {
		Root  int64 `json:"root_bytes"`
		Bytes int64 `json:"bytes"`
	} `json:"config"`
	Groups []struct {
		Alias   string `json:"alias"`
		Current bool   `json:"current"`
		COSL    struct {
			Bytes  int64  `json:"bytes"`
			Rows   uint64 `json:"rows"`
			Pruned uint64 `json:"pruned_rows"`
		} `json:"cosl"`
		Raft      int64 `json:"raft_bytes"`
		Tsnet     int64 `json:"tsnet_bytes"`
		Vault     int64 `json:"vault_bytes"`
		Workspace int64 `json:"workspace_bytes"`
		Other     int64 `json:"other_bytes"`
		Bytes     int64 `json:"bytes"`
	} `json:"groups"`
	Total int64           `json:"total_bytes"`
	Serve json.RawMessage `json:"serve"`
}

func nodeUsage(t *testing.T, env map[string]string, args ...string) usageJSON {
	t.Helper()
	r := run(t, env, "", append(args, "node", "usage", "--json")...)
	if r.code != 0 {
		t.Fatalf("node usage exit %d: %s", r.code, r.stderr)
	}
	var u usageJSON
	if err := json.Unmarshal([]byte(r.stdout), &u); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	return u
}

// treeBytes is the apparent size of every regular file under root.
func treeBytes(t *testing.T, root string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err == nil {
			n += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func writeBytes(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fileBytes sums the sizes of the paths that exist.
func fileBytes(paths ...string) int64 {
	var n int64
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			n += info.Size()
		}
	}
	return n
}

// twoGroups is a --dir Node with the implicit Group, which has talk (two
// rows of it pruned), Raft, tsnet, Vault and Conversation Workspace files of
// known sizes, plus a second, empty Group "other", which is current.
func twoGroups(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir)
	groupWithTalk(t, dir)
	store, err := cosl.Open(filepath.Join(dir, "group", group.ImplicitAlias, "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := store.Prune(nil, 4); err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	store.Close()
	gdir := filepath.Join(dir, "group", group.ImplicitAlias)
	writeBytes(t, filepath.Join(gdir, "raft.db"), 1000)
	writeBytes(t, filepath.Join(gdir, "raft-snap", "1-2-3", "state.bin"), 24)
	writeBytes(t, filepath.Join(gdir, "tsnet", "tailscaled.state"), 100)
	writeBytes(t, filepath.Join(gdir, "secp256k1-abcdef", "config.toml"), 10)
	writeBytes(t, filepath.Join(gdir, "secp256k1-abcdef", "AGENT.md"), 7)
	writeBytes(t, filepath.Join(dir, "vault", "shares-"+group.ImplicitAlias), 300)
	writeBytes(t, filepath.Join(dir, "vault", "secrets-"+group.ImplicitAlias), 40)
	r := run(t, nil, "", "--dir", dir, "group", "new", "other", "--headscale", "https://hs.example")
	if r.code != 0 {
		t.Fatalf("group new other: %s", r.stderr)
	}
	return dir
}

func TestNodeUsageAccountsEveryGroupWithoutServe(t *testing.T) {
	dir := twoGroups(t)
	u := nodeUsage(t, nil, "--dir", dir)

	if u.Binary.Path == "" || u.Binary.Bytes == 0 {
		t.Fatalf("binary = %+v", u.Binary)
	}
	if string(u.Serve) != "null" {
		t.Fatalf("serve = %s, want null while serve is down", u.Serve)
	}
	if len(u.Groups) != 2 || u.Groups[0].Alias != group.ImplicitAlias || u.Groups[1].Alias != "other" {
		t.Fatalf("groups = %+v", u.Groups)
	}
	if u.Groups[0].Current || !u.Groups[1].Current {
		t.Fatalf("current = %v %v, want other", u.Groups[0].Current, u.Groups[1].Current)
	}

	g := u.Groups[0]
	gdir := filepath.Join(dir, "group", group.ImplicitAlias)
	coslFile := filepath.Join(gdir, "cosl.sqlite")
	if want := fileBytes(coslFile, coslFile+"-wal", coslFile+"-shm"); g.COSL.Bytes != want || want == 0 {
		t.Errorf("COSL bytes = %d, want %d", g.COSL.Bytes, want)
	}
	if g.COSL.Rows != 5 || g.COSL.Pruned != 2 {
		t.Errorf("COSL rows = %d pruned %d, want 5 and 2", g.COSL.Rows, g.COSL.Pruned)
	}
	if g.Raft != 1024 || g.Tsnet != 100 || g.Vault != 340 {
		t.Errorf("raft %d tsnet %d vault %d, want 1024 100 340", g.Raft, g.Tsnet, g.Vault)
	}
	// In one --dir tree the Workspace shares group/<alias>/ with the data;
	// it is counted once, as Workspace, never again as other.
	if want := fileBytes(filepath.Join(gdir, "config.toml")) + 17; g.Workspace != want {
		t.Errorf("workspace = %d, want %d", g.Workspace, want)
	}
	if want := treeBytes(t, gdir) + 340; g.Bytes != want {
		t.Errorf("group bytes = %d, want every file in its subtree plus its Vault files, %d", g.Bytes, want)
	}

	if u.Data.Identity == 0 || u.Data.VaultShared == 0 || u.Config.Root == 0 {
		t.Errorf("node-wide rows = %+v %+v", u.Data, u.Config)
	}
	if u.Total != u.Binary.Bytes+u.Data.Bytes+u.Config.Bytes {
		t.Errorf("total %d != binary %d + data %d + config %d", u.Total, u.Binary.Bytes, u.Data.Bytes, u.Config.Bytes)
	}
	// Every file of the Node is counted, and none twice.
	if want := treeBytes(t, dir); u.Total-u.Binary.Bytes != want {
		t.Errorf("data + config = %d, want the whole tree, %d", u.Total-u.Binary.Bytes, want)
	}
}

func TestNodeUsageGroupFlagNarrowsToOneGroup(t *testing.T) {
	dir := twoGroups(t)
	all := nodeUsage(t, nil, "--dir", dir)
	u := nodeUsage(t, nil, "--dir", dir, "--group", group.ImplicitAlias)

	if len(u.Groups) != 1 || u.Groups[0].Alias != group.ImplicitAlias {
		t.Fatalf("groups = %+v, want only %s", u.Groups, group.ImplicitAlias)
	}
	if u.Data.Identity != all.Data.Identity || u.Data.VaultShared != all.Data.VaultShared || u.Config.Root != all.Config.Root {
		t.Fatalf("node-wide rows changed under --group: %+v %+v", u.Data, u.Config)
	}
	want := u.Binary.Bytes + u.Data.Identity + u.Data.VaultShared + u.Config.Root + u.Groups[0].Bytes
	if u.Total != want {
		t.Fatalf("total %d, want the rows shown, %d", u.Total, want)
	}
	if u.Total >= all.Total {
		t.Fatalf("narrowed total %d not below %d", u.Total, all.Total)
	}

	r := run(t, nil, "", "--dir", dir, "--group", "nope", "node", "usage")
	if r.code != 2 || !strings.Contains(r.stderr, `no Group "nope"`) {
		t.Fatalf("unknown --group: exit %d %s", r.code, r.stderr)
	}
}

// With config and data in separate trees, the Workspace is read from the
// config tree and nothing in either is missed.
func TestNodeUsageSeparateConfigAndDataTrees(t *testing.T) {
	env := map[string]string{
		"DEFIOS_CONFIG_DIR": filepath.Join(t.TempDir(), "config"),
		"DEFIOS_DATA_DIR":   filepath.Join(t.TempDir(), "data"),
		"DEFIOS_PASSPHRASE": "hunter2hunter2",
		"XDG_RUNTIME_DIR":   t.TempDir(),
	}
	if r := run(t, env, "", "init", "--name", "laptop"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	if r := run(t, env, "", "group", "new", "home", "--headscale", "https://hs.example"); r.code != 0 {
		t.Fatalf("group new: %s", r.stderr)
	}
	writeBytes(t, filepath.Join(env["DEFIOS_CONFIG_DIR"], "group", "home", "ed25519-1234", "config.toml"), 9)
	writeBytes(t, filepath.Join(env["DEFIOS_DATA_DIR"], "group", "home", "raft.db"), 50)

	u := nodeUsage(t, env)
	if len(u.Groups) != 1 || !u.Groups[0].Current {
		t.Fatalf("groups = %+v", u.Groups)
	}
	if want := treeBytes(t, filepath.Join(env["DEFIOS_CONFIG_DIR"], "group", "home")); u.Groups[0].Workspace != want {
		t.Errorf("workspace = %d, want %d", u.Groups[0].Workspace, want)
	}
	if u.Groups[0].Raft != 50 {
		t.Errorf("raft = %d, want 50", u.Groups[0].Raft)
	}
	if want := treeBytes(t, env["DEFIOS_CONFIG_DIR"]); u.Config.Bytes != want {
		t.Errorf("config = %d, want %d", u.Config.Bytes, want)
	}
	if want := treeBytes(t, env["DEFIOS_DATA_DIR"]); u.Data.Bytes != want {
		t.Errorf("data = %d, want %d", u.Data.Bytes, want)
	}
}

func TestNodeUsageHumanOutput(t *testing.T) {
	dir := twoGroups(t)
	r := run(t, nil, "", "--dir", dir, "node", "usage")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"binary", "identity", "group other (current)", "COSL", "5 rows, 2 pruned", "1.0 KiB", "total", "serve not running"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}

	fakeServe(t, dir, "[]")
	r = run(t, nil, "", "--dir", dir, "node", "usage")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"serve (other)", "pid", "4242", "uptime", "1h2m3s", "cpu", "rss", "12.0 MiB", "goroutines"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}
}

func TestNodeUsageWithoutNodeIsOperationalError(t *testing.T) {
	r := run(t, nil, "", "--dir", t.TempDir(), "node", "usage")
	if r.code != 2 {
		t.Fatalf("exit %d, want 2: %s", r.code, r.stderr)
	}
}
