package node_test

import (
	"errors"
	"testing"

	"defios.com/internal/node"
	"defios.com/internal/nodedir"
	"defios.com/internal/vault"
)

// skipPreparams stands in for generating ECDSA preparams, which takes
// minutes; it reports progress as generating would.
func skipPreparams(progress func(string)) ([]byte, error) {
	progress("generating ECDSA preparams")
	return nil, nil
}

func create(t *testing.T, name string) nodedir.Paths {
	t.Helper()
	p := nodedir.Resolve(nodedir.Lookup{Dir: t.TempDir()})
	if _, err := node.Create(p, node.Creation{DisplayName: name, Passphrase: passphrase, Preparams: skipPreparams}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreateMintsANodeThatOpensAndReportsProgress(t *testing.T) {
	p := nodedir.Resolve(nodedir.Lookup{Dir: t.TempDir()})
	var progress []string
	c, err := node.Create(p, node.Creation{
		DisplayName: "laptop",
		Passphrase:  passphrase,
		Progress:    func(msg string) { progress = append(progress, msg) },
		Preparams:   skipPreparams,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) == 0 {
		t.Fatal("no progress reported while preparams generate")
	}
	if c.DisplayName != "laptop" || len(c.NodeID) != 64 {
		t.Fatalf("created = %+v", c)
	}
	if _, err := node.Open(p, node.Options{}); err != nil {
		t.Fatalf("Open after Create: %v", err)
	}
	if _, err := vault.Open(p.VaultSharedFile(), passphrase); err != nil {
		t.Fatalf("Vault does not open with the passphrase: %v", err)
	}
}

func TestCreateRefusals(t *testing.T) {
	existing := create(t, "laptop")
	if err := node.CheckCreate(existing); !errors.Is(err, node.ErrNodeExists) {
		t.Fatalf("CheckCreate over a Node: %v; want ErrNodeExists", err)
	}
	if _, err := node.Create(existing, node.Creation{DisplayName: "x", Passphrase: passphrase, Preparams: skipPreparams}); !errors.Is(err, node.ErrNodeExists) {
		t.Fatalf("Create over a Node: %v; want ErrNodeExists", err)
	}

	fresh := nodedir.Resolve(nodedir.Lookup{Dir: t.TempDir()})
	var invalid *node.InvalidError
	if _, err := node.Create(fresh, node.Creation{DisplayName: "two\nlines", Passphrase: passphrase, Preparams: skipPreparams}); !errors.As(err, &invalid) {
		t.Fatalf("bad display name: %v; want InvalidError", err)
	}
	if _, err := node.Create(fresh, node.Creation{DisplayName: "laptop", Preparams: skipPreparams}); !errors.Is(err, vault.ErrEmptyPassphrase) {
		t.Fatalf("empty passphrase: %v; want vault.ErrEmptyPassphrase", err)
	}
	if _, err := node.Open(fresh, node.Options{}); !errors.Is(err, node.ErrNoNode) {
		t.Fatalf("a refused Create left a Node: %v", err)
	}
}
