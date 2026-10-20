package group_test

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

func TestSignedIdentityVerifiesAndYieldsRosterNode(t *testing.T) {
	n, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	id, err := group.SignIdentity(n, "laptop", "defios-laptop")
	if err != nil {
		t.Fatal(err)
	}
	if len(id.Sig) != ed25519.SignatureSize {
		t.Fatalf("sig is %d bytes", len(id.Sig))
	}
	raw, _ := proto.Marshal(id)
	back := new(groupv1.Identity)
	if err := proto.Unmarshal(raw, back); err != nil {
		t.Fatal(err)
	}
	node, err := group.VerifyIdentity(back)
	if err != nil {
		t.Fatalf("VerifyIdentity: %v", err)
	}
	if node.NodeID != n.NodeID || !bytes.Equal(node.AuthPublicKey, n.AuthPublicKey) || !bytes.Equal(node.EncryptionPublicKey, n.EncryptionPublicKey) {
		t.Fatal("keys changed in transit")
	}
	if node.DisplayName != "laptop" || node.TsnetHostname != "defios-laptop" {
		t.Fatalf("names: %+v", node)
	}
}

func TestVerifyIdentityRejectsTamperAndWrongKey(t *testing.T) {
	n, _ := identity.Generate()
	id, _ := group.SignIdentity(n, "laptop", "h")
	id.DisplayName = "evil"
	if _, err := group.VerifyIdentity(id); !errors.Is(err, group.ErrBadIdentity) {
		t.Fatalf("tampered display name: err = %v", err)
	}
}

// §11.3: a Node ID advertised with a non-matching auth public key is
// rejected at Identity fetch.
func TestVerifyIdentityRejectsNodeIDNotDerivedFromAuthKey(t *testing.T) {
	a, _ := identity.Generate()
	b, _ := identity.Generate()
	// b signs honestly but claims a's Node ID.
	id, _ := group.SignIdentity(b, "imposter", "h")
	id.NodeId = a.NodeID[:]
	id.Sig = nil
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(id)
	id.Sig = ed25519.Sign(ed25519.NewKeyFromSeed(b.AuthSeed), raw)
	// Signature is valid under b's key, so only the derivation check can
	// catch this.
	if _, err := group.VerifyIdentity(id); !errors.Is(err, group.ErrBadIdentity) {
		t.Fatalf("node_id mismatch: err = %v", err)
	}
}
