package serve

import (
	"fmt"
	"sync"

	"defios.com/internal/keyalias"
	"defios.com/internal/session"
)

// aliasHolds are the Key aliases this Node chose with session accept --alias,
// by Session, each waiting on that Session's mint. Memory is enough: a restart
// already aborts the Sessions this Node has accepted (ADR 0033).
type aliasHolds struct {
	mu sync.Mutex
	by map[session.ID]string
}

// hold makes alias the one id waits to mint with, or fails with the Session
// that already waits on it, compared as Key aliases are. A Session keeps the
// alias it first held: accepting again with that one changes nothing, and with
// another is refused. undo lets go of what this call took.
func (h *aliasHolds) hold(id session.ID, alias string) (undo func(), err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if other, ok := h.holderLocked(alias); ok {
		if other != id {
			return nil, waitingError(alias, other)
		}
		return func() {}, nil
	}
	if held, ok := h.by[id]; ok {
		return nil, fmt.Errorf("%w: Session %s is already waiting to mint as %q", ErrBadAlias, id, held)
	}
	if h.by == nil {
		h.by = map[session.ID]string{}
	}
	h.by[id] = alias
	return func() { h.release(id) }, nil
}

// check fails if alias is waiting on a mint.
func (h *aliasHolds) check(alias string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if other, ok := h.holderLocked(alias); ok {
		return waitingError(alias, other)
	}
	return nil
}

func (h *aliasHolds) holderLocked(alias string) (session.ID, bool) {
	for id, held := range h.by {
		if keyalias.Same(held, alias) {
			return id, true
		}
	}
	return session.ID{}, false
}

func waitingError(alias string, id session.ID) error {
	return fmt.Errorf("%w: key alias %q is waiting on Session %s", ErrBadAlias, alias, id)
}

// of is the alias id is waiting to mint with, or "".
func (h *aliasHolds) of(id session.ID) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.by[id]
}

// release lets go of id's alias, if it has one.
func (h *aliasHolds) release(id session.ID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.by, id)
}

// all is every alias waiting on a mint, in no order.
func (h *aliasHolds) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.by))
	for _, alias := range h.by {
		out = append(out, alias)
	}
	return out
}
