package shell

import (
	"context"
	"time"

	"defios.com/desktop/internal/prefs"
	"defios.com/internal/backup"
	"defios.com/internal/node"
	"defios.com/internal/session"
	"defios.com/internal/vault"
)

// The Vault page: this device's Backup and passphrase change, and the
// edit-target Group's Secrets. A Secret's value is read back only with the
// passphrase typed for it, never the one the app holds.

// VaultNode is what the Vault page needs of a Node besides the rest of
// Node: CheckPassphrase, Lock, Unlock, Secrets and SetSecret.
type VaultNode interface {
	Backup(ctx context.Context, dst string, passphrase node.Passphrase) (backup.Manifest, error)
	ChangePassphrase(ctx context.Context, old, next string) (vault.Change, error)
	RevealSecret(ctx context.Context, group, name string, passphrase node.Passphrase) (value, source string, err error)
	RemoveSecret(ctx context.Context, group, name string, passphrase node.Passphrase) error
}

// VaultView is the Vault page.
type VaultView struct {
	// Group is the edit-target Group, whose Secrets show, and Served
	// whether it is served now.
	Group  string
	Served bool

	Secrets        []VaultSecret
	SecretsProblem *Problem

	// LastBackup is when the app last wrote a Backup; nil for never.
	LastBackup *time.Time

	// Problem is why the last change was refused, Where the form it is
	// about ("backup", "passphrase", "new-secret" or "secret:<name>") and
	// Field the field ("old", "next", "again"; "name", "value").
	Problem *Problem
	Where   string
	Field   string
	// Done is what the last change did: "backup", "passphrase", "set" or
	// "removed".
	Done string
}

// VaultSecret is one of the Group's Secrets: where it resolves from
// (secret.SourceEnv, SourceVault or SourceNone) and the config fields
// that reference it.
type VaultSecret struct {
	Name, Source string
	Uses         []SecretUseView
}

// SecretUseView is a config field that references a Secret. Place is
// "Group" for the Group's own config, a Workspace's name for a
// Conversation's, or "@handle" for an Agent Profile's.
type SecretUseView struct {
	Place, Field string
}

// SecretDraft sets a Secret's value: a new one, or one listed.
type SecretDraft struct {
	New         bool
	Name, Value string
}

// RevealView is a Secret's value, read with the passphrase typed.
type RevealView struct {
	Name, Value, Source string
	Problem             *Problem
}

// Vault is the Vault page for the edit-target Group.
func (s *Shell) Vault() VaultView {
	v, ok := s.vaultTarget()
	if ok {
		s.fillVault(&v)
	}
	return v
}

// BackupTo writes a Backup of this device to the new file dst, under its
// passphrase, and clears the "Back up now" banner.
func (s *Shell) BackupTo(dst string) VaultView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.vaultTarget()
	if !ok {
		return v
	}
	v.Where = "backup"
	if _, err := s.node().Backup(s.ctx, dst, s.held()); err != nil {
		v.Problem = backupWriteProblem(err)
	} else {
		now := time.Now()
		s.savePrefs(func(p *prefs.Prefs) { p.BackupDue, p.LastBackup = false, &now })
		v.Done = "backup"
	}
	s.fillVault(&v)
	return v
}

// ChangePassphrase re-seals this device's Vault under next. It stops
// serving first, as a change needs, then serves the same Group again
// under next; under old again if the change fails.
func (s *Shell) ChangePassphrase(old, next, again string) VaultView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.vaultTarget()
	if !ok {
		return v
	}
	v.Where = "passphrase"
	n := s.node()
	refuse := func(field string, p *Problem) VaultView {
		v.Field, v.Problem = field, p
		s.fillVault(&v)
		return v
	}
	switch {
	case old == "":
		return refuse("old", &Problem{"empty-passphrase", "Enter the passphrase this device has now."})
	case next == "":
		return refuse("next", &Problem{"empty-passphrase", "Enter a new passphrase."})
	case next == old:
		return refuse("next", &Problem{"same-passphrase", "The new passphrase is the one this device has now."})
	case again != next:
		return refuse("again", &Problem{"mismatch", "The two new passphrases differ."})
	}
	if err := n.CheckPassphrase(old); err != nil {
		return refuse("old", problemOf(err))
	}

	served := s.State().Served
	// Unlocking keeps the Lock's status from routing to the lock screen.
	s.set(func(st *State) { st.View, st.Problem = ViewUnlocking, nil })
	if err := n.Lock(); err != nil {
		s.apply(n.Status())
		return refuse("", problemOf(err))
	}
	s.forget()
	_, err := n.ChangePassphrase(s.ctx, old, next)
	pass := next
	if err != nil {
		pass = old
	}
	if st := s.unlock(served, pass); st.View != ViewServed {
		// Not serving again: the lock screen says why.
		v.Problem = st.Problem
		if err != nil {
			v.Problem = problemOf(err)
		}
		return v
	}
	if err != nil {
		return refuse("", problemOf(err))
	}
	v.Done = "passphrase"
	s.fillVault(&v)
	return v
}

