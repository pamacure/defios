package session

import (
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/nacl/box"
)

// seal wraps plain for one Encryption public key (docs/spec/mvp.md §2.7):
// a fresh ephemeral X25519 keypair, a 24-byte nonce, and NaCl box.
// The ephemeral public key and nonce are returned beside the ciphertext.
func seal(plain, recipientPub []byte) (ephemeralPub, nonce, ciphertext []byte, err error) {
	if len(recipientPub) != 32 {
		return nil, nil, nil, fmt.Errorf("session: encryption public key is %d bytes, want 32", len(recipientPub))
	}
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("session: %w", err)
	}
	var rp [32]byte
	copy(rp[:], recipientPub)
	var n [24]byte
	if _, err := rand.Read(n[:]); err != nil {
		return nil, nil, nil, fmt.Errorf("session: %w", err)
	}
	ct := box.Seal(nil, plain, &n, &rp, priv)
	return append([]byte(nil), pub[:]...), append([]byte(nil), n[:]...), ct, nil
}

// open reverses seal with the recipient's Encryption private key.
func open(ciphertext, nonce, ephemeralPub, recipientPriv []byte) ([]byte, bool) {
	if len(nonce) != 24 || len(ephemeralPub) != 32 || len(recipientPriv) != 32 {
		return nil, false
	}
	var n [24]byte
	var ep, rp [32]byte
	copy(n[:], nonce)
	copy(ep[:], ephemeralPub)
	copy(rp[:], recipientPriv)
	return box.Open(nil, ciphertext, &n, &ep, &rp)
}
