package session

import (
	"errors"
	"sync"
)

// NewExchangeEngine is the stand-in keygen engine used until a curve
// adapter is wired. On Start it broadcasts one byte and sends another,
// point-to-point, to every other participant. WaitingFor is those
// participants until one of their messages has been received. It never
// touches the Vault.
func NewExchangeEngine(self [32]byte) Engine {
	return &exchange{
		self:  self,
		out:   make(chan Outgoing, 32),
		done:  make(chan Result, 1),
		heard: map[[32]byte]bool{},
	}
}

type exchange struct {
	self    [32]byte
	out     chan Outgoing
	done    chan Result
	mu      sync.Mutex
	heard   map[[32]byte]bool
	others  [][32]byte
	started bool
}

func (e *exchange) Start(p Params) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errAlreadyStarted
	}
	e.started = true
	for _, id := range p.Participants {
		if id == e.self {
			continue
		}
		e.others = append(e.others, id)
	}
	e.mu.Unlock()

	e.out <- Outgoing{Data: []byte{0x01}}
	for _, id := range p.Participants {
		if id == e.self {
			continue
		}
		to := id
		e.out <- Outgoing{To: &to, Data: []byte{0x02}}
	}
	return nil
}

// errAlreadyStarted refuses a second Start on the same engine.
var errAlreadyStarted = errors.New("session: engine already started")

func (e *exchange) Receive(from [32]byte, _ bool, _ []byte) error {
	e.mu.Lock()
	e.heard[from] = true
	e.mu.Unlock()
	return nil
}

func (e *exchange) Out() <-chan Outgoing { return e.out }

func (e *exchange) Done() <-chan Result { return e.done }

func (e *exchange) WaitingFor() [][32]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out [][32]byte
	for _, id := range e.others {
		if !e.heard[id] {
			out = append(out, id)
		}
	}
	return out
}

func (e *exchange) Abort() {}
