package serve

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/outbox"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	coslv1 "defios.com/proto/cosl/v1"
)

type signProposeRequest struct {
	Key string `json:"key"`
	signreq.Spec
}

type signInspectRequest struct {
	MsgID string `json:"msg_id"`
}

type signRefuseRequest struct {
	MsgID  string `json:"msg_id"`
	Reason string `json:"reason"`
}

// signPropose commits a sign.request scoped to the Key's Conversation. The
// proposer's envelope signature is its acceptance, so one append is both the
// proposal and the accept; no Session exists until t Nodes have accepted.
// The aliases are this Node's Networks for the Key, resolved to the chains
// the request names (ADR 0023). The writer validates first, so a bad request
// fails loudly here (ADR 0009). handle is the agent proposing, empty for a
// person.
func (s *server) signPropose(ctx context.Context, req signProposeRequest, handle string) (SendView, error) {
	ob, store, running := s.writer()
	rec, held := s.slot.Current()
	if !running || !held {
		return SendView{}, ErrNoReplica
	}
	k, conv, nets, err := s.keyNetworks(store, req.Key)
	if err != nil {
		return SendView{}, err
	}
	payload, err := signreq.Build(req.Spec, nets)
	if err != nil {
		return SendView{}, err
	}
	payload.Handle = handle
	if err := signreq.Validate(payload, k); err != nil {
		return SendView{}, err
	}
	author := cosl.Author{Node: s.opts.Node, GroupID: rec.GroupID()}
	signed, err := author.Sign(cosl.TypeSignRequest, conv[:], payload)
	if err != nil {
		return SendView{}, fmt.Errorf("serve: %w", err)
	}
	v, err := write(ctx, ob, signed)
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
	return v, nil
}

// keyNetworks is the Key query names, its Conversation ID, and this Node's
// Networks as that Conversation's settings give them.
func (s *server) keyNetworks(store *cosl.Store, query string) (key.Key, [32]byte, []network.Network, error) {
	keys, err := key.Read(store)
	if err != nil {
		return key.Key{}, [32]byte{}, nil, err
	}
	k, err := key.Resolve(keys, query)
	if err != nil {
		return key.Key{}, [32]byte{}, nil, err
	}
	conv := key.ConversationID(k.Curve, k.PublicKey)
	e, err := s.load(store, conv[:])
	if err != nil {
		return key.Key{}, [32]byte{}, nil, err
	}
	nets, err := e.Networks()
	if err != nil {
		return key.Key{}, [32]byte{}, nil, err
	}
	return k, conv, nets, nil
}

// signInfoFrom is the Session layer's view of a sign.request, read off the log.
// It reads the store it is given, not the server's current one: the Session
// layer calls it while stopReplica holds the server lock waiting for it.
func signInfoFrom(store *cosl.Store) func([32]byte) (session.SignInfo, error) {
	return func(id [32]byte) (session.SignInfo, error) {
		rows, err := store.Log(cosl.Query{MsgTypes: signreq.LookupMsgTypes})
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

// acceptSign is this Node's accept of a Sign Request, taken by the agent
// handle or a person, which it gives only when it has a Network for every
// chain the request names: a Node's Networks are the chains it is willing to
// sign for. A refusal still in the Outbox is already this Node's stance.
func (s *server) acceptSign(svc *session.Service, ctx context.Context, id [32]byte, handle string) error {
	ob, store, running := s.writer()
	if !running {
		return ErrNoReplica
	}
	if err := signreq.CheckHandle(handle); err != nil {
		return err
	}
	if queuedStance(ob, id) == signreq.StanceRefused {
		return session.ErrAlreadyRefused
	}
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeSignRequest}})
	if err != nil {
		return err
	}
	req, conv, err := signreq.Request(rows, id)
	if err != nil {
		return err
	}
	e, err := s.load(store, conv)
	if err != nil {
		return err
	}
	nets, err := e.Networks()
	if err != nil {
		return err
	}
	if err := signreq.CheckChains(req, nets); err != nil {
		return err
	}
	return svc.AcceptSign(ctx, id, handle)
}

