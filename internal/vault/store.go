package vault

import (
	"bytes"
	"sync"

	vaultv1 "defios.com/proto/vault/v1"
)

// Store is an unlocked Vault. Put and Delete reseal the file before they
// return, so a crash cannot leave a share only in memory.
type Store struct {
	mu         sync.Mutex
	path       string
	passphrase string
	contents   *vaultv1.VaultContents
}

// NewStore keeps contents, already decrypted, and reseals them to path.
func NewStore(path, passphrase string, contents *vaultv1.VaultContents) *Store {
	return &Store{path: path, passphrase: passphrase, contents: contents}
}

// Put writes or replaces the share for curve and the canonical public key.
func (s *Store) Put(curve string, publicKey, saveData []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	save := append([]byte(nil), saveData...)
	pk := append([]byte(nil), publicKey...)
	for _, sh := range s.contents.Shares {
		if sh.Curve == curve && bytes.Equal(sh.PublicKey, pk) {
			sh.SaveData = save
			return Seal(s.path, s.contents, s.passphrase)
		}
	}
	s.contents.Shares = append(s.contents.Shares, &vaultv1.KeyShare{
		Curve: curve, PublicKey: pk, SaveData: save,
	})
	return Seal(s.path, s.contents, s.passphrase)
}

// Delete removes the share for curve and public key. A missing share is
// success: abort is retried, and the share may already be gone.
func (s *Store) Delete(curve string, publicKey []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]*vaultv1.KeyShare, 0, len(s.contents.Shares))
	found := false
	for _, sh := range s.contents.Shares {
		if sh.Curve == curve && bytes.Equal(sh.PublicKey, publicKey) {
			found = true
			continue
		}
		next = append(next, sh)
	}
	if !found {
		return nil
	}
	s.contents.Shares = next
	return Seal(s.path, s.contents, s.passphrase)
}

// Present reports whether a share for curve and public key is in memory.
func (s *Store) Present(curve string, publicKey []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sh := range s.contents.Shares {
		if sh.Curve == curve && bytes.Equal(sh.PublicKey, publicKey) {
			return true
		}
	}
	return false
}

// ShareRef is a public key this Vault holds a share of. Save data stays put.
type ShareRef struct {
	Curve     string
	PublicKey []byte
}

// Shares lists the public keys present, without their save data.
func (s *Store) Shares() []ShareRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ShareRef, len(s.contents.Shares))
	for i, sh := range s.contents.Shares {
		out[i] = ShareRef{Curve: sh.Curve, PublicKey: append([]byte(nil), sh.PublicKey...)}
	}
	return out
}
