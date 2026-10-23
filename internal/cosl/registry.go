// Package cosl is the canonically-ordered signature log: envelope signing,
// the type registry, the append-only sqlite replica, and the FSM that
// verifies committed entries into it (docs/spec/mvp.md §5).
package cosl

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	coslv1 "defios.com/proto/cosl/v1"
)

// Registry ids (docs/spec/mvp.md §5.2).
const (
	TypeMembershipJoin  uint32 = 1
	TypeSessionStart    uint32 = 2
	TypeSessionAbort    uint32 = 3
	TypeSessionComplete uint32 = 4
	TypeTssSession      uint32 = 5
	TypeAppFreeform     uint32 = 6
	TypeSignRequest     uint32 = 7
	TypeNetworkCreate   uint32 = 8
	TypeNetworkUpdate   uint32 = 9
	TypeNetworkRemove   uint32 = 10
	TypeSignAccept      uint32 = 11
)

// kind is one registry entry: its wire name and its payload schema.
type kind struct {
	name string
	new  func() proto.Message
}

// registry is the single table every type lookup reads. Adding a kind is
// one line here plus a Summary case.
var registry = map[uint32]kind{
	TypeMembershipJoin:  {"membership.join", func() proto.Message { return &coslv1.MembershipJoin{} }},
	TypeSessionStart:    {"session.start", func() proto.Message { return &coslv1.SessionStart{} }},
	TypeSessionAbort:    {"session.abort", func() proto.Message { return &coslv1.SessionAbort{} }},
	TypeSessionComplete: {"session.complete", func() proto.Message { return &coslv1.SessionComplete{} }},
	TypeTssSession:      {"tss.session", func() proto.Message { return &coslv1.TssSession{} }},
	TypeAppFreeform:     {"app.freeform", func() proto.Message { return &coslv1.AppFreeform{} }},
	TypeSignRequest:     {"sign.request", func() proto.Message { return &coslv1.SignRequest{} }},
	TypeNetworkCreate:   {"network.create", func() proto.Message { return &coslv1.NetworkConfig{} }},
	TypeNetworkUpdate:   {"network.update", func() proto.Message { return &coslv1.NetworkConfig{} }},
	TypeNetworkRemove:   {"network.remove", func() proto.Message { return &coslv1.NetworkRemove{} }},
	TypeSignAccept:      {"sign.accept", func() proto.Message { return &coslv1.SignAccept{} }},
}

// ErrUnknownType is returned by DecodePayload for ids outside the registry.
var ErrUnknownType = errors.New("cosl: unknown msg_type")

// TypeName is the registry name for id, or "unknown(<id>)".
func TypeName(id uint32) string {
	if k, ok := registry[id]; ok {
		return k.name
	}
	return fmt.Sprintf("unknown(%d)", id)
}

// TypeByName resolves a registry name to its id.
func TypeByName(name string) (uint32, bool) {
	for id, k := range registry {
		if k.name == name {
			return id, true
		}
	}
	return 0, false
}

// DecodePayload parses payload as the registry message for msgType.
func DecodePayload(msgType uint32, payload []byte) (proto.Message, error) {
	k, ok := registry[msgType]
	if !ok {
		return nil, ErrUnknownType
	}
	msg := k.new()
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil, fmt.Errorf("cosl: %s payload: %w", k.name, err)
	}
	return msg, nil
}
