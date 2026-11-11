package node

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"defios.com/internal/chainfill"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

// Formation failures a front end tells apart, besides ErrNotServed and
// NotReachedError: serve.ErrRejected (another Node refused the message),
// serve.ErrTailnetNotUp, group.ErrOneGroup (the Group already has a
// pending or valid Record), group.ErrUnknownGroup (no such proposal here)
// and group.ErrNoGroup.

// ErrNotServed: the operation needs its Group served, and no process
// serves it now.
var ErrNotServed = errors.New("node: the Group is not served")

// ServedGroup, passed as the Group to Discover, Propose, Accept or
// Decline, means whichever Group this Node serves now. It is for front
// ends, like the CLI, whose verbs act on the served Group.
const ServedGroup = ""

// NotReachedError names the Nodes a formation message did not reach, so a
// front end can offer to send it again once they are reachable.
type NotReachedError struct {
	NodeIDs []string
	Err     error
}

func (e *NotReachedError) Error() string { return e.Err.Error() }
func (e *NotReachedError) Unwrap() error { return e.Err }

// notServedError keeps the message of what found the Group not served.
type notServedError struct{ err error }

func (e *notServedError) Error() string        { return e.err.Error() }
func (e *notServedError) Is(target error) bool { return target == ErrNotServed }
func (e *notServedError) Unwrap() error        { return e.err }

// DiscoveredNode is a Node on the served Group's tailnet whose Identity
// verifies.
type DiscoveredNode struct {
	NodeID              string
	DisplayName         string
	Hostname            string
	AuthPublicKey       string // hex
	EncryptionPublicKey string // hex
}

// Formation is a Group after a formation operation.
type Formation struct {
	GroupID string
	Status  string // none | pending | valid | declined
	// Undelivered are the Roster Nodes an accept or decline did not reach:
	// the operation took effect here and should be retried for them.
	Undelivered []string
}

// servedAPI is a serving Node's verbs: *serve.Running in this process,
// serve.Client over the socket of another.
type servedAPI interface {
	Nodes(ctx context.Context) ([]serve.NodeSummary, error)
	GroupCreate(ctx context.Context, nodeIDs []identity.ID) (serve.GroupView, error)
	GroupAccept(ctx context.Context, gid group.ID) (serve.GroupView, error)
	GroupDecline(ctx context.Context, gid group.ID, reason string) (serve.GroupView, error)

	ConversationSendAs(ctx context.Context, text, handle string, depth uint32, conversationID []byte) (serve.SendView, error)
	ConversationCancel(ctx context.Context, conversationID []byte, chain string) (serve.CancelView, error)
	ConversationStream(ctx context.Context, conversationID []byte) (*serve.ChainStream, error)
	OutboxDrop(ctx context.Context, msgID string) (serve.DropView, error)

	SessionStart(ctx context.Context, curve string, t int, alias string) (session.Summary, error)
	SessionAccept(ctx context.Context, id session.ID, alias string) error
	SessionAbort(ctx context.Context, id session.ID, reason string) error
	KeyShares(ctx context.Context) ([]serve.KeyShare, error)
	WaitingKeyAliases(ctx context.Context) (serve.WaitingKeyAliases, error)

	SignList(ctx context.Context) ([]signreq.Summary, error)
	SignInspect(ctx context.Context, msgID string) (signreq.Detail, error)
	SignFill(ctx context.Context, keyQuery string, txs []signreq.TxSpec) (chainfill.Result, error)
	SignPropose(ctx context.Context, keyQuery string, spec signreq.Spec) (serve.SendView, error)
	SignAccept(ctx context.Context, msgID string) (serve.AcceptView, error)
	SignRefuse(ctx context.Context, msgID, reason string) (serve.AcceptView, error)
	SignDecline(ctx context.Context, msgID string) (serve.DeclineView, error)
	SignBroadcast(ctx context.Context, msgID string) (serve.BroadcastView, error)
	HookRuns(ctx context.Context) ([]serve.HookRun, error)
}

var (
	_ servedAPI = (*serve.Running)(nil)
	_ servedAPI = serve.Client{}
)

