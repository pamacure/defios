package node

import (
	"context"

	"defios.com/internal/chainfill"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

// Sign Request failures a front end tells apart, besides ErrNotServed,
// NotReachedError and InvalidError: signreq.ErrUnknown and
// signreq.ErrAmbiguous (a msg_id prefix naming none or two),
// signreq.ErrInvalid and signreq.ErrCurveMismatch (a request the writer
// refuses), signreq.ErrNotSigned (nothing to broadcast yet),
// chainfill.ErrBitcoin, key.ErrUnknown and key.ErrAmbiguous,
// network.ErrUnknown, network.ErrInvalid and network.ErrNoChain (no
// Network of this Node's for a chain), session.ErrAlreadyRefused,
// session.ErrAlreadyAccepted, session.ErrSignerSetFull,
// session.ErrUnsupported, raftx.ErrNoLeader and raftx.ErrNotInserted.

// SignRequest is one Sign Request as this Node holds it: its status, each
// Node's stance with the Handle of the agent that took it (none for a
// person), this Node's own Stance, and its Session and Broadcast Receipt
// count. Hooks are this Node's Hooks' firings on it, as the process
// serving the Group has seen them since it started.
type SignRequest struct {
	signreq.Summary
	Hooks []HookRun
}

// SignRequestDetail is a Sign Request decoded: each transaction by the
// chain it names, its Digests, and its Broadcast Receipts; Hooks as
// SignRequest has them.
type SignRequestDetail struct {
	signreq.Detail
	Hooks []HookRun
}

// HookRun is one firing of a Hook of this Node's on a Sign Request: the
// Hook, the event it fired on, and its Jobs. An agent Job names its Handle
// and, once it starts, the Conversation and chain whose Transcript is its
// run.
type HookRun = serve.HookRun

// JobRun is one Job of a HookRun, and its State.
type JobRun = serve.JobRun

// States of a JobRun.
const (
	JobPending = serve.JobPending
	JobRunning = serve.JobRunning
	JobOK      = serve.JobOK
	JobFailed  = serve.JobFailed
	JobNotRun  = serve.JobNotRun // an earlier Job failed, or serving stopped first
)

// SignSpec is a Sign Request to propose: its reason, deadline, and
// transactions, each naming one of this Node's Networks by alias.
type SignSpec = signreq.Spec

// TxSpec is one transaction of a SignSpec.
type TxSpec = signreq.TxSpec

// Filled is transactions completed from this Node's Networks, and notes of
// what was filled from where.
type Filled = chainfill.Result

// Answered is this Node's accept or refusal: committed, or Queued in its
// Outbox.
type Answered struct{ Queued bool }

// DeclineResult is what declining a request did (session.Decline*).
type DeclineResult = session.DeclineResult

// Broadcast is a broadcast's outcome, one entry per chain; Failed reports
// whether any chain's did not go out.
type Broadcast = serve.BroadcastView

// SignRequestEvent is a Sign Request whose status, stances, Session or
// Broadcast Receipts moved, or whose deadline passed.
type SignRequestEvent struct {
	Group   string
	Request signreq.Summary
}

// HookRunEvent is a HookRun that changed: it fired, or a Job started or
// ended.
type HookRunEvent struct {
	Group string
	Run   HookRun
}

// SignRequests lists groupAlias's Sign Requests, open and past, in log
// order. It needs the Group served: this Node's stance may still wait in
// the Outbox the serving process holds.
func (h *Host) SignRequests(ctx context.Context, groupAlias string) ([]SignRequest, error) {
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return nil, err
	}
	list, err := api.SignList(ctx)
	if err != nil {
		return nil, wrapServed(err)
	}
	runs, err := hookRuns(ctx, api)
	if err != nil {
		return nil, err
	}
	out := make([]SignRequest, len(list))
	for i, s := range list {
		out[i] = SignRequest{Summary: s, Hooks: runs[s.MsgID]}
	}
	return out, nil
}

