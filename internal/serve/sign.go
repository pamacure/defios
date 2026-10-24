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
// proposal and the accept; no Session exists until t Nodes have accepted. The writer validates against the folded Networks
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
	// Proposing is the proposer's accept: its signature on the request is the
	// accept the log sees, so only the local gate opens (ADR 0010). The
	// signing Session starts once t Nodes have accepted.
	if svc := s.sessionLayer(); svc != nil && s.opts.NewSigner != nil {
		if err := svc.AcceptProposed(signed.MsgID); err != nil {
			return SendView{}, err
		}
	}
	return SendView{Seq: pos.Index, Term: pos.Term, MsgID: hex.EncodeToString(signed.MsgID[:])}, nil
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
		if !session.SupportsSigning(sg.Curve) {
			return session.SignInfo{}, session.ErrUnsupported
		}
		return sg, nil
	}
}

// signAnswer is this Node's accept or decline of a Sign Request.
// reply, if set, builds the response body after answer ran.
func (s *server) signAnswer(w http.ResponseWriter, r *http.Request, answer func(*session.Service, context.Context, [32]byte) error, reply func() any) {
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
	if reply != nil {
		writeJSON(w, reply())
		return
	}
	writeJSON(w, struct{}{})
}

// DeclineView is what `sign decline` did.
type DeclineView struct {
	Result session.DeclineResult `json:"result"`
}

func (s *server) signRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sign/accept", func(w http.ResponseWriter, r *http.Request) {
		s.signAnswer(w, r, (*session.Service).AcceptSign, nil)
	})
	mux.HandleFunc("POST /v1/sign/decline", func(w http.ResponseWriter, r *http.Request) {
		var result session.DeclineResult
		s.signAnswer(w, r, func(svc *session.Service, ctx context.Context, id [32]byte) (err error) {
			result, err = svc.DeclineSign(ctx, id)
			return err
		}, func() any { return DeclineView{Result: result} })
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
		sessions, err := signSessions(rows)
		if err != nil {
			writeError(w, err)
			return
		}
		for i := range list {
			list[i].Session = sessions[list[i].MsgID]
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
		sessions, err := signSessions(rows)
		if err != nil {
			writeError(w, err)
			return
		}
		d.Session = sessions[d.MsgID]
		writeJSON(w, d)
	})
}

// signSessions maps each Sign Request's msg id to the signing Session started
// for it.
func signSessions(rows []cosl.Row) (map[string]string, error) {
	out := map[string]string{}
	list, err := session.List(rows)
	if err != nil {
		return nil, err
	}
	for _, sum := range list {
		if _, ok := out[sum.SignRequest]; sum.SignRequest != "" && !ok {
			out[sum.SignRequest] = sum.SessionID
		}
	}
	return out, nil
}

// SignPropose is `sign propose`: commit a sign.request for the Key.
func (c Client) SignPropose(ctx context.Context, keyQuery string, spec signreq.Spec) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/sign/propose", signProposeRequest{Key: keyQuery, Spec: spec}, &v)
	return v, err
}

// SignAccept is `sign accept`: append this Node's sign.accept.
func (c Client) SignAccept(ctx context.Context, msgID string) error {
	return c.do(ctx, "/v1/sign/accept", signInspectRequest{MsgID: msgID}, &struct{}{})
}

// SignDecline is `sign decline`: abort the signing Session, if one has started.
func (c Client) SignDecline(ctx context.Context, msgID string) (DeclineView, error) {
	var v DeclineView
	err := c.do(ctx, "/v1/sign/decline", signInspectRequest{MsgID: msgID}, &v)
	return v, err
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
