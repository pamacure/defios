package cli_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// modelReplica writes a COSL for the implicit Group holding a membership.join
// and one model.create per config, as a serve would have committed them.
func modelReplica(t *testing.T, dir string, configs ...*coslv1.ModelConfig) {
	t.Helper()
	alice, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(group.RosterNode{Public: alice.Public, DisplayName: "alice"})
	g := roster.GroupID()
	gdir := filepath.Join(dir, "group", group.ImplicitAlias)
	if err := os.MkdirAll(gdir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := cosl.Open(filepath.Join(gdir, "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fsm := cosl.NewFSM(store, roster)
	type entry struct {
		typ     uint32
		payload proto.Message
	}
	entries := []entry{{cosl.TypeMembershipJoin, cosl.MembershipJoin(alice, "alice", "defios-alice")}}
	for _, c := range configs {
		entries = append(entries, entry{cosl.TypeModelCreate, c})
	}
	for i, e := range entries {
		raw, _ := proto.Marshal(e.payload)
		msgID := make([]byte, 32)
		rand.Read(msgID)
		env := &coslv1.Envelope{
			Version: 1, GroupId: g[:], MsgType: e.typ, Sender: alice.NodeID[:],
			MsgId: msgID, CreatedAt: 1700000000_000000000 + int64(i)*1e9, Payload: raw,
		}
		data, err := cosl.Sign(env, ed25519.NewKeyFromSeed(alice.AuthSeed))
		if err != nil {
			t.Fatal(err)
		}
		if res, err := fsm.Apply(uint64(i+1), 1, data); err != nil || !res.Inserted {
			t.Fatalf("apply %d: %v %+v", i, err, res)
		}
	}
}

// credentialNode is an initialised Node with one Group holding an anthropic
// Model "claude" and an openai Model "gpt".
func credentialNode(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir)
	modelReplica(t, dir,
		&coslv1.ModelConfig{Alias: "claude", Provider: "anthropic", Model: "claude-opus-5-5"},
		&coslv1.ModelConfig{Alias: "gpt", Provider: "openai", Model: "gpt-5"},
	)
	return dir
}

// fileContaining names the first file under dir whose bytes contain secret.
func fileContaining(t *testing.T, dir, secret string) string {
	t.Helper()
	var hit string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || hit != "" {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte(secret)) {
			hit = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hit
}

func TestModelCredentialSetThenShowWithTheVaultUnlocked(t *testing.T) {
	dir := credentialNode(t)
	const secret = "sk-ant-api03-secretsecret"

	r := run(t, withPass, secret+"\n", "--dir", dir, "model", "credential", "set", "claude")
	if r.code != 0 {
		t.Fatalf("set exit %d: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stdout+r.stderr, secret) {
		t.Fatalf("set echoed the credential: %q", r.stdout+r.stderr)
	}

	r = run(t, withPass, "", "--dir", dir, "model", "credential", "show", "claude")
	if r.code != 0 {
		t.Fatalf("show exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "source vault") || strings.Contains(r.stdout, secret) {
		t.Fatalf("show without --reveal:\n%s", r.stdout)
	}

	r = run(t, withPass, "", "--dir", dir, "--json", "model", "credential", "show", "claude", "--reveal")
	if r.code != 0 {
		t.Fatalf("show --reveal exit %d: %s", r.code, r.stderr)
	}
	var v struct {
		Alias, Source, Env, Credential string
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	if v.Alias != "claude" || v.Source != "vault" || v.Env != "ANTHROPIC_API_KEY" || v.Credential != secret {
		t.Fatalf("show --reveal --json = %+v", v)
	}

	// The credential is encrypted at rest: not in config.toml, not on the
	// COSL, not anywhere in the Node's tree in the clear.
	if hit := fileContaining(t, dir, secret); hit != "" {
		t.Fatalf("credential in the clear in %s", hit)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault", "credentials-"+group.ImplicitAlias)); err != nil {
		t.Fatalf("credentials file: %v", err)
	}
}

// The provider's environment variable wins over the Vault for every alias of
// that provider, and needs no passphrase to report.
func TestModelCredentialEnvironmentVariableTakesPrecedence(t *testing.T) {
	dir := credentialNode(t)
	if r := run(t, withPass, "from-vault", "--dir", dir, "model", "credential", "set", "claude"); r.code != 0 {
		t.Fatalf("set exit %d: %s", r.code, r.stderr)
	}
	env := map[string]string{"ANTHROPIC_API_KEY": "from-env"}
	r := run(t, env, "", "--dir", dir, "model", "credential", "show", "claude", "--reveal")
	if r.code != 0 {
		t.Fatalf("show exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "source env") || !strings.Contains(r.stdout, "credential from-env") {
		t.Fatalf("show:\n%s", r.stdout)
	}

	// OPENAI_API_KEY is unset, so gpt has nothing.
	r = run(t, withPass, "", "--dir", dir, "model", "credential", "show", "gpt")
	if r.code != 0 || !strings.Contains(r.stdout, "source none") {
		t.Fatalf("show gpt exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
}

func TestModelCredentialRemove(t *testing.T) {
	dir := credentialNode(t)
	if r := run(t, withPass, "s3cret", "--dir", dir, "model", "credential", "set", "claude"); r.code != 0 {
		t.Fatalf("set exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, withPass, "", "--dir", dir, "model", "credential", "remove", "claude"); r.code != 0 {
		t.Fatalf("remove exit %d: %s", r.code, r.stderr)
	}
	r := run(t, withPass, "", "--dir", dir, "model", "credential", "show", "claude")
	if r.code != 0 || !strings.Contains(r.stdout, "source none") {
		t.Fatalf("show after remove exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
}

func TestModelCredentialSetErrors(t *testing.T) {
	dir := credentialNode(t)
	// A passphrase that does not open this Node's Vault must not seal a
	// credentials file of its own.
	bad := map[string]string{"DEFIOS_PASSPHRASE": "not-the-passphrase"}
	if r := run(t, bad, "s3cret", "--dir", dir, "model", "credential", "set", "claude"); r.code != 4 {
		t.Fatalf("wrong passphrase exit %d, want 4: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault", "credentials-"+group.ImplicitAlias)); !os.IsNotExist(err) {
		t.Fatalf("credentials file written under a wrong passphrase: %v", err)
	}
	if r := run(t, withPass, "\n", "--dir", dir, "model", "credential", "set", "claude"); r.code != 1 {
		t.Fatalf("empty credential exit %d, want 1: %s", r.code, r.stderr)
	}
	if r := run(t, withPass, "s3cret", "--dir", dir, "model", "credential", "set", "Bad Alias"); r.code != 1 {
		t.Fatalf("bad alias exit %d, want 1: %s", r.code, r.stderr)
	}
	if r := run(t, withPass, "", "--dir", dir, "model", "credential", "show", "nope"); r.code != 1 {
		t.Fatalf("show unknown model exit %d, want 1: %s", r.code, r.stderr)
	}
}

// init has no credential input: even with a provider variable in its
// environment it writes no credentials file and the value lands nowhere.
func TestInitNeverWritesACredential(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"DEFIOS_PASSPHRASE": "hunter2hunter2", "ANTHROPIC_API_KEY": "sk-ant-from-env", "OPENAI_API_KEY": "sk-oai-from-env"}
	if r := run(t, env, "", "--dir", dir, "init", "--name", "laptop"); r.code != 0 {
		t.Fatalf("init exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, env, "", "--dir", dir, "group", "new", group.ImplicitAlias, "--headscale", "https://hs.example"); r.code != 0 {
		t.Fatalf("group new exit %d: %s", r.code, r.stderr)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "vault", "credentials-*"))
	if len(matches) != 0 {
		t.Fatalf("credentials files after init: %v", matches)
	}
	for _, s := range []string{"sk-ant-from-env", "sk-oai-from-env"} {
		if hit := fileContaining(t, dir, s); hit != "" {
			t.Fatalf("%s written to %s", s, hit)
		}
	}
}

// group new lays down the group-level agent config directory, empty.
func TestGroupNewCreatesTheGroupLevelConversationConfigDir(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir)
	path := filepath.Join(dir, "group", group.ImplicitAlias, "conversation", "group-level")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("%s: dir=%v mode %o", path, info.IsDir(), info.Mode().Perm())
	}
}