// reach reaches the process serving groupAlias: this host, or another
// process on the Node's socket.
func (h *Host) reach(ctx context.Context, groupAlias string) (servedAPI, error) {
	if cur := h.serving(); cur != nil {
		if groupAlias != ServedGroup && groupAlias != cur.group {
			return nil, servesOther(groupAlias, cur.group)
		}
		return cur.run, nil
	}
	c := h.socket()
	if groupAlias == ServedGroup {
		return c, nil // a socket nobody serves fails the call itself
	}
	st, err := c.Status(ctx)
	if err != nil {
		return nil, wrapServed(err)
	}
	if st.Alias != groupAlias {
		return nil, servesOther(groupAlias, st.Alias)
	}
	return c, nil
}

// socket reaches the process serving this Node over its socket.
func (h *Host) socket() serve.Client { return serve.Client{Socket: h.p.Socket} }

// serving is what this host serves now; nil when locked.
func (h *Host) serving() *serving {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur
}

func servesOther(want, served string) error {
	return &notServedError{fmt.Errorf("%w: %q (this Node serves %q)", ErrNotServed, want, served)}
}

// wrapServed types the serving process's errors for a front end.
func wrapServed(err error) error {
	var unreachable *serve.UnreachableError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, serve.ErrNotRunning):
		return &notServedError{err}
	case errors.As(err, &unreachable):
		return &NotReachedError{NodeIDs: []string{unreachable.Node.String()}, Err: err}
	}
	return err
}

func formationOf(v serve.GroupView) Formation {
	return Formation{GroupID: v.GroupID, Status: v.Status, Undelivered: v.Undelivered}
}

// Discover lists the Nodes on groupAlias's tailnet, which must be served.
func (h *Host) Discover(ctx context.Context, groupAlias string) ([]DiscoveredNode, error) {
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return nil, err
	}
	return discover(ctx, api)
}

func discover(ctx context.Context, api servedAPI) ([]DiscoveredNode, error) {
	nodes, err := api.Nodes(ctx)
	if err != nil {
		return nil, wrapServed(err)
	}
	out := make([]DiscoveredNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, DiscoveredNode(n))
	}
	return out, nil
}

// Propose proposes a Group of nodeIDs, this Node among them, from
// groupAlias, which must be served and unformed. A Node the proposal did
// not reach is a *NotReachedError, and the proposal is withdrawn, so
// Propose may be called again once it is reachable.
func (h *Host) Propose(ctx context.Context, groupAlias string, nodeIDs []string) (Formation, error) {
	ids := make([]identity.ID, 0, len(nodeIDs))
	for _, s := range nodeIDs {
		id, err := identity.ParseID(strings.ToLower(s))
		if err != nil {
			return Formation{}, &InvalidError{Err: err}
		}
		ids = append(ids, id)
	}
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Formation{}, err
	}
	v, err := api.GroupCreate(ctx, ids)
	if err != nil {
		return Formation{}, wrapServed(err)
	}
	return formationOf(v), nil
}

// Accept accepts the pending proposal for groupID held by groupAlias. The
// Nodes it did not reach are in Undelivered; Accept again to retry them.
func (h *Host) Accept(ctx context.Context, groupAlias, groupID string) (Formation, error) {
	gid, err := group.ParseID(strings.ToLower(groupID))
	if err != nil {
		return Formation{}, &InvalidError{Err: err}
	}
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Formation{}, err
	}
	v, err := api.GroupAccept(ctx, gid)
	if err != nil {
		return Formation{}, wrapServed(err)
	}
	return formationOf(v), nil
}

// Decline declines the pending proposal for groupID held by groupAlias,
// freeing it; reason goes to the other Nodes' logs.
func (h *Host) Decline(ctx context.Context, groupAlias, groupID, reason string) (Formation, error) {
	gid, err := group.ParseID(strings.ToLower(groupID))
	if err != nil {
		return Formation{}, &InvalidError{Err: err}
	}
	api, err := h.reach(ctx, groupAlias)
	if err != nil {
		return Formation{}, err
	}
	v, err := api.GroupDecline(ctx, gid, reason)
	if err != nil {
		return Formation{}, wrapServed(err)
	}
	return formationOf(v), nil
}
