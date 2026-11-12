package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"defios.com/internal/backup"
	"defios.com/internal/config"
	"defios.com/internal/secret"
	"defios.com/internal/serve"
	"defios.com/internal/vault"
)

// Vault failures a front end tells apart, besides the vault package's own
// (see Node's errors) and InvalidError.
var (
	// ErrNoPassphrase: the operation needs the Vault opened, the host is
	// locked, and no passphrase was given.
	ErrNoPassphrase = errors.New("node: the Vault passphrase is needed")
	// ErrHosted: this host serves the Node and holds its passphrase; Lock
	// it first.
	ErrHosted = errors.New("node: this host serves the Node; Lock it first")
)

// Secret is one of a Group's Secrets (ADR 0013): a name the Group's Vault
// holds, or one a config field references. Its value is not in it;
// RevealSecret reads it.
type Secret struct {
	Name string
	// Source is where a reference to it resolves: secret.SourceEnv (the
	// environment, which wins), secret.SourceVault or secret.SourceNone.
	Source string
	// Uses are the config fields that reference it, in the Group's
	// config.toml first, then its Conversations', then the grants of the
	// Agent Profiles included in the Group.
	Uses []SecretUse
}

// SecretUse is one config field that references a Secret.
type SecretUse struct {
	// Conversation is the hex ID of the Conversation whose config.toml
	// holds the field, with its Workspace name; GroupTalk for the Group's
	// own config.toml or an Agent Profile's. Conversation is "" too for a
	// Workspace no Key on the replica names.
	Conversation, Workspace string
	// Handle is the Agent Profile whose config.toml holds the field; ""
	// for a Group's or Conversation's.
	Handle string
	// Field is the key, e.g. tailnet_auth_key, providers.<alias>.api_key,
	// networks.<alias>.rpc_urls[i], jobs.<name>.webhook or
	// mcp.<plugin>/<server>.secrets.
	Field string
}

