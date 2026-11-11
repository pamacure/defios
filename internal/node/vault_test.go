package node_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"defios.com/internal/node"
	"defios.com/internal/serve"
	"defios.com/internal/vault"
)

// given is a Passphrase that gives phrase.
func given(phrase string) node.Passphrase {
	return func() (string, bool, error) { return phrase, true, nil }
}

func TestSecretsListsEachNameWithItsSourceAndTheFieldsThatReferenceIt(t *testing.T) {
	ctx := context.Background()
	p := nodeDir(t)
	h, err := node.Open(p, node.Options{
		Getenv: func(k string) string {
			if k == "ANTHROPIC_KEY" {
				return "sk-from-env"
			}
			return ""
		},
		NewTailnet: func(serve.TsnetOptions) serve.Tailnet { return loopTailnet{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Lock() })
	if _, err := h.AddProvider(ctx, "alpha", node.GroupTalk, "anthropic", node.ProviderEdit{APIKey: ptr("$ANTHROPIC_KEY")}); err != nil {
		t.Fatal(err)
	}
	if err := h.SetSecret(ctx, "alpha", "ETH_RPC", "https://rpc.example/key", given(passphrase)); err != nil {
		t.Fatal(err)
	}

	got, err := h.Secrets(ctx, "alpha", given(passphrase))
	if err != nil {
		t.Fatal(err)
	}
	want := []node.Secret{
		{Name: "ANTHROPIC_KEY", Source: "env", Uses: []node.SecretUse{{Conversation: node.GroupTalk, Field: "providers.anthropic.api_key"}}},
		{Name: "ETH_RPC", Source: "vault"},
		{Name: "TS_AUTHKEY", Source: "none", Uses: []node.SecretUse{{Conversation: node.GroupTalk, Field: "tailnet_auth_key"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Secrets =\n%+v\nwant\n%+v", got, want)
	}

	// Another Group's Vault is its own.
	beta, err := h.Secrets(ctx, "beta", given(passphrase))
	if err != nil {
		t.Fatal(err)
	}
	if len(beta) != 1 || beta[0].Name != "TS_AUTHKEY" {
		t.Fatalf("beta's Secrets = %+v", beta)
	}

	if _, err := h.Secrets(ctx, "alpha", nil); !errors.Is(err, node.ErrNoPassphrase) {
		t.Fatalf("Secrets with no passphrase: %v; want ErrNoPassphrase", err)
	}
	if _, err := h.Secrets(ctx, "alpha", given("wrong")); !errors.Is(err, vault.ErrBadPassphrase) {
		t.Fatalf("Secrets with the wrong passphrase: %v", err)
	}
}

// While hosted, Secrets are set, removed and listed with the passphrase
// the host holds; none is asked for.
func TestAHostedNodeUsesTheHeldPassphraseForSecrets(t *testing.T) {
	ctx := context.Background()
	h := open(t, nodeDir(t))
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	// Any Group, not only the one served.
	if err := h.SetSecret(ctx, "beta", "ETH_RPC", "https://rpc.example/key", nil); err != nil {
		t.Fatal(err)
	}
	s, err := h.Secret(ctx, "beta", "ETH_RPC", nil)
	if err != nil || s.Source != "vault" {
		t.Fatalf("Secret = %+v, %v; want from the Vault", s, err)
	}
	if err := h.RemoveSecret(ctx, "beta", "ETH_RPC", nil); err != nil {
		t.Fatal(err)
	}
	if err := h.RemoveSecret(ctx, "beta", "ETH_RPC", nil); err != nil {
		t.Fatalf("removing a Secret not there: %v; want success", err)
	}
	if s, err := h.Secret(ctx, "beta", "ETH_RPC", nil); err != nil || s.Source != "none" {
		t.Fatalf("after remove, Secret = %+v, %v", s, err)
	}

	// Locked, it needs one given.
	if err := h.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := h.SetSecret(ctx, "beta", "ETH_RPC", "x", nil); !errors.Is(err, node.ErrNoPassphrase) {
		t.Fatalf("SetSecret locked with no passphrase: %v; want ErrNoPassphrase", err)
	}
	if err := h.SetSecret(ctx, "gamma", "ETH_RPC", "x", given(passphrase)); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("SetSecret in an unknown Group: %v", err)
	}
}

// A Conversation's own config.toml references Secrets too.
func TestASecretsUsesCoverItsGroupsConversations(t *testing.T) {
	ctx := context.Background()
	alice, _, k := keyed(t, "", "")
	h, group := alice.h, "alpha"
	conv, err := h.ResolveConversation(ctx, group, k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddProvider(ctx, group, conv, "local", node.ProviderEdit{
		BaseURL: ptr("http://localhost:11434"), APIPath: ptr("/v1/chat/completions"), APIKey: ptr("$LOCAL_KEY"),
	}); err != nil {
		t.Fatal(err)
	}
	s, err := h.Secret(ctx, group, "LOCAL_KEY", given(passphrase))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Uses) != 1 || s.Uses[0].Conversation != conv || s.Uses[0].Workspace == "" || s.Uses[0].Field != "providers.local.api_key" {
		t.Fatalf("uses = %+v", s.Uses)
	}
}

func TestBackupUsesTheHeldPassphraseAndRefusesAnExistingFile(t *testing.T) {
	ctx := context.Background()
	h := open(t, nodeDir(t))
	dst := filepath.Join(t.TempDir(), "node.backup")
	if _, err := h.Backup(ctx, dst, nil); !errors.Is(err, node.ErrNoPassphrase) {
		t.Fatalf("Backup locked with no passphrase: %v", err)
	}
	if _, err := h.Backup(ctx, dst, given("wrong")); !errors.Is(err, vault.ErrBadPassphrase) {
		t.Fatalf("Backup with the wrong passphrase: %v", err)
	}
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	m, err := h.Backup(ctx, dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.NodeID != h.NodeID() {
		t.Fatalf("manifest = %+v", m)
	}
	if _, err := h.Backup(ctx, dst, nil); !errors.Is(err, os.ErrExist) {
		t.Fatalf("Backup onto an existing file: %v; want os.ErrExist", err)
	}
}

func TestChangePassphraseIsRefusedWhileHosted(t *testing.T) {
	ctx := context.Background()
	p := nodeDir(t)
	h := open(t, p)
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ChangePassphrase(ctx, passphrase, "a new passphrase"); !errors.Is(err, node.ErrHosted) {
		t.Fatalf("ChangePassphrase while hosted: %v; want ErrHosted", err)
	}
	if _, err := open(t, p).ChangePassphrase(ctx, passphrase, "a new passphrase"); !errors.Is(err, node.ErrServedElsewhere) {
		t.Fatalf("ChangePassphrase while another host serves: %v; want ErrServedElsewhere", err)
	}
	if err := h.Lock(); err != nil {
		t.Fatal(err)
	}

	var invalid *node.InvalidError
	if _, err := h.ChangePassphrase(ctx, passphrase, passphrase); !errors.As(err, &invalid) {
		t.Fatalf("the same passphrase: %v; want InvalidError", err)
	}
	if _, err := h.ChangePassphrase(ctx, "wrong", "a new passphrase"); !errors.Is(err, vault.ErrBadPassphrase) {
		t.Fatalf("a wrong old passphrase: %v", err)
	}
	if _, err := h.ChangePassphrase(ctx, passphrase, "a new passphrase"); err != nil {
		t.Fatal(err)
	}
	if err := h.Unlock(passphrase, "alpha"); !errors.Is(err, vault.ErrBadPassphrase) {
		t.Fatalf("Unlock with the old passphrase: %v", err)
	}
	if err := h.Unlock("a new passphrase", "alpha"); err != nil {
		t.Fatalf("Unlock with the new passphrase: %v", err)
	}
}
