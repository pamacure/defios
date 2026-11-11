package serve

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"defios.com/internal/config"
	"defios.com/internal/key"
	"defios.com/internal/keyalias"
	"defios.com/internal/session"
)

// ErrBadAlias is a Key alias refused because it is invalid or taken here.
var ErrBadAlias = errors.New("serve: key alias refused")

type (
	sessionStartRequest struct {
		Curve string `json:"curve"`
		T     int    `json:"t"`
		// Alias is the Key alias to propose; empty proposes none.
		Alias string `json:"alias,omitempty"`
	}
	sessionIDRequest struct {
		SessionID string `json:"session_id"`
		Reason    string `json:"reason,omitempty"`
	}
	sessionAcceptRequest struct {
		SessionID string `json:"session_id"`
		// Alias is this Node's own Key alias for the Key; empty keeps the
		// proposal.
		Alias string `json:"alias,omitempty"`
	}
	sessionAck struct {
		SessionID string `json:"session_id"`
	}
)

// WaitingKeyAliases are the Key aliases serve holds for a mint, all of one
// Group, the only one this serve runs.
type WaitingKeyAliases struct {
	Group   string   `json:"group"`
	Aliases []string `json:"aliases"`
}

// KeyShare names a Key this Node holds a share of; the share stays in the
// Vault.
type KeyShare struct {
	Curve     string `json:"curve"`
	PublicKey string `json:"public_key"` // hex
}

func (s *server) sessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/key/shares", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.keyShares())
	})
	mux.HandleFunc("GET /v1/key/aliases/waiting", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.waitingKeyAliases())
	})
	mux.HandleFunc("POST /v1/session/start", func(w http.ResponseWriter, r *http.Request) {
		var req sessionStartRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		sum, err := s.sessionStart(r.Context(), req.Curve, req.T, req.Alias)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, sum)
	})
	mux.HandleFunc("POST /v1/session/accept", func(w http.ResponseWriter, r *http.Request) {
		var req sessionAcceptRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		id, err := session.ParseID(req.SessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.sessionAccept(r.Context(), id, req.Alias); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, sessionAck{SessionID: id.String()})
	})
	mux.HandleFunc("POST /v1/session/abort", func(w http.ResponseWriter, r *http.Request) {
		var req sessionIDRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		id, err := session.ParseID(req.SessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.sessionAbort(r.Context(), id, req.Reason); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, sessionAck{SessionID: id.String()})
	})
}

// keyShares is the Keys this Node holds a share of, from the unlocked Vault.
func (s *server) keyShares() []KeyShare {
	if s.opts.Vault == nil {
		return []KeyShare{}
	}
	shares := s.opts.Vault.Shares()
	out := make([]KeyShare, len(shares))
	for i, sh := range shares {
		out[i] = KeyShare{Curve: sh.Curve, PublicKey: hex.EncodeToString(sh.PublicKey)}
	}
	return out
}

func (s *server) waitingKeyAliases() WaitingKeyAliases {
	return WaitingKeyAliases{Group: s.opts.Alias, Aliases: s.holds.all()}
}

// sessionStart starts a keygen Session proposing alias, "" for none.
func (s *server) sessionStart(ctx context.Context, curve string, t int, alias string) (session.Summary, error) {
	svc := s.sessionLayer()
	if svc == nil {
		return session.Summary{}, ErrNoReplica
	}
	stored, err := s.storedProposal(alias)
	if err != nil {
		return session.Summary{}, err
	}
	return svc.Start(ctx, curve, t, stored)
}

// sessionAccept is this Node's local accept of id, with its own Key alias
// unless alias is "".
func (s *server) sessionAccept(ctx context.Context, id session.ID, alias string) error {
	svc := s.sessionLayer()
	if svc == nil {
		return ErrNoReplica
	}
	return s.accept(ctx, svc, id, alias)
}

// sessionAbort appends session.abort for id.
func (s *server) sessionAbort(ctx context.Context, id session.ID, reason string) error {
	svc := s.sessionLayer()
	if svc == nil {
		return ErrNoReplica
	}
	return svc.Abort(ctx, id, reason)
}

// accept is this Node's local accept of id, with its own Key alias for the Key
// unless alias is empty. The alias is held before the Keys are read, so a Key
// minting meanwhile is seen in one or the other: it lets go of its hold only
// once its config.toml names it. A refused alias leaves id unaccepted.
func (s *server) accept(ctx context.Context, svc *session.Service, id session.ID, alias string) error {
	if alias == "" {
		return svc.Accept(ctx, id)
	}
	stored, err := normalizeAlias(alias)
	if err != nil {
		return err
	}
	undo, err := s.holds.hold(id, stored)
	if err != nil {
		return err
	}
	if err := s.refuseAliasOnAKey(stored); err != nil {
		undo()
		return err
	}
	if err := svc.AcceptKeygen(ctx, id); err != nil {
		undo()
		return err
	}
	return nil
}

