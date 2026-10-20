package identity_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"defios.com/internal/identity"
)

func TestNodeIDIsDomainSeparatedSHA256OfAuthPublicKey(t *testing.T) {
	pub := bytes.Repeat([]byte{0xab}, ed25519.PublicKeySize)
	got := identity.NodeID(pub)

	h := sha256.New()
	h.Write([]byte("defios-node-id-v1"))
	h.Write(pub)
	if want := h.Sum(nil); !bytes.Equal(got[:], want) {
		t.Fatalf("NodeID = %x, want %x", got, want)
	}
}

func TestNodeIDKnownVector(t *testing.T) {
	// All-zero public key; vector computed independently:
	//   { printf 'defios-node-id-v1'; head -c 32 /dev/zero; } | sha256sum
	pub := make([]byte, ed25519.PublicKeySize)
	got := identity.NodeID(pub)
	const want = "4784e5e8d44fbd97e85e08cc76a67f41a6d1b35562ba7e5e9e0be16d214c7dc1"
	if hex.EncodeToString(got[:]) != want {
		t.Fatalf("NodeID = %x, want %s", got, want)
	}
}

func TestSameSeedReproducesSameNode(t *testing.T) {
	seed := bytes.Repeat([]byte{0x01}, ed25519.SeedSize)
	a, err := identity.FromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := identity.FromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if a.NodeID != b.NodeID {
		t.Fatalf("node ids differ: %x vs %x", a.NodeID, b.NodeID)
	}
	if !bytes.Equal(a.AuthPublicKey, b.AuthPublicKey) {
		t.Fatal("auth public keys differ")
	}
}

func TestFromSeedRejectsWrongLength(t *testing.T) {
	if _, err := identity.FromSeed(make([]byte, 31)); err == nil {
		t.Fatal("expected error for 31-byte seed")
	}
}

func TestGenerateMintsDistinctNodes(t *testing.T) {
	a, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if a.NodeID == b.NodeID {
		t.Fatal("two Generate calls produced the same Node ID")
	}
	if len(a.EncryptionPrivateKey) != 32 || len(a.EncryptionPublicKey) != 32 {
		t.Fatalf("encryption key sizes %d/%d", len(a.EncryptionPrivateKey), len(a.EncryptionPublicKey))
	}
}

func TestIdentityFileRoundTripAndShape(t *testing.T) {
	n, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity")
	if err := identity.WriteFile(path, n.Public); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("identity is not JSON: %v", err)
	}
	if doc["version"] != float64(1) {
		t.Fatalf("version = %v, want 1", doc["version"])
	}
	for _, k := range []string{"node_id", "auth_public_key", "encryption_public_key"} {
		s, ok := doc[k].(string)
		if !ok {
			t.Fatalf("%s missing or not a string", k)
		}
		if _, err := hex.DecodeString(s); err != nil || len(s) != 64 {
			t.Fatalf("%s = %q, want 64 lowercase hex chars", k, s)
		}
	}

	back, err := identity.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.NodeID != n.NodeID || !bytes.Equal(back.AuthPublicKey, n.AuthPublicKey) || !bytes.Equal(back.EncryptionPublicKey, n.EncryptionPublicKey) {
		t.Fatal("identity round trip lost data")
	}
}

func TestReadFileRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(path, []byte(`{"version":2,"node_id":"00","auth_public_key":"00","encryption_public_key":"00"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.ReadFile(path); err == nil {
		t.Fatal("expected error for version 2")
	}
}