// InspectSignRequest is the Sign Request of groupAlias whose msg_id starts
// with msgID, decoded. It needs the Group served, as SignRequests does.
func (h *Host) InspectSignRequest(ctx context.Context, groupAlias, msgID string) (SignRequestDetail, error) {
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return SignRequestDetail{}, err
	}
	d, err := api.SignInspect(ctx, msgID)
	if err != nil {
		return SignRequestDetail{}, wrapServed(err)
	}
	runs, err := hookRuns(ctx, api)
	if err != nil {
		return SignRequestDetail{}, err
	}
	return SignRequestDetail{Detail: d, Hooks: runs[d.MsgID]}, nil
}

// hookRuns is api's Hook runs by the Sign Request they fired on.
func hookRuns(ctx context.Context, api servedAPI) (map[string][]HookRun, error) {
	runs, err := api.HookRuns(ctx)
	if err != nil {
		return nil, wrapServed(err)
	}
	out := map[string][]HookRun{}
	for _, r := range runs {
		out[r.Event.SignRequestID] = append(out[r.Event.SignRequestID], r)
	}
	return out, nil
}

// FillTransactions completes txs for the Key keyQuery names from this
// Node's Networks, reading the chains; it writes nothing. groupAlias must
// be served.
func (h *Host) FillTransactions(ctx context.Context, groupAlias, keyQuery string, txs []TxSpec) (Filled, error) {
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Filled{}, err
	}
	v, err := api.SignFill(ctx, keyQuery, txs)
	if err != nil {
		return Filled{}, wrapServed(err)
	}
	return v, nil
}

// ProposeSignRequest proposes that the Key keyQuery names sign spec's
// transactions, in groupAlias, which must be served. Proposing is this
// Node's accept.
func (h *Host) ProposeSignRequest(ctx context.Context, groupAlias, keyQuery string, spec SignSpec) (Sent, error) {
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Sent{}, err
	}
	v, err := api.SignPropose(ctx, keyQuery, spec)
	if err != nil {
		return Sent{}, wrapServed(err)
	}
	return Sent(v), nil
}

// AcceptSignRequest accepts the request msgID names, offering to sign it:
// a Node's first accept or refusal is its stance, and final.
func (h *Host) AcceptSignRequest(ctx context.Context, groupAlias, msgID string) (Answered, error) {
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Answered{}, err
	}
	v, err := api.SignAccept(ctx, msgID)
	if err != nil {
		return Answered{}, wrapServed(err)
	}
	return Answered{Queued: v.Queued}, nil
}

// RefuseSignRequest refuses the request msgID names, saying why: reason
// is required.
func (h *Host) RefuseSignRequest(ctx context.Context, groupAlias, msgID, reason string) (Answered, error) {
	if err := signreq.CheckReason(reason); err != nil {
		return Answered{}, &InvalidError{Err: err}
	}
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Answered{}, err
	}
	v, err := api.SignRefuse(ctx, msgID, reason)
	if err != nil {
		return Answered{}, wrapServed(err)
	}
	return Answered{Queued: v.Queued}, nil
}

// DeclineSignRequest aborts the signing Session of the request msgID
// names, an emergency stop; before one starts it writes nothing.
func (h *Host) DeclineSignRequest(ctx context.Context, groupAlias, msgID string) (DeclineResult, error) {
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return "", err
	}
	v, err := api.SignDecline(ctx, msgID)
	if err != nil {
		return "", wrapServed(err)
	}
	return v.Result, nil
}

// BroadcastSignRequest submits the signed transactions of the request
// msgID names through this Node's Networks and records a Broadcast Receipt
// per chain that went out. A chain that fails is reported in it, not as
// an error.
func (h *Host) BroadcastSignRequest(ctx context.Context, groupAlias, msgID string) (Broadcast, error) {
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Broadcast{}, err
	}
	v, err := api.SignBroadcast(ctx, msgID)
	if err != nil {
		return Broadcast{}, wrapServed(err)
	}
	return v, nil
}
