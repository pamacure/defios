package serve

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/raftx"
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
	return SendView{Seq: pos.Index, Term: pos.Term, MsgID: hex.EncodeToString(signed.MsgID[:])}, nil
}

func (s *server) signRoutes(mux *http.ServeMux) {
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
