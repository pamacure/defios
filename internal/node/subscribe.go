package node

import (
	"context"
	"sync"
)

// subscriber queues one Subscribe's events, so a slow front end never
// blocks the Node and never misses one.
type subscriber struct {
	mu    sync.Mutex
	queue []Event
	wake  chan struct{} // cap 1: there is something in queue
}

func (s *subscriber) push(ev Event) {
	s.mu.Lock()
	s.queue = append(s.queue, ev)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscriber) pop() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return Event{}, false
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	return ev, true
}

// Subscribe implements Node.
func (h *Host) Subscribe(ctx context.Context) <-chan Event {
	s := &subscriber{wake: make(chan struct{}, 1)}
	out := make(chan Event)
	h.mu.Lock()
	st := h.status.clone()
	s.push(Event{Status: &st})
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	go func() {
		defer close(out)
		defer func() {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
		}()
		for {
			ev, ok := s.pop()
			if !ok {
				select {
				case <-s.wake:
					continue
				case <-ctx.Done():
					return
				}
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