// SaveVaultSecret keeps d's value in the edit-target Group's Vault.
func (s *Shell) SaveVaultSecret(d SecretDraft) VaultView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.vaultTarget()
	if !ok {
		return v
	}
	v.Where = "secret:" + d.Name
	if d.New {
		v.Where = "new-secret"
	}
	if v.Field, v.Problem = s.setSecret(v.Group, d.Name, d.Value); v.Problem == nil {
		v.Done = "set"
	}
	s.fillVault(&v)
	return v
}

// RemoveVaultSecret deletes name from the edit-target Group's Vault, if
// it holds it. A config field may still reference it, and the
// environment may still supply it.
func (s *Shell) RemoveVaultSecret(name string) VaultView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.vaultTarget()
	if !ok {
		return v
	}
	v.Where = "secret:" + name
	if err := s.node().RemoveSecret(s.ctx, v.Group, name, s.held()); err != nil {
		v.Problem = problemOf(err)
	} else {
		v.Done = "removed"
	}
	s.fillVault(&v)
	return v
}

// RevealSecret is the value of the edit-target Group's Secret name, read
// with passphrase as typed: the Vault never opens for it with the one the
// app holds.
func (s *Shell) RevealSecret(name, passphrase string) RevealView {
	r := RevealView{Name: name}
	v, ok := s.vaultTarget()
	if !ok {
		r.Problem = problemOf(node.ErrLocked)
		return r
	}
	typed := func() (string, bool, error) { return passphrase, passphrase != "", nil }
	value, source, err := s.node().RevealSecret(s.ctx, v.Group, name, typed)
	if err != nil {
		r.Problem = problemOf(err)
		return r
	}
	r.Value, r.Source = value, source
	return r
}

// vaultTarget is the page's Group and whether it is served; ok false,
// with nothing to show, while no Group is served in this app.
func (s *Shell) vaultTarget() (VaultView, bool) {
	st := s.State()
	v := VaultView{Group: st.EditGroup, Served: st.EditGroupServed}
	return v, s.node() != nil && st.View == ViewServed && v.Group != ""
}

func (s *Shell) fillVault(v *VaultView) {
	v.LastBackup = s.Prefs().LastBackup
	list, err := s.node().Secrets(s.ctx, v.Group, s.held())
	if err != nil {
		v.SecretsProblem = problemOf(err)
		return
	}
	v.Secrets = make([]VaultSecret, len(list))
	for i, sc := range list {
		out := VaultSecret{Name: sc.Name, Source: sc.Source}
		for _, u := range sc.Uses {
			out.Uses = append(out.Uses, SecretUseView{Place: usePlace(u), Field: u.Field})
		}
		v.Secrets[i] = out
	}
}

func usePlace(u node.SecretUse) string {
	switch {
	case u.Handle != "":
		return "@" + u.Handle
	case u.Workspace != "":
		return u.Workspace
	}
	return "Group"
}

// keyMinted asks for a Backup once a keygen Session of the served Group
// completes: the new Key's share is in no Backup yet.
func (s *Shell) keyMinted(served string, ev *node.SessionEvent) {
	if ev == nil || ev.Group != served || served == "" ||
		ev.Session.SignRequest != "" || ev.Session.Status != session.Complete {
		return
	}
	s.mu.Lock()
	due := s.p.BackupDue
	s.p.BackupDue = true
	s.mu.Unlock()
	if !due {
		if err := s.persist(); err != nil {
			s.fail(appProblem("prefs", err))
		}
	}
	s.set(func(st *State) { st.KeysMinted++ })
}