// Secrets lists group's Secrets by name: every one its Vault holds and
// every one its config fields reference. The Vault is opened with this
// host's passphrase while it is unlocked, else with the one passphrase
// gives. The Group need not be served.
func (h *Host) Secrets(ctx context.Context, group string, passphrase Passphrase) ([]Secret, error) {
	if err := h.knownGroup(group); err != nil {
		return nil, err
	}
	uses, err := h.secretUses(group)
	if err != nil {
		return nil, err
	}
	var held map[string]string
	err = h.withSecrets(group, passphrase, true, func(s map[string]string, _ string) error {
		held = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for n := range uses {
		names[n] = true
	}
	for n := range held {
		names[n] = true
	}
	r := h.resolver(group, func() (map[string]string, error) { return held, nil })
	out := make([]Secret, 0, len(names))
	for _, n := range slices.Sorted(maps.Keys(names)) {
		_, source, err := r.Lookup(n)
		if err != nil {
			return nil, err
		}
		out = append(out, Secret{Name: n, Source: source, Uses: uses[n]})
	}
	return out, nil
}

// Secret is group's Secret name, as Secrets lists it: the Vault is opened
// only when the environment lacks name, and passphrase asked for only
// then.
func (h *Host) Secret(ctx context.Context, group, name string, passphrase Passphrase) (Secret, error) {
	_, source, err := h.lookupSecret(group, name, passphrase, true)
	if err != nil {
		return Secret{}, err
	}
	uses, err := h.secretUses(group)
	if err != nil {
		return Secret{}, err
	}
	return Secret{Name: name, Source: source, Uses: uses[name]}, nil
}

// RevealSecret is the value group's Secret name resolves to, "" for none,
// and where it comes from: secret.SourceEnv, SourceVault or SourceNone.
// The Vault opens only with the passphrase entered, never the one a hosted
// Node holds, so a value is read back only for someone who knows it; one
// from the environment needs none.
func (h *Host) RevealSecret(ctx context.Context, group, name string, passphrase Passphrase) (value, source string, err error) {
	return h.lookupSecret(group, name, passphrase, false)
}

// lookupSecret resolves name as a config reference in group would. held
// says whether the Vault may open with this host's passphrase.
func (h *Host) lookupSecret(group, name string, passphrase Passphrase, held bool) (value, source string, err error) {
	if err := secret.ValidateName(name); err != nil {
		return "", "", &InvalidError{Err: err}
	}
	if err := h.knownGroup(group); err != nil {
		return "", "", err
	}
	r := h.resolver(group, func() (map[string]string, error) {
		var stored map[string]string
		err := h.withSecrets(group, passphrase, held, func(s map[string]string, _ string) error {
			stored = s
			return nil
		})
		return stored, err
	})
	return r.Lookup(name)
}

// resolver finds group's Secrets as serving it does: the environment
// first, then held.
func (h *Host) resolver(group string, held func() (map[string]string, error)) *secret.Resolver {
	return &secret.Resolver{Group: group, Getenv: h.o.Getenv, Vault: held}
}

// SetSecret seals value into group's Secrets under name, replacing any
// value it had, with this host's passphrase while it is unlocked, else the
// one passphrase gives. A served Group reads it at its next use.
func (h *Host) SetSecret(ctx context.Context, group, name, value string, passphrase Passphrase) error {
	if err := secret.ValidateName(name); err != nil {
		return &InvalidError{Err: err}
	}
	if value == "" {
		return invalidf("a Secret must not be empty")
	}
	return h.changeSecrets(group, passphrase, func(s map[string]string) bool {
		s[name] = value
		return true
	})
}

// RemoveSecret deletes name from group's Secrets. One that is not there is
// success.
func (h *Host) RemoveSecret(ctx context.Context, group, name string, passphrase Passphrase) error {
	if err := secret.ValidateName(name); err != nil {
		return &InvalidError{Err: err}
	}
	return h.changeSecrets(group, passphrase, func(s map[string]string) bool {
		_, ok := s[name]
		delete(s, name)
		return ok
	})
}

// changeSecrets applies change to group's Secrets and reseals them if it
// says they changed.
func (h *Host) changeSecrets(group string, passphrase Passphrase, change func(map[string]string) bool) error {
	if err := h.knownGroup(group); err != nil {
		return err
	}
	return h.withSecrets(group, passphrase, true, func(s map[string]string, phrase string) error {
		if !change(s) {
			return nil
		}
		return vault.SealSecrets(h.p.VaultSecretsFile(group), s, phrase)
	})
}

// withSecrets calls fn with group's Secrets and the passphrase that opened
// them, holding the Vault's lock throughout: as vaultPassphrase gives it.
func (h *Host) withSecrets(group string, passphrase Passphrase, held bool, fn func(secrets map[string]string, phrase string) error) error {
	unlock, err := useVault(h.p)
	if err != nil {
		return err
	}
	defer unlock()
	phrase, err := h.vaultPassphrase(passphrase, held)
	if err != nil {
		return err
	}
	secrets, err := h.openSecrets(group, phrase)
	if err != nil {
		return err
	}
	return fn(secrets, phrase)
}

// openSecrets opens group's Secrets file, empty when none is sealed yet.
// The passphrase is first checked against the Node's shared secrets, so a
// typo cannot seal a Secrets file the Node's own passphrase will not open.
// The caller holds useVault's lock.
func (h *Host) openSecrets(group, phrase string) (map[string]string, error) {
	if _, err := vault.Open(h.p.VaultSharedFile(), phrase); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoNode
	} else if err != nil {
		return nil, err
	}
	secrets, err := vault.OpenSecrets(h.p.VaultSecretsFile(group), phrase)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	return secrets, err
}

// vaultPassphrase is the Vault passphrase: with held, this host's while it
// is unlocked; else the one passphrase gives.
func (h *Host) vaultPassphrase(passphrase Passphrase, held bool) (string, error) {
	if held {
		h.mu.Lock()
		phrase := h.passphrase
		h.mu.Unlock()
		if phrase != "" {
			return phrase, nil
		}
	}
	if passphrase != nil {
		p, ok, err := passphrase()
		if err != nil {
			return "", err
		}
		if ok && p != "" {
			return p, nil
		}
	}
	return "", ErrNoPassphrase
}

// secretUses is, by Secret name, every config field of group's that
// references one: its config.toml's, its Conversations', and the grants
// of the Agent Profiles included in it.
func (h *Host) secretUses(group string) (map[string][]SecretUse, error) {
	out := map[string][]SecretUse{}
	cfg, err := config.ReadFile(h.p.GroupConfigFile(group))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	in := func(conv, ws string) func(ref, field string) {
		return func(ref, field string) {
			if name, err := secret.ParseRef(ref); err == nil {
				out[name] = append(out[name], SecretUse{Conversation: conv, Workspace: ws, Field: field})
			}
		}
	}
	add := in(GroupTalk, "")
	add(cfg.TailnetAuthKey, "tailnet_auth_key")
	workspaceRefs(cfg.Workspace, add)
	for _, name := range slices.Sorted(maps.Keys(cfg.Jobs)) {
		j := cfg.Jobs[name]
		for _, s := range j.Secrets {
			add("$"+s, "jobs."+name+".secrets")
		}
		add(j.Webhook, "jobs."+name+".webhook")
		for _, k := range slices.Sorted(maps.Keys(j.Headers)) {
			add(j.Headers[k], "jobs."+name+".headers."+k)
		}
	}

	files, err := config.ConversationFiles(h.p, group)
	if err != nil {
		return nil, err
	}
	var ids map[string]string
	if len(files) > 0 {
		ids = h.workspaceIDs(group)
	}
	slices.Sort(files)
	for _, path := range files {
		w, err := config.ReadWorkspace(path)
		if err != nil {
			return nil, err
		}
		ws := filepath.Base(filepath.Dir(path))
		workspaceRefs(w, in(ids[ws], ws))
	}

	includers, err := h.includers()
	if err != nil {
		return nil, err
	}
	for _, handle := range slices.Sorted(maps.Keys(includers)) {
		if !slices.ContainsFunc(includers[handle], func(p Place) bool { return p.Group == group }) {
			continue
		}
		p, err := config.ReadProfile(h.p.AgentProfileFile(handle))
		if errors.Is(err, os.ErrNotExist) {
			continue // included but undefined: nothing references a Secret
		} else if err != nil {
			return nil, err
		}
		for _, srv := range slices.Sorted(maps.Keys(p.MCP)) {
			for _, s := range p.MCP[srv].Secrets {
				out[s] = append(out[s], SecretUse{Handle: handle, Field: "mcp." + srv + ".secrets"})
			}
		}
	}
	return out, nil
}

// workspaceRefs calls add with each Secret reference a Workspace's
// Networks and Providers hold, and the field that holds it.
func workspaceRefs(w config.Workspace, add func(ref, field string)) {
	for _, alias := range slices.Sorted(maps.Keys(w.Networks)) {
		for i, u := range w.Networks[alias].RPCURLs {
			add(u, fmt.Sprintf("networks.%s.rpc_urls[%d]", alias, i))
		}
	}
	for _, alias := range slices.Sorted(maps.Keys(w.Providers)) {
		add(w.Providers[alias].APIKey, "providers."+alias+".api_key")
	}
}

// Backup seals this Node into the new file dst under its Vault passphrase
// (ADR 0027): this host's while it is unlocked, else the one passphrase
// gives. It works while the Node is served. A file already at dst is
// os.ErrExist, and a Vault file that does not open with the passphrase is
// vault.ErrBadPassphrase.
func (h *Host) Backup(ctx context.Context, dst string, passphrase Passphrase) (backup.Manifest, error) {
	unlock, err := useVault(h.p)
	if err != nil {
		return backup.Manifest{}, err
	}
	defer unlock()
	phrase, err := h.vaultPassphrase(passphrase, true)
	if err != nil {
		return backup.Manifest{}, err
	}
	m, err := backup.Write(h.p, phrase, dst)
	if errors.Is(err, backup.ErrNoNode) {
		return backup.Manifest{}, ErrNoNode
	}
	return m, err
}

// CheckPassphrase reports whether passphrase opens the Vault:
// vault.ErrBadPassphrase when it does not. A front end checks it before an
// operation that would act first and need the passphrase only later.
func (h *Host) CheckPassphrase(passphrase string) error {
	unlock, err := useVault(h.p)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := vault.Open(h.p.VaultSharedFile(), passphrase); errors.Is(err, os.ErrNotExist) {
		return ErrNoNode
	} else if err != nil {
		return err
	}
	return nil
}

// CheckPassphraseChange reports what would stop ChangePassphrase that needs
// no passphrase to find, so a front end can refuse before asking for two:
// ErrHosted while this host serves the Node, ErrServedElsewhere while
// another process does.
func (h *Host) CheckPassphraseChange() error {
	if h.serving() != nil {
		return ErrHosted
	}
	if serve.IsUp(h.p.Socket) {
		return ErrServedElsewhere
	}
	return nil
}

// ChangePassphrase re-seals the whole Vault under next, staged so a crash
// never leaves it under two passphrases (ADR 0027), or finishes a change
// an interruption left. It is refused as CheckPassphraseChange says, and
// with vault.ErrBusy when the Vault is in use. Existing Backups are not
// re-sealed.
func (h *Host) ChangePassphrase(ctx context.Context, old, next string) (vault.Change, error) {
	h.op.Lock() // no Unlock starts mid-change
	defer h.op.Unlock()
	if err := h.CheckPassphraseChange(); err != nil {
		return vault.Change{}, err
	}
	if old == "" || next == "" {
		return vault.Change{}, vault.ErrEmptyPassphrase
	}
	if next == old {
		return vault.Change{}, invalidf("the new passphrase is the old one")
	}
	change, err := vault.ChangePassphrase(h.p.VaultDir(), old, next)
	if errors.Is(err, os.ErrNotExist) {
		return vault.Change{}, ErrNoNode
	}
	return change, err
}
