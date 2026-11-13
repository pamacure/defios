package shell_test

import (
	"context"
	"os"

	"defios.com/internal/backup"
	"defios.com/internal/node"
	"defios.com/internal/session"
	"defios.com/internal/vault"
)

// The Vault page's Secrets and passphrase change are tested over a real
// Node (vault_host_test.go); the fake writes Backups and refuses the rest.

// fakeExistingFile is a file a Backup cannot be written over.
const fakeExistingFile = "/backups/taken.backup"

func (n *fakeNode) Backup(_ context.Context, dst string, passphrase node.Passphrase) (backup.Manifest, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pass != pw {
		return backup.Manifest{}, node.ErrNoPassphrase
	}
	if dst == fakeExistingFile {
		return backup.Manifest{}, os.ErrExist
	}
	n.written = append(n.written, dst)
	return backup.Manifest{}, nil
}

// backups are the files each Backup wrote.
func (n *fakeNode) backups() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.written...)
}

func (n *fakeNode) ChangePassphrase(context.Context, string, string) (vault.Change, error) {
	return vault.Change{}, errNotFaked
}
func (n *fakeNode) RevealSecret(context.Context, string, string, node.Passphrase) (string, string, error) {
	return "", "", errNotFaked
}
func (n *fakeNode) RemoveSecret(context.Context, string, string, node.Passphrase) error {
	return errNotFaked
}

// signed is a signing Session completing.
func (n *fakeNode) signed() {
	n.emit(node.Event{Session: &node.SessionEvent{Group: n.servedGroup(), Session: node.Session{
		SessionID: "5e55", Status: session.Complete, Curve: "ecdsa", T: 2, N: 2, SignRequest: "5e9",
	}}})
}
