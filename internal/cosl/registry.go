// Package cosl is the canonically-ordered signature log: envelope signing,
// the type registry, the append-only sqlite replica, and the FSM that
// verifies committed entries into it (docs/spec/mvp.md §5).
package cosl

import "fmt"

// Registry ids (docs/spec/mvp.md §5.2).
const (
	TypeMembershipJoin  uint32 = 1
	TypeSessionStart    uint32 = 2
	TypeSessionAbort    uint32 = 3
	TypeSessionComplete uint32 = 4
	TypeTssSession      uint32 = 5
	TypeAppFreeform     uint32 = 6
)

var typeNames = map[uint32]string{
	TypeMembershipJoin:  "membership.join",
	TypeSessionStart:    "session.start",
	TypeSessionAbort:    "session.abort",
	TypeSessionComplete: "session.complete",
	TypeTssSession:      "tss.session",
	TypeAppFreeform:     "app.freeform",
}

// TypeName is the registry name for id, or "unknown(<id>)".
func TypeName(id uint32) string {
	if name, ok := typeNames[id]; ok {
		return name
	}
	return fmt.Sprintf("unknown(%d)", id)
}

// TypeByName resolves a registry name to its id.
func TypeByName(name string) (uint32, bool) {
	for id, n := range typeNames {
		if n == name {
			return id, true
		}
	}
	return 0, false
}