// storedProposal is alias in stored form if this Node can propose it: valid,
// waiting on no mint, and on no Key of this Group here. Empty proposes none.
func (s *server) storedProposal(alias string) (string, error) {
	if alias == "" {
		return "", nil
	}
	stored, err := normalizeAlias(alias)
	if err != nil {
		return "", err
	}
	if err := s.holds.check(stored); err != nil {
		return "", err
	}
	if err := s.refuseAliasOnAKey(stored); err != nil {
		return "", err
	}
	return stored, nil
}

func normalizeAlias(alias string) (string, error) {
	stored, err := keyalias.Normalize(alias)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadAlias, err)
	}
	return stored, nil
}

// refuseAliasOnAKey fails if alias is on a Key of this Group here. A config.toml
// that did not read fails it, for the alias might be in that file (ADR 0033).
func (s *server) refuseAliasOnAKey(alias string) error {
	_, store, ok := s.current()
	if !ok {
		return ErrNoReplica
	}
	keys, err := key.Read(store)
	if err != nil {
		return err
	}
	holders, err := config.AliasHolders(keys, config.ReadKeyAliases(s.opts.Paths, s.opts.Alias, keys), alias)
	if err != nil {
		return err
	}
	if len(holders) > 0 {
		return fmt.Errorf("%w: key alias %q is already on %s in this Group", ErrBadAlias, alias, holders[0].Ref())
	}
	return nil
}

func (s *server) sessionLayer() *session.Service {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	return s.sessions
}

// WaitingKeyAliases is what `key alias`, which needs no serve, asks one that
// is running: a restart of serve holds none, so ErrNotRunning means none wait.
func (c Client) WaitingKeyAliases(ctx context.Context) (WaitingKeyAliases, error) {
	var w WaitingKeyAliases
	err := c.do(ctx, "/v1/key/aliases/waiting", nil, &w)
	return w, err
}

// SessionStart is `session start`.
func (c Client) SessionStart(ctx context.Context, curve string, t int, alias string) (session.Summary, error) {
	var sum session.Summary
	err := c.do(ctx, "/v1/session/start", sessionStartRequest{Curve: curve, T: t, Alias: alias}, &sum)
	return sum, err
}

// SessionAccept is `session accept`.
func (c Client) SessionAccept(ctx context.Context, id session.ID, alias string) error {
	return c.do(ctx, "/v1/session/accept", sessionAcceptRequest{SessionID: id.String(), Alias: alias}, &sessionAck{})
}

// SessionAbort is `session abort`.
func (c Client) SessionAbort(ctx context.Context, id session.ID, reason string) error {
	return c.do(ctx, "/v1/session/abort", sessionIDRequest{SessionID: id.String(), Reason: reason}, &sessionAck{})
}

// KeyShares is the public keys this Node holds a share of. Save data stays
// in the Vault.
func (c Client) KeyShares(ctx context.Context) ([]KeyShare, error) {
	var refs []KeyShare
	err := c.do(ctx, "/v1/key/shares", nil, &refs)
	return refs, err
}

// The Session and Key verbs in this process, as Client has them over the
// socket. The writing ones need the tailnet up, as the socket's do.

// WaitingKeyAliases is Client.WaitingKeyAliases in this process.
func (r *Running) WaitingKeyAliases(context.Context) (WaitingKeyAliases, error) {
	return r.s.waitingKeyAliases(), nil
}

// KeyShares is Client.KeyShares in this process.
func (r *Running) KeyShares(context.Context) ([]KeyShare, error) { return r.s.keyShares(), nil }

// SessionStart is Client.SessionStart in this process.
func (r *Running) SessionStart(ctx context.Context, curve string, t int, alias string) (session.Summary, error) {
	if err := r.tailnet(); err != nil {
		return session.Summary{}, err
	}
	return r.s.sessionStart(ctx, curve, t, alias)
}

// SessionAccept is Client.SessionAccept in this process.
func (r *Running) SessionAccept(ctx context.Context, id session.ID, alias string) error {
	if err := r.tailnet(); err != nil {
		return err
	}
	return r.s.sessionAccept(ctx, id, alias)
}

// SessionAbort is Client.SessionAbort in this process.
func (r *Running) SessionAbort(ctx context.Context, id session.ID, reason string) error {
	if err := r.tailnet(); err != nil {
		return err
	}
	return r.s.sessionAbort(ctx, id, reason)
}
