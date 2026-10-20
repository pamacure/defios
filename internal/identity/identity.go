// Package identity mints a Node's keys, derives its Node ID, and reads and
// writes the plaintext identity file (docs/spec/mvp.md §2.1, §3.2).
package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// nodeIDDomain separates Node IDs from every other SHA-256 in the system.
const nodeIDDomain = "defios-node-id-v1"

// ID is a 32-byte Node ID.
type ID [32]byte

// String is the lowercase hex display form.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// NodeID derives SHA-256("defios-node-id-v1" || auth_public_key).
func NodeID(authPublicKey ed25519.PublicKey) ID {
	h := sha256.New()
	h.Write([]byte(nodeIDDomain))
	h.Write(authPublicKey)
	var id ID
	copy(id[:], h.Sum(nil))
	return id
}

// Public is what the identity file holds: nothing secret.
type Public struct {
	NodeID              ID
	AuthPublicKey       ed25519.PublicKey
	EncryptionPublicKey []byte // 32-byte X25519
}

// Node is a Node's full key material; it only exists inside init and serve.
type Node struct {
	Public
	AuthSeed             []byte // 32-byte Ed25519 seed
	EncryptionPrivateKey []byte // 32-byte X25519 scalar
}

// Generate mints a fresh auth seed and Encryption keypair.
func Generate() (*Node, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return FromSeed(seed)
}

// FromSeed restores the auth keypair from a 32-byte seed and mints a fresh
// Encryption keypair. This is the one place a Node ID is computed: init.
// Later readers take it from the identity file (ADR 0005), so a rotated
// auth key never changes it.
func FromSeed(seed []byte) (*Node, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("auth seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	encPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	authPub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return &Node{
		Public: Public{
			NodeID:              NodeID(authPub),
			AuthPublicKey:       authPub,
			EncryptionPublicKey: encPriv.PublicKey().Bytes(),
		},
		AuthSeed:             append([]byte(nil), seed...),
		EncryptionPrivateKey: encPriv.Bytes(),
	}, nil
}

// Unlock rebuilds the Node from the identity file's public half and the
// Vault's secrets. The Node ID is taken from the file, never recomputed
// (ADR 0005); the seed must still produce the file's auth public key.
func Unlock(pub Public, seed, encryptionPrivateKey []byte) (*Node, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("vault auth seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	authPub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !authPub.Equal(pub.AuthPublicKey) {
		return nil, errors.New("vault auth seed does not match the identity file's auth public key")
	}
	encPriv, err := ecdh.X25519().NewPrivateKey(encryptionPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("vault encryption private key: %w", err)
	}
	encPub, err := ecdh.X25519().NewPublicKey(pub.EncryptionPublicKey)
	if err != nil || !encPriv.PublicKey().Equal(encPub) {
		return nil, errors.New("vault encryption private key does not match the identity file's encryption public key")
	}
	return &Node{
		Public:               pub,
		AuthSeed:             append([]byte(nil), seed...),
		EncryptionPrivateKey: encPriv.Bytes(),
	}, nil
}

// fileV1 is the on-disk JSON shape of the identity file.
type fileV1 struct {
	Version             int    `json:"version"`
	NodeID              string `json:"node_id"`
	AuthPublicKey       string `json:"auth_public_key"`
	EncryptionPublicKey string `json:"encryption_public_key"`
}

// WriteFile writes the identity file, mode 0600.
func WriteFile(path string, p Public) error {
	doc := fileV1{
		Version:             1,
		NodeID:              p.NodeID.String(),
		AuthPublicKey:       hex.EncodeToString(p.AuthPublicKey),
		EncryptionPublicKey: hex.EncodeToString(p.EncryptionPublicKey),
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// ReadFile parses an identity file written by WriteFile.
func ReadFile(path string) (Public, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Public{}, err
	}
	var doc fileV1
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Public{}, fmt.Errorf("identity: %w", err)
	}
	if doc.Version != 1 {
		return Public{}, fmt.Errorf("identity: unsupported version %d", doc.Version)
	}
	var p Public
	nodeID, err := hex.DecodeString(doc.NodeID)
	if err != nil || len(nodeID) != len(p.NodeID) {
		return Public{}, errors.New("identity: node_id is not 32 bytes of hex")
	}
	copy(p.NodeID[:], nodeID)
	if p.AuthPublicKey, err = hex.DecodeString(doc.AuthPublicKey); err != nil || len(p.AuthPublicKey) != ed25519.PublicKeySize {
		return Public{}, errors.New("identity: auth_public_key is not 32 bytes of hex")
	}
	if p.EncryptionPublicKey, err = hex.DecodeString(doc.EncryptionPublicKey); err != nil || len(p.EncryptionPublicKey) != 32 {
		return Public{}, errors.New("identity: encryption_public_key is not 32 bytes of hex")
	}
	return p, nil
}
