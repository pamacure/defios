// Package session runs a keygen Session over the COSL: it commits
// session.start / session.abort / tss.session and drives a KeygenEngine
// only after this Node's local accept (docs/spec/mvp.md §7).
package session

import (
	"encoding/hex"
	"errors"
)

// Sentinels the CLI and the local API map onto exit codes.
var (
	ErrBadCurve       = errors.New("session: curve must be ecdsa or eddsa")
	ErrUnknownSession = errors.New("session: no such session")
	ErrNotParticipant = errors.New("session: this Node is not a participant")
	ErrClosedSession  = errors.New("session: session is not open")
	ErrNoEngine       = errors.New("session: no engine")
	// ErrUnsupported is a signing Session for a curve or transaction that
	// cannot be signed yet.
	ErrUnsupported = errors.New("session: signing is not supported for this Key")
	// ErrBadSignRequest is a sign.request that is definitively missing or
	// invalid, as opposed to one that could not be read this time.
	ErrBadSignRequest = errors.New("session: bad sign request")
)

// Unavailable is the engine serve uses until a curve adapter is wired.
// Start fails, so a committed Session is aborted instead of minting.
type Unavailable struct{}

func (Unavailable) Start(Params) error                   { return ErrNoEngine }
func (Unavailable) Receive([32]byte, bool, []byte) error { return ErrNoEngine }
func (Unavailable) Out() <-chan Outgoing                 { return nil }
func (Unavailable) Done() <-chan Result                  { return nil }
func (Unavailable) WaitingFor() [][32]byte               { return nil }
func (Unavailable) Abort()                               {}

// ID is a Session's 32-byte identity, minted by the initiator.
type ID [32]byte

// String is the lowercase hex display form.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// ParseID reads the 64-hex-digit display form.
func ParseID(s string) (ID, error) {
	var id ID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, errBadID(s)
	}
	copy(id[:], b)
	return id, nil
}

func errBadID(s string) error {
	return &idError{s}
}

type idError struct{ s string }

func (e *idError) Error() string {
	return "session: " + quote(e.s) + " is not a 32-byte hex Session ID"
}

func quote(s string) string {
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return `"` + s + `"`
}

// Status of a Session on the replica.
const (
	Open     = "open"
	Aborted  = "aborted"
	Complete = "complete"
)

// Engine is the keygen engine seam (docs/spec/mvp.md §7.3). The Session
// layer seals Outgoing when To is set, wraps tss.session, and unwraps on
// receive. Adapters (tss-lib, later Lockness) implement this; the COSL
// does not change.
type Engine interface {
	Start(params Params) error
	Receive(from [32]byte, isBroadcast bool, data []byte) error
	Out() <-chan Outgoing // Data is always plaintext engine bytes
	Done() <-chan Result  // share + canonical public key, or error
	WaitingFor() [][32]byte
	Abort()
}

// Params is one Node's view of a keygen Session.
type Params struct {
	SessionID    [32]byte
	Curve        string // ecdsa | eddsa
	T, N         int    // DeFiOS min-signers, roster size
	Participants [][32]byte
	Nonce        [32]byte // unsealed preimage
	Preparams    []byte   // ECDSA only

	// A signing Session sets these. Participants are then the t signers, T
	// is the Key's t, and N is the Roster size.
	Digests  [][]byte // one message to sign each, in order (ECDSA: a 32-byte hash; EdDSA: the message itself)
	SaveData []byte   // this Node's share of the Key
}

// Outgoing is one engine message. A nil To is a broadcast.
type Outgoing struct {
	To   *[32]byte
	Data []byte
}

// Result is the engine's terminal output. The Session layer writes
// SaveData to the Vault before it appends session.complete.
type Result struct {
	PublicKey []byte
	SaveData  []byte
	// Signatures is a signing Session's output: 65-byte r||s||v, one per Digest.
	Signatures [][]byte
	Err        error
}
