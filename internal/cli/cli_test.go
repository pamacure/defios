package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"defios.com/internal/cli"
)

type result struct {
	code   int
	stdout string
	stderr string
}

// run executes the CLI in-process against dir with the given env.
func run(t *testing.T, env map[string]string, stdin string, args ...string) result {
	t.Helper()
	env = withPreparamSkip(env)
	var out, errb bytes.Buffer
	app := cli.App{
		Stdin:  strings.NewReader(stdin),
		Stdout: &out,
		Stderr: &errb,
		Getenv: func(k string) string { return env[k] },
		Home:   t.TempDir(),
	}
	code := app.Run(context.Background(), args)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

var withPass = map[string]string{"DEFIOS_PASSPHRASE": "hunter2hunter2"}

// withPreparamSkip keeps init from generating Paillier preparams. A test
// that wants generation omits the helper and leaves DEFIOS_ECDSA_PREPARAMS
// unset. "skip" is the test hook.
func withPreparamSkip(env map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		out[k] = v
	}
	if _, ok := out["DEFIOS_ECDSA_PREPARAMS"]; !ok {
		out["DEFIOS_ECDSA_PREPARAMS"] = "skip"
	}
	return out
}

func mustInit(t *testing.T, dir string, extra ...string) result {
	t.Helper()
	args := append([]string{"--dir", dir, "init", "--name", "laptop"}, extra...)
	r := run(t, withPass, "", args...)
	if r.code != 0 {
		t.Fatalf("init exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	return r
}

func inspectJSON(t *testing.T, dir string) map[string]any {
	t.Helper()
	r := run(t, nil, "", "--dir", dir, "node", "inspect", "--json")
	if r.code != 0 {
		t.Fatalf("node inspect --json exit %d: %s", r.code, r.stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("node inspect --json is not JSON: %v\n%s", err, r.stdout)
	}
	return doc
}

func TestInitCreatesTreeWithStrictModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node")
	mustInit(t, dir)

	for _, name := range []string{"identity", "vault", "config.toml"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o, want 0600", name, info.Mode().Perm())
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %o, want 0700", info.Mode().Perm())
	}
}

func TestInitWritesIdentityVaultAndConfig(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)

	var id struct {
		Version             int    `json:"version"`
		NodeID              string `json:"node_id"`
		AuthPublicKey       string `json:"auth_public_key"`
		EncryptionPublicKey string `json:"encryption_public_key"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	if id.Version != 1 || len(id.NodeID) != 64 || len(id.AuthPublicKey) != 64 || len(id.EncryptionPublicKey) != 64 {
		t.Fatalf("identity shape wrong: %+v", id)
	}

	vault, err := os.ReadFile(filepath.Join(dir, "vault"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(vault, []byte("age-encryption.org/v1\n-> scrypt ")) || !bytes.Contains(vault, []byte(" 18\n")) {
		t.Fatalf("vault is not an age scrypt/18 file:\n%q", vault[:min(len(vault), 80)])
	}

	cfg, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	wantHost := `hostname = "defios-` + id.NodeID[:8] + `"`
	for _, line := range []string{`display_name = "laptop"`, wantHost, `headscale_url = `} {
		if !strings.Contains(string(cfg), line) {
			t.Errorf("config.toml missing %q:\n%s", line, cfg)
		}
	}
}

func TestInitHonoursHostnameAndHeadscaleFlags(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir, "--hostname", "box", "--headscale-url", "https://hs.example")
	cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	for _, line := range []string{`hostname = "box"`, `headscale_url = "https://hs.example"`} {
		if !strings.Contains(string(cfg), line) {
			t.Errorf("config.toml missing %q:\n%s", line, cfg)
		}
	}
}

func TestInitRefusesExistingNode(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	r := run(t, withPass, "", "--dir", dir, "init", "--name", "again")
	if r.code != 2 {
		t.Fatalf("second init exit %d, want 2; stderr: %s", r.code, r.stderr)
	}
}

func TestSameAuthSeedReproducesSameNodeID(t *testing.T) {
	seed := strings.Repeat("ab", 32)
	a, b := t.TempDir(), t.TempDir()
	mustInit(t, a, "--auth-key", seed)
	mustInit(t, b, "--auth-key", seed)
	ida, idb := inspectJSON(t, a), inspectJSON(t, b)
	if ida["node_id"] != idb["node_id"] {
		t.Fatalf("node ids differ: %v vs %v", ida["node_id"], idb["node_id"])
	}
	if ida["auth_public_key"] != idb["auth_public_key"] {
		t.Fatal("auth public keys differ for the same seed")
	}
}

func TestInitRejectsMalformedAuthKey(t *testing.T) {
	r := run(t, withPass, "", "--dir", t.TempDir(), "init", "--auth-key", "abcd")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1 (usage); stderr: %s", r.code, r.stderr)
	}
}

func TestPassphraseFromFile(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(pf, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, nil, "", "--dir", dir, "init", "--name", "n", "--passphrase-file", pf)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
}

func TestEmptyPassphraseRejected(t *testing.T) {
	pf := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(pf, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, nil, "", "--dir", t.TempDir(), "init", "--name", "n", "--passphrase-file", pf)
	if r.code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", r.code, r.stderr)
	}
}

func TestNoPassphraseSourceWithoutTerminalIsUsageError(t *testing.T) {
	r := run(t, nil, "", "--dir", t.TempDir(), "init", "--name", "n")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", r.code, r.stderr)
	}
}

func TestNodeInspectHumanOutput(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	doc := inspectJSON(t, dir)

	r := run(t, nil, "", "--dir", dir, "node", "inspect")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{
		"Node ID",
		doc["node_id"].(string),
		"laptop",
		"defios-" + doc["node_id"].(string)[:8],
		doc["auth_public_key"].(string),
		doc["encryption_public_key"].(string),
		"Serve",
		"Group",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("node inspect output missing %q:\n%s", want, r.stdout)
		}
	}
}

func TestNodeInspectJSONFields(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir, "--hostname", "box")
	doc := inspectJSON(t, dir)
	if doc["display_name"] != "laptop" || doc["hostname"] != "box" {
		t.Fatalf("display_name/hostname wrong: %v", doc)
	}
	if doc["serve_up"] != false {
		t.Fatalf("serve_up = %v, want false", doc["serve_up"])
	}
	if g, ok := doc["group_id"]; !ok || g != nil {
		t.Fatalf("group_id = %v, want present and null", g)
	}
}

func TestNodeInspectWithoutNodeIsOperationalError(t *testing.T) {
	r := run(t, nil, "", "--dir", filepath.Join(t.TempDir(), "nowhere"), "node", "inspect")
	if r.code != 2 {
		t.Fatalf("exit %d, want 2; stderr: %s", r.code, r.stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"frobnicate"},
		{"node", "inspect", "--bogus"},
		{"init", "extra-positional"},
	} {
		r := run(t, nil, "", append([]string{"--dir", t.TempDir()}, args...)...)
		if r.code != 1 {
			t.Errorf("%v: exit %d, want 1", args, r.code)
		}
	}
}

func TestServeWrongPassphraseIsVerifyFailure(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir, "--headscale-url", "http://headscale.invalid")
	r := run(t, map[string]string{"DEFIOS_PASSPHRASE": "not-it"}, "", "--dir", dir, "serve")
	if r.code != 4 {
		t.Fatalf("exit %d, want 4; stderr: %s", r.code, r.stderr)
	}
}

func TestDirectoryLookupFallsBackToEnvThenXDG(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	env := map[string]string{
		"DEFIOS_PASSPHRASE": "pw-pw-pw",
		"DEFIOS_CONFIG_DIR": cfgDir,
		"DEFIOS_DATA_DIR":   dataDir,
	}
	r := run(t, env, "", "init", "--name", "envnode")
	if r.code != 0 {
		t.Fatalf("init exit %d: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "config.toml")); err != nil {
		t.Errorf("config.toml not under DEFIOS_CONFIG_DIR: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "identity")); err != nil {
		t.Errorf("identity not under DEFIOS_DATA_DIR: %v", err)
	}

	xdg := t.TempDir()
	env = map[string]string{
		"DEFIOS_PASSPHRASE": "pw-pw-pw",
		"XDG_CONFIG_HOME":   filepath.Join(xdg, "cfg"),
		"XDG_DATA_HOME":     filepath.Join(xdg, "data"),
	}
	r = run(t, env, "", "init", "--name", "xdgnode")
	if r.code != 0 {
		t.Fatalf("init exit %d: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(xdg, "cfg", "defios", "config.toml")); err != nil {
		t.Errorf("config.toml not under XDG_CONFIG_HOME/defios: %v", err)
	}
	if _, err := os.Stat(filepath.Join(xdg, "data", "defios", "vault")); err != nil {
		t.Errorf("vault not under XDG_DATA_HOME/defios: %v", err)
	}
}
