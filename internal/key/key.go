// Package key reads minted Keys off a COSL replica (docs/spec/mvp.md §2.4, §9.3).
// There is no keys table: a Key exists when n session.complete rows agree.
package key

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"defios.com/internal/cosl"
	"defios.com/internal/session"
)

var (
	// ErrUnknown is an inspect query that matches nothing.
	ErrUnknown = errors.New("key: no such key")
	// ErrAmbiguous is a hex prefix that matches more than one Key.
	ErrAmbiguous = errors.New("key: ambiguous key")
)

// Key is one minted threshold public key.
type Key struct {
	SessionID session.ID
	Curve     string
	PublicKey []byte
	T, N      int
}

// Ref is the CLI form curve:hex.
func (k Key) Ref() string { return k.Curve + ":" + hex.EncodeToString(k.PublicKey) }

// ConversationID is SHA-256("defios-conversation-id-v1" || curve || canonical public key).
func ConversationID(curve string, publicKey []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("defios-conversation-id-v1"))
	h.Write([]byte(curve))
	h.Write(publicKey)
	var id [32]byte
	copy(id[:], h.Sum(nil))
	return id
}

// Conversation is the hex Conversation ID.
func (k Key) Conversation() string {
	id := ConversationID(k.Curve, k.PublicKey)
	return hex.EncodeToString(id[:])
}

// MsgTypes are the rows that decide whether a Key is minted: session.start,
// session.complete and session.abort. A reader listing Keys asks the
// replica for these only (cosl.Query.MsgTypes), so talk and round traffic
// are never decoded to find a Key (ADR 0014).
var MsgTypes = []uint32{cosl.TypeSessionStart, cosl.TypeSessionComplete, cosl.TypeSessionAbort}

// Read lists the replica's minted Keys in mint order, reading only MsgTypes.
func Read(store *cosl.Store) ([]Key, error) {
	rows, err := store.Log(cosl.Query{MsgTypes: MsgTypes})
	if err != nil {
		return nil, err
	}
	return List(rows)
}

// List returns minted Keys in replica order. rows may be the whole log or
// only its MsgTypes rows; the answer is the same.
func List(rows []cosl.Row) ([]Key, error) {
	minted, err := session.CompletedSessions(rows)
	if err != nil {
		return nil, err
	}
	out := make([]Key, len(minted))
	for i, m := range minted {
		out[i] = Key{SessionID: m.SessionID, Curve: m.Curve, PublicKey: m.PublicKey, T: m.T, N: m.N}
	}
	return out, nil
}

// Resolve matches query as curve:hex, a unique public-key hex prefix, or a
// Conversation ID hex.
func Resolve(keys []Key, query string) (Key, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return Key{}, ErrUnknown
	}
	if curve, pk, ok := strings.Cut(query, ":"); ok {
		raw, err := hex.DecodeString(pk)
		if err != nil || curve == "" {
			return Key{}, ErrUnknown
		}
		for _, k := range keys {
			if k.Curve == curve && string(k.PublicKey) == string(raw) {
				return k, nil
			}
		}
		return Key{}, ErrUnknown
	}
	var match []Key
	for _, k := range keys {
		pub := hex.EncodeToString(k.PublicKey)
		if strings.HasPrefix(pub, query) || k.Conversation() == query {
			match = append(match, k)
		}
	}
	switch len(match) {
	case 0:
		return Key{}, ErrUnknown
	case 1:
		return match[0], nil
	default:
		return Key{}, ErrAmbiguous
	}
}

// workspacePrefix is how many hex characters of a public key name its
// Conversation's Workspace: short enough to type, and a prefix key inspect
// accepts.
const workspacePrefix = 12

// WorkspaceNames names each Key's Conversation Workspace, by Conversation ID:
// <curve>-<first 12 hex of the public key>, or <curve>-<full public key hex>
// when an earlier Key in keys already holds the short name. keys is in mint
// order, as List returns it from the log, so every Node names alike and a
// name never changes once given (ADR 0013).
func WorkspaceNames(keys []Key) map[[32]byte]string {
	out := map[[32]byte]string{}
	taken := map[string]bool{}
	for _, k := range keys {
		id := ConversationID(k.Curve, k.PublicKey)
		if _, ok := out[id]; ok {
			continue
		}
		pub := hex.EncodeToString(k.PublicKey)
		name := k.Curve + "-" + pub[:min(workspacePrefix, len(pub))]
		if taken[name] {
			name = k.Curve + "-" + pub
		}
		taken[name] = true
		out[id] = name
	}
	return out
}
