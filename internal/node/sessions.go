package node

import (
	"context"
	"strings"

	"defios.com/internal/cosl"
	"defios.com/internal/keyalias"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

// Session failures a front end tells apart, besides ErrNotServed and
// NoReplicaError: session.ErrBadCurve, serve.ErrBadAlias (a Key alias
// invalid or taken here), session.ErrNotKeygen, session.ErrUnknownSession,
// session.ErrNotParticipant and raftx.ErrNotInserted.

// Session is one Session as the replica holds it: a keygen's, or a Sign
// Request's signing.
type Session = session.Summary

// SessionDetail is a Session with its participants and completes.
type SessionDetail = session.Detail

// Keygen is a keygen Session to start: its curve, its threshold and the
// Key alias it proposes to every Node party to the Key, "" for none.
type Keygen struct {
	Curve string // ecdsa or eddsa
	T     int    // min signers; n is the Roster
	Alias string
}

// StartKeygen starts a keygen Session in groupAlias, which must be served;
// this Node accepts it.
func (h *Host) StartKeygen(ctx context.Context, groupAlias string, k Keygen) (Session, error) {
	if k.Curve != "ecdsa" && k.Curve != "eddsa" {
		return Session{}, invalidf("curve must be ecdsa or eddsa")
	}
	if k.T < 1 {
		return Session{}, invalidf("t must be >= 1")
	}
	alias, err := storedAlias(k.Alias)
	if err != nil {
		return Session{}, err
	}
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Session{}, err
	}
	sum, err := api.SessionStart(ctx, k.Curve, k.T, alias)
	return sum, wrapServed(err)
}

// AcceptSession accepts Session id here so this Node's engine may start,
// with alias as its own Key alias for the Key instead of the one proposed,
// unless "".
func (h *Host) AcceptSession(ctx context.Context, groupAlias, id, alias string) error {
	sid, err := parseSessionID(id)
	if err != nil {
		return err
	}
	if alias, err = storedAlias(alias); err != nil {
		return err
	}
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return err
	}
	return wrapServed(api.SessionAccept(ctx, sid, alias))
}

// AbortSession appends session.abort for Session id, with reason.
func (h *Host) AbortSession(ctx context.Context, groupAlias, id, reason string) error {
	sid, err := parseSessionID(id)
	if err != nil {
		return err
	}
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return err
	}
	return wrapServed(api.SessionAbort(ctx, sid, reason))
}

// Sessions lists groupAlias's Sessions in start order, from its replica.
func (h *Host) Sessions(_ context.Context, groupAlias string) ([]Session, error) {
	rows, err := h.sessionRows(groupAlias)
	if err != nil {
		return nil, err
	}
	return session.List(rows, serve.SignInfoOf(rows))
}

// InspectSession is Session id of groupAlias, from its replica.
func (h *Host) InspectSession(_ context.Context, groupAlias, id string) (SessionDetail, error) {
	sid, err := parseSessionID(id)
	if err != nil {
		return SessionDetail{}, err
	}
	rows, err := h.sessionRows(groupAlias)
	if err != nil {
		return SessionDetail{}, err
	}
	return session.Inspect(rows, sid, serve.SignInfoOf(rows))
}

func (h *Host) sessionRows(groupAlias string) ([]cosl.Row, error) {
	store, err := h.replica(groupAlias)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.Log(cosl.Query{MsgTypes: signreq.LookupMsgTypes})
}

func parseSessionID(s string) (session.ID, error) {
	id, err := session.ParseID(strings.ToLower(s))
	if err != nil {
		return session.ID{}, &InvalidError{Err: err}
	}
	return id, nil
}

// storedAlias is a Key alias in stored form, "" for none.
func storedAlias(alias string) (string, error) {
	if alias == "" {
		return "", nil
	}
	stored, err := keyalias.Normalize(alias)
	if err != nil {
		return "", &InvalidError{Err: err}
	}
	return stored, nil
}
