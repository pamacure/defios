package serve

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/raftx"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

type signProposeRequest struct {
	Key string `json:"key"`
	signreq.Spec
}

type signInspectRequest struct {
	MsgID string `json:"msg_id"`
}

// signPropose commits a sign.request scoped to the Key's Conversation. The
// proposer's envelope signature is its acceptance, so one append is both the
// proposal and the accept. The writer validates against the folded Networks
// and Keys first, so a bad request fails loudly here (ADR 0009).
func (s *server) signPropose(ctx context.Context, req signProposeRequest) (SendView, error) {
	replica, store, running := s.current()
	rec, held := s.slot.Current()
	if !running || !held {
		return SendView{}, ErrNoReplica
	}
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		return SendView{}, err
	}
	keys, err := key.List(rows)
	if err != nil {
		return SendView{}, err
	}
	k, err := key.Resolve(keys, req.Key)
	if err != nil {
		return SendView{}, err
	}
	nets, err := network.Fold(rows)
	if err != nil {
		return SendView{}, err
	}
	payload, err := signreq.Build(req.Spec, nets)
	if err != nil {
		return SendView{}, err
	}
	if err := signreq.Validate(payload, k, nets); err != nil {
		return SendView{}, err
	}
	conv := key.ConversationID(k.Curve, k.PublicKey)
	author := cosl.Author{Node: s.opts.Node, GroupID: rec.GroupID()}
	signed, err := author.Sign(cosl.TypeSignRequest, conv[:], payload)
	if err != nil {
		return SendView{}, fmt.Errorf("serve: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, raftx.ApplyTimeout)
	defer cancel()
	pos, err := replica.Append(ctx, signed)
	if err != nil {
		return SendView{}, err
	}
	// Proposing is the proposer's accept: the signing Session starts in the
	// same step (ADR 0010). A request that cannot be signed yet (not
	// Ethereum) stays proposed without one.
	if svc := s.sessionLayer(); svc != nil && s.opts.NewSigner != nil {
		if err := waitApplied(ctx, store, signed.MsgID); err != nil {
			return SendView{}, err
		}
		if _, err := svc.StartSign(ctx, signed.MsgID); err != nil {
			if !errors.Is(err, session.ErrBadSignRequest) && !errors.Is(err, session.ErrUnsupported) {
				return SendView{}, err
			}
			s.opts.Logf("serve: %x proposed without a signing session: %v", signed.MsgID[:4], err)
		}
	}
	return SendView{Seq: pos.Index, Term: pos.Term, MsgID: hex.EncodeToString(signed.MsgID[:])}, nil
}

// waitApplied blocks until this replica has applied the sign.request: a
// follower's Append returns on commit, ahead of its own apply.
func waitApplied(ctx context.Context, store *cosl.Store, msgID [32]byte) error {
	for {
		rows, err := store.Log(cosl.Query{})
		if err != nil {
			return err
		}
		if _, err := signreq.Resolve(rows, hex.EncodeToString(msgID[:])); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// signInfoFrom is the Session layer's view of a sign.request, read off the log.
// It reads the store it is given, not the server's current one: the Session
// layer calls it while stopReplica holds the server lock waiting for it.
func signInfoFrom(store *cosl.Store) func([32]byte) (session.SignInfo, error) {
	return func(id [32]byte) (session.SignInfo, error) {
		rows, err := store.Log(cosl.Query{})
		if err != nil {
			return session.SignInfo{}, err
		}
		sg, err := signreq.Lookup(rows, id)
		if errors.Is(err, signreq.ErrUnknown) || errors.Is(err, signreq.ErrInvalid) || errors.Is(err, key.ErrUnknown) {
			return session.SignInfo{}, fmt.Errorf("%w: %v", session.ErrBadSignRequest, err)
		}
		if err != nil {
			return session.SignInfo{}, err
		}
		if sg.Curve != "ecdsa" {
			return session.SignInfo{}, session.ErrUnsupported
		}
		return session.SignInfo{Curve: sg.Curve, PublicKey: sg.PublicKey, T: sg.T, Digests: sg.Digests, Deadline: sg.Deadline}, nil
	}
}

// signAnswer is the local accept or decline of a Sign Request's Session.
func (s *server) signAnswer(w http.ResponseWriter, r *http.Request, answer func(*session.Service, context.Context, [32]byte) error) {
	var req signInspectRequest
	if !s.readLocal(w, r, &req) {
		return
	}
	_, store, running := s.current()
	svc := s.sessionLayer()
	if !running || svc == nil {
		writeError(w, ErrNoReplica)
		return
	}
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		writeError(w, err)
		return
	}
	id, err := signreq.Resolve(rows, req.MsgID)
	if err == nil {
		err = answer(svc, r.Context(), id)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, struct{}{})
}

func (s *server) signRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sign/accept", func(w http.ResponseWriter, r *http.Request) {
		s.signAnswer(w, r, (*session.Service).AcceptSign)
	})
	mux.HandleFunc("POST /v1/sign/decline", func(w http.ResponseWriter, r *http.Request) {
		s.signAnswer(w, r, (*session.Service).DeclineSign)
	})
	mux.HandleFunc("POST /v1/sign/propose", func(w http.ResponseWriter, r *http.Request) {
		var req signProposeRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		v, err := s.signPropose(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, v)
	})
	mux.HandleFunc("POST /v1/sign/list", func(w http.ResponseWriter, r *http.Request) {
		var req struct{}
		if !s.readLocal(w, r, &req) {
			return
		}
		_, store, running := s.current()
		if !running {
			writeError(w, ErrNoReplica)
			return
		}
		rows, err := store.Log(cosl.Query{})
		if err != nil {
			writeError(w, err)
			return
		}
		list, err := signreq.List(rows, time.Now())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, list)
	})
	mux.HandleFunc("POST /v1/sign/inspect", func(w http.ResponseWriter, r *http.Request) {
		var req signInspectRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		_, store, running := s.current()
		if !running {
			writeError(w, ErrNoReplica)
			return
		}
		rows, err := store.Log(cosl.Query{})
		if err != nil {
			writeError(w, err)
			return
		}
		history, err := network.History(rows)
		if err != nil {
			writeError(w, err)
			return
		}
		d, err := signreq.Inspect(rows, req.MsgID, history, time.Now())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, d)
	})
}

// SignPropose is `sign propose`: commit a sign.request for the Key.
func (c Client) SignPropose(ctx context.Context, keyQuery string, spec signreq.Spec) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/sign/propose", signProposeRequest{Key: keyQuery, Spec: spec}, &v)
	return v, err
}

// SignAccept is `sign accept`: this Node's local accept of the signing Session.
func (c Client) SignAccept(ctx context.Context, msgID string) error {
	return c.do(ctx, "/v1/sign/accept", signInspectRequest{MsgID: msgID}, &struct{}{})
}

// SignDecline is `sign decline`: abort the signing Session.
func (c Client) SignDecline(ctx context.Context, msgID string) error {
	return c.do(ctx, "/v1/sign/decline", signInspectRequest{MsgID: msgID}, &struct{}{})
}

// SignList is `sign list`.
func (c Client) SignList(ctx context.Context) ([]signreq.Summary, error) {
	var v []signreq.Summary
	err := c.do(ctx, "/v1/sign/list", struct{}{}, &v)
	return v, err
}

// SignInspect is `sign inspect`.
func (c Client) SignInspect(ctx context.Context, msgID string) (signreq.Detail, error) {
	var v signreq.Detail
	err := c.do(ctx, "/v1/sign/inspect", signInspectRequest{MsgID: msgID}, &v)
	return v, err
}