// refuseSign is this Node's refusal of a Sign Request, taken by the agent
// handle or a person. A stance still in the Outbox is already this Node's,
// as one on the log is.
func (s *server) refuseSign(svc *session.Service, ctx context.Context, id [32]byte, reason, handle string) error {
	ob, _, running := s.writer()
	if !running {
		return ErrNoReplica
	}
	if err := signreq.CheckReason(reason); err != nil {
		return err
	}
	if err := signreq.CheckHandle(handle); err != nil {
		return err
	}
	switch queuedStance(ob, id) {
	case signreq.StanceRefused:
		return session.ErrAlreadyRefused
	case signreq.StanceAccepted:
		return session.ErrAlreadyAccepted
	}
	return svc.RefuseSign(ctx, id, reason, handle)
}

// queuedStance is this Node's stance on the request id that its replica may
// not show yet, from what it wrote that waits in its Outbox or has committed
// unapplied: the first sign.accept or sign.refuse naming it. Empty if none.
func queuedStance(ob *outbox.Outbox, id [32]byte) string {
	for _, e := range ob.Unapplied() {
		env, err := cosl.Unmarshal(e.Bytes)
		if err != nil || env.MsgType != cosl.TypeSignAccept && env.MsgType != cosl.TypeSignRefuse {
			continue
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			continue
		}
		switch m := msg.(type) {
		case *coslv1.SignAccept:
			if bytes.Equal(m.SignRequestId, id[:]) {
				return signreq.StanceAccepted
			}
		case *coslv1.SignRefuse:
			if bytes.Equal(m.SignRequestId, id[:]) {
				return signreq.StanceRefused
			}
		}
	}
	return ""
}

// stance is this Node's own stance on the request sum, on the log or still
// in its Outbox.
func (s *server) stance(ob *outbox.Outbox, sum signreq.Summary) string {
	me := s.opts.Node.NodeID.String()
	switch {
	case slices.Contains(sum.Accepted, me):
		return signreq.StanceAccepted
	case slices.Contains(sum.Refused, me):
		return signreq.StanceRefused
	}
	if id, err := hex.DecodeString(sum.MsgID); err == nil && len(id) == 32 {
		if st := queuedStance(ob, [32]byte(id)); st != "" {
			return st
		}
	}
	return signreq.StanceUnaddressed
}

// signAnswer is this Node's accept, refusal or decline of a Sign Request.
// reply, if set, builds the response body after answer ran.
func (s *server) signAnswer(w http.ResponseWriter, r *http.Request, answer func(*session.Service, context.Context, [32]byte) error, reply func() any) {
	var req signInspectRequest
	if !s.readLocal(w, r, &req) {
		return
	}
	err := s.resolveSign(req.MsgID, nil, func(svc *session.Service, id [32]byte) error {
		return answer(svc, r.Context(), id)
	})
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

// resolveSign runs answer on the Sign Request whose msg id starts with
// msgID. With conv set, only the requests in that Key's Conversation are
// matched.
func (s *server) resolveSign(msgID string, conv []byte, answer func(*session.Service, [32]byte) error) error {
	_, store, running := s.current()
	if !running {
		return ErrNoReplica
	}
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeSignRequest}})
	if err != nil {
		return err
	}
	if conv != nil {
		rows = slices.DeleteFunc(rows, func(r cosl.Row) bool { return !bytes.Equal(r.ConversationID, conv) })
	}
	id, err := signreq.Resolve(rows, msgID)
	if err != nil {
		return err
	}
	svc := s.sessionLayer()
	if svc == nil {
		return ErrNoReplica
	}
	return answer(svc, id)
}

// AcceptView is what `sign accept` or `sign refuse` did: its sign.accept or
// sign.refuse committed, or is Queued in this Node's Outbox (ADR 0025).
type AcceptView struct {
	Queued bool `json:"queued,omitempty"`
}

