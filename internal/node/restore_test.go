package node_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"defios.com/internal/backup"
	"defios.com/internal/node"
	"defios.com/internal/nodedir"
	"defios.com/internal/vault"
)

// backupOf seals the Node in p into a Backup file.
func backupOf(t *testing.T, p nodedir.Paths) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "node.backup")
	if _, err := backup.Write(p, passphrase, file); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestRestoreBringsBackTheSameNode(t *testing.T) {
	orig := nodeDir(t)
	file := backupOf(t, orig)

	p := nodedir.Resolve(nodedir.Lookup{Dir: t.TempDir()})
	r, err := node.Restore(context.Background(), p, file, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	want := open(t, orig).NodeID()
	h := open(t, p)
	if r.NodeID != want || h.NodeID() != want {
		t.Fatalf("restored %s, opens as %s; want the original %s", r.NodeID, h.NodeID(), want)
	}
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatalf("restored Node does not serve: %v", err)
	}
}

func TestRestoreRefusals(t *testing.T) {
	orig := nodeDir(t)
	file := backupOf(t, orig)
	ctx := context.Background()

	if _, err := node.Restore(ctx, orig, file, passphrase); !errors.Is(err, node.ErrNodeExists) {
		t.Fatalf("over a Node: %v; want ErrNodeExists", err)
	}

	p := nodedir.Resolve(nodedir.Lookup{Dir: t.TempDir()})
	if _, err := node.Restore(ctx, p, file, "wrong passphrase"); !errors.Is(err, vault.ErrBadPassphrase) {
		t.Fatalf("wrong passphrase: %v; want vault.ErrBadPassphrase", err)
	}
	if _, err := node.Open(p, node.Options{}); !errors.Is(err, node.ErrNoNode) {
		t.Fatalf("a refused Restore left a Node: %v", err)
	}
	var notAFile *node.NotAFileError
	if err := node.CheckRestore(p, t.TempDir()); !errors.As(err, &notAFile) {
		t.Fatalf("a directory as Backup: %v; want NotAFileError", err)
	}

	if err := os.WriteFile(filepath.Join(p.Config, "stray"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var notEmpty *node.ConfigNotEmptyError
	if err := node.CheckRestore(p, file); !errors.As(err, &notEmpty) {
		t.Fatalf("non-empty config dir: %v; want ConfigNotEmptyError", err)
	}
}
