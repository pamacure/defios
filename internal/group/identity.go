package group

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

// ErrBadIdentity is returned by VerifyIdentity for a signature that does
// not check or a node_id that is not derived from the auth public key.
var ErrBadIdentity = errors.New("group: identity does not verify")

var canonical = proto.MarshalOptions{Deterministic: true}

// SignIdentity builds this Node's signed Identity advertisement (§6.1).
func SignIdentity(n *identity.Node, displayName, tsnetHostname string) (*groupv1.Identity, error) {
	id := &groupv1.Identity{
		NodeId:              n.NodeID[:],
		AuthPublicKey:       n.AuthPublicKey,
		EncryptionPublicKey: n.EncryptionPublicKey,
		DisplayName:         displayName,
		TsnetHostname:       tsnetHostname,
	}
	msg, err := canonical.Marshal(id)
	if err != nil {
		return nil, fmt.Errorf("group: %w", err)
	}
	id.Sig = ed25519.Sign(ed25519.NewKeyFromSeed(n.AuthSeed), msg)
	return id, nil
}

// VerifyIdentity checks the signature under the advertised auth public key
// and that node_id == SHA-256("defios-node-id-v1" || auth_public_key), then
// returns the Roster entry it describes.
func VerifyIdentity(id *groupv1.Identity) (RosterNode, error) {
	if len(id.AuthPublicKey) != ed25519.PublicKeySize || len(id.Sig) != ed25519.SignatureSize {
		return RosterNode{}, fmt.Errorf("%w: malformed key or signature", ErrBadIdentity)
	}
	unsigned := proto.Clone(id).(*groupv1.Identity)
	unsigned.Sig = nil
	msg, err := canonical.Marshal(unsigned)
	if err != nil {
		return RosterNode{}, fmt.Errorf("group: %w", err)
	}
	if !ed25519.Verify(id.AuthPublicKey, msg, id.Sig) {
		return RosterNode{}, fmt.Errorf("%w: bad signature", ErrBadIdentity)
	}
	want := identity.NodeID(id.AuthPublicKey)
	var got identity.ID
	if len(id.NodeId) != len(got) {
		return RosterNode{}, fmt.Errorf("%w: node_id is not 32 bytes", ErrBadIdentity)
	}
	copy(got[:], id.NodeId)
	if got != want {
		return RosterNode{}, fmt.Errorf("%w: node_id %s is not derived from the auth public key", ErrBadIdentity, got)
	}
	if len(id.EncryptionPublicKey) != 32 {
		return RosterNode{}, fmt.Errorf("%w: encryption_public_key is not 32 bytes", ErrBadIdentity)
	}
	return RosterNode{
		Public: identity.Public{
			NodeID:              got,
			AuthPublicKey:       ed25519.PublicKey(id.AuthPublicKey),
			EncryptionPublicKey: id.EncryptionPublicKey,
		},
		DisplayName:   id.DisplayName,
		TsnetHostname: id.TsnetHostname,
	}, nil
}
