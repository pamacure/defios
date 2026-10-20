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
	var err error
	if id.Sig, err = sign(n, id); err != nil {
		return nil, err
	}
	return id, nil
}

// VerifyIdentity checks the signature under the advertised auth public key
// and that node_id == SHA-256("defios-node-id-v1" || auth_public_key), then
// returns the Roster entry it describes.
func VerifyIdentity(id *groupv1.Identity) (RosterNode, error) {
	if len(id.AuthPublicKey) != ed25519.PublicKeySize {
		return RosterNode{}, fmt.Errorf("%w: malformed auth public key", ErrBadIdentity)
	}
	if err := verify(id.AuthPublicKey, id, id.Sig); err != nil {
		return RosterNode{}, fmt.Errorf("%w: %w", ErrBadIdentity, err)
	}
	got, ok := nodeIDFromBytes(id.NodeId)
	if !ok {
		return RosterNode{}, fmt.Errorf("%w: node_id is not 32 bytes", ErrBadIdentity)
	}
	if want := identity.NodeID(id.AuthPublicKey); got != want {
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