// DeclineView is what `sign decline` did.
type DeclineView struct {
	Result session.DeclineResult `json:"result"`
}

func (s *server) signRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sign/accept", func(w http.ResponseWriter, r *http.Request) {
		var v AcceptView
		s.signAnswer(w, r, func(svc *session.Service, ctx context.Context, id [32]byte) error {
			err := s.acceptSign(svc, ctx, id, "")
			if errors.Is(err, session.ErrQueued) {
				v.Queued, err = true, nil
			}
			return err
		}, func() any { return v })
	})
	mux.HandleFunc("POST /v1/sign/refuse", func(w http.ResponseWriter, r *http.Request) {
		var req signRefuseRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		var v AcceptView
		err := s.resolveSign(req.MsgID, nil, func(svc *session.Service, id [32]byte) error {
			err := s.refuseSign(svc, r.Context(), id, req.Reason, "")
			if errors.Is(err, session.ErrQueued) {
				v.Queued, err = true, nil
			}
			return err
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, v)
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
		v, err := s.signPropose(r.Context(), req, "")
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
		list, err := s.signList()
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
		d, err := s.signInspect(req.MsgID, nil)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, d)
	})
}

// signList is every Sign Request, open and past, with its Session and this
// Node's stance: `sign list`.
func (s *server) signList() ([]signreq.Summary, error) {
	_, store, running := s.current()
	if !running {
		return nil, ErrNoReplica
	}
	rows, err := store.Log(cosl.Query{MsgTypes: signreq.MsgTypes})
	if err != nil {
		return nil, err
	}
	list, err := signreq.List(rows, time.Now())
	if err != nil {
		return nil, err
	}
	sessions, err := signSessions(rows)
	if err != nil {
		return nil, err
	}
	ob, _, _ := s.writer()
	for i := range list {
		list[i].Session = sessions[list[i].MsgID]
		list[i].Stance = s.stance(ob, list[i])
	}
	return list, nil
}

// signInspect is the Sign Request whose msg id starts with msgID, decoded,
// with its Session and this Node's stance: `sign inspect`. With conv set,
// only the requests in that Key's Conversation are matched.
func (s *server) signInspect(msgID string, conv []byte) (signreq.Detail, error) {
	_, store, running := s.current()
	if !running {
		return signreq.Detail{}, ErrNoReplica
	}
	rows, err := store.Log(cosl.Query{MsgTypes: signreq.MsgTypes})
	if err != nil {
		return signreq.Detail{}, err
	}
	if conv != nil {
		rows = slices.DeleteFunc(rows, func(r cosl.Row) bool {
			return r.MsgType == cosl.TypeSignRequest && !bytes.Equal(r.ConversationID, conv)
		})
	}
	d, err := signreq.Inspect(rows, msgID, time.Now())
	if err != nil {
		return signreq.Detail{}, err
	}
	sessions, err := signSessions(rows)
	if err != nil {
		return signreq.Detail{}, err
	}
	d.Session = sessions[d.MsgID]
	ob, _, _ := s.writer()
	d.Stance = s.stance(ob, d.Summary)
	return d, nil
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

// SignAccept is `sign accept`: append this Node's sign.accept, or queue it.
func (c Client) SignAccept(ctx context.Context, msgID string) (AcceptView, error) {
	var v AcceptView
	err := c.do(ctx, "/v1/sign/accept", signInspectRequest{MsgID: msgID}, &v)
	return v, err
}

// SignRefuse is `sign refuse`: append this Node's sign.refuse, or queue it.
func (c Client) SignRefuse(ctx context.Context, msgID, reason string) (AcceptView, error) {
	var v AcceptView
	err := c.do(ctx, "/v1/sign/refuse", signRefuseRequest{MsgID: msgID, Reason: reason}, &v)
	return v, err
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
