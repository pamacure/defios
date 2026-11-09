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
	sessionAck struct {
		SessionID string `json:"session_id"`
	}
)

type keyShareRef struct {
	Curve     string `json:"curve"`
	PublicKey string `json:"public_key"`
}

func (s *server) sessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/key/shares", func(w http.ResponseWriter, _ *http.Request) {
		if s.opts.Vault == nil {
			writeJSON(w, []keyShareRef{})
			return
		}
		shares := s.opts.Vault.Shares()
		out := make([]keyShareRef, len(shares))
		for i, sh := range shares {
			out[i] = keyShareRef{Curve: sh.Curve, PublicKey: hex.EncodeToString(sh.PublicKey)}
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /v1/session/start", func(w http.ResponseWriter, r *http.Request) {
		var req sessionStartRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		svc := s.sessionLayer()
		if svc == nil {
			writeError(w, ErrNoReplica)
			return
		}
		alias, err := s.storedProposal(req.Alias)
		if err != nil {
			writeError(w, err)
			return
		}
		sum, err := svc.Start(r.Context(), req.Curve, req.T, alias)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, sum)
	})
	mux.HandleFunc("POST /v1/session/accept", func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.sessionID(w, r)
		if !ok {
			return
		}
		svc := s.sessionLayer()
		if svc == nil {
			writeError(w, ErrNoReplica)
			return
		}
		if err := svc.Accept(r.Context(), id); err != nil {
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
		svc := s.sessionLayer()
		if svc == nil {
			writeError(w, ErrNoReplica)
			return
		}
		if err := svc.Abort(r.Context(), id, req.Reason); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, sessionAck{SessionID: id.String()})
	})
}

// sessionID reads a local request that names a Session.
func (s *server) sessionID(w http.ResponseWriter, r *http.Request) (session.ID, bool) {
	var req sessionIDRequest
	if !s.readLocal(w, r, &req) {
		return session.ID{}, false
	}
	id, err := session.ParseID(req.SessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return session.ID{}, false
	}
	return id, true
}

// storedProposal is alias in stored form if this Node can propose it: valid, and
// on no Key of this Group here. A config.toml that did not read fails it, for
// the alias might be in that file (ADR 0033). Empty proposes none.
func (s *server) storedProposal(alias string) (string, error) {
	if alias == "" {
		return "", nil
	}
	stored, err := keyalias.Normalize(alias)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadAlias, err)
	}
	_, store, ok := s.current()
	if !ok {
		return "", ErrNoReplica
	}
	keys, err := key.Read(store)
	if err != nil {
		return "", err
	}
	holders, err := config.AliasHolders(keys, config.ReadKeyAliases(s.opts.Paths, s.opts.Alias, keys), stored)
	if err != nil {
		return "", err
	}
	if len(holders) > 0 {
		return "", fmt.Errorf("%w: key alias %q is already on %s in this Group", ErrBadAlias, stored, holders[0].Ref())
	}
	return stored, nil
}

func (s *server) sessionLayer() *session.Service {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	return s.sessions
}

// SessionStart is `session start`.
func (c Client) SessionStart(ctx context.Context, curve string, t int, alias string) (session.Summary, error) {
	var sum session.Summary
	err := c.do(ctx, "/v1/session/start", sessionStartRequest{Curve: curve, T: t, Alias: alias}, &sum)
	return sum, err
}

// SessionAccept is `session accept`.
func (c Client) SessionAccept(ctx context.Context, id session.ID) error {
	return c.do(ctx, "/v1/session/accept", sessionIDRequest{SessionID: id.String()}, &sessionAck{})
}

// SessionAbort is `session abort`.
func (c Client) SessionAbort(ctx context.Context, id session.ID, reason string) error {
	return c.do(ctx, "/v1/session/abort", sessionIDRequest{SessionID: id.String(), Reason: reason}, &sessionAck{})
}

// KeyShares is the public keys this Node holds a share of. Save data stays
// in the Vault.
func (c Client) KeyShares(ctx context.Context) ([]keyShareRef, error) {
	var refs []keyShareRef
	err := c.do(ctx, "/v1/key/shares", nil, &refs)
	return refs, err
}
