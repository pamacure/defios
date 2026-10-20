package cosl

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// Author signs this Node's COSL envelopes for one Group (§4.3 step 1).
type Author struct {
	Node    *identity.Node
	GroupID group.ID
}

// Signed is a signed envelope ready for the log.
type Signed struct {
	MsgID [32]byte
	Bytes []byte // canonical signed envelope
}

// Sign builds an envelope of msgType carrying payload, Group-level when
// conversationID is nil, and signs it with this Node's auth key.
func (a Author) Sign(msgType uint32, conversationID []byte, payload proto.Message) (Signed, error) {
	if conversationID != nil && len(conversationID) != 32 {
		return Signed{}, fmt.Errorf("cosl: conversation id is %d bytes, want 32", len(conversationID))
	}
	raw, err := canonical.Marshal(payload)
	if err != nil {
		return Signed{}, fmt.Errorf("cosl: payload: %w", err)
	}
	var msgID [32]byte
	if _, err := rand.Read(msgID[:]); err != nil {
		return Signed{}, fmt.Errorf("cosl: %w", err)
	}
	env := &coslv1.Envelope{
		Version:        1,
		GroupId:        a.GroupID[:],
		MsgType:        msgType,
		Sender:         a.Node.NodeID[:],
		MsgId:          msgID[:],
		ConversationId: conversationID,
		CreatedAt:      time.Now().UnixNano(),
		Payload:        raw,
	}
	b, err := Sign(env, ed25519.NewKeyFromSeed(a.Node.AuthSeed))
	if err != nil {
		return Signed{}, err
	}
	return Signed{MsgID: msgID, Bytes: b}, nil
}

// MembershipJoin is this Node's own membership.join payload (§5.3): its
// pinned keys and advertised names. The FSM requires it self-asserted.
func MembershipJoin(n *identity.Node, displayName, tsnetHostname string) *coslv1.MembershipJoin {
	return &coslv1.MembershipJoin{
		DisplayName:         displayName,
		TsnetHostname:       tsnetHostname,
		EncryptionPublicKey: n.EncryptionPublicKey,
		AuthPublicKey:       n.AuthPublicKey,
	}
}
