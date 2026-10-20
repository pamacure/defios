package serve

import (
	"context"
	"encoding/hex"
	"net/http"

	"defios.com/internal/session"
)

type (
	sessionStartRequest struct {
		Curve string `json:"curve"`
		T     int    `json:"t"`
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
		sum, err := svc.Start(r.Context(), req.Curve, req.T)
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

func (s *server) sessionLayer() *session.Service {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	return s.sessions
}

// SessionStart is `session start`.
func (c Client) SessionStart(ctx context.Context, curve string, t int) (session.Summary, error) {
	var sum session.Summary
	err := c.do(ctx, "/v1/session/start", sessionStartRequest{Curve: curve, T: t}, &sum)
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
