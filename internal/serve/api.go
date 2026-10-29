package serve

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/key"
	"defios.com/internal/model"
	"defios.com/internal/network"
	"defios.com/internal/raftx"
	"defios.com/internal/recap"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

// Status is what the local API reports about this serve.
type Status struct {
	NodeID    string `json:"node_id"`
	Hostname  string `json:"hostname"`
	TailnetUp bool   `json:"tailnet_up"`
	RaftUp    bool   `json:"raft_up"`          // a valid Group's replica is running
	Leader    string `json:"leader,omitempty"` // Node ID of the current Raft leader, if known
}

// ErrTailnetNotUp is returned for tailnet operations before Up completes.
var ErrTailnetNotUp = errors.New("serve: tailnet is still coming up")

// NodeSummary is one discovered, verified Identity.
type NodeSummary struct {
	NodeID              string `json:"node_id"`
	DisplayName         string `json:"display_name"`
	Hostname            string `json:"hostname"`
	AuthPublicKey       string `json:"auth_public_key"`
	EncryptionPublicKey string `json:"encryption_public_key"`
}

// formationHandler is what other Nodes reach on :4634.
func (s *server) formationHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/identity", func(w http.ResponseWriter, r *http.Request) {
		raw, err := proto.Marshal(s.self)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(raw)
	})
	s.formationRoutes(mux)
	return mux
}

// GroupView is the local API's report of this Node's Group after a
// formation verb. Undelivered lists Roster Nodes a message did not reach.
type GroupView struct {
	GroupID     string   `json:"group_id,omitempty"`
	Status      string   `json:"status"` // none | pending | valid | declined
	Undelivered []string `json:"undelivered,omitempty"`
}

// Local formation request bodies.
type (
	createRequest struct {
		NodeIDs []string `json:"node_ids"`
	}
	acceptRequest struct {
		GroupID string `json:"group_id"`
	}
	declineRequest struct {
		GroupID string `json:"group_id"`
		Reason  string `json:"reason"`
	}
)

// localHandler is what the CLI reaches over the unix socket. Framing is
// HTTP+JSON; it is this Node's private API, not a public one (§3.5).
func (s *server) localHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		st := Status{
			NodeID:    s.opts.Node.NodeID.String(),
			Hostname:  s.opts.Config.Hostname,
			TailnetUp: s.tailnetUp.Load(),
		}
		if replica, _, ok := s.current(); ok {
			st.RaftUp = true
			if leader, ok := replica.Leader(); ok {
				st.Leader = leader.String()
			}
		}
		writeJSON(w, st)
	})
	mux.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		if !s.tailnetUp.Load() {
			writeError(w, ErrTailnetNotUp)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		nodes, err := s.discover(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out := make([]NodeSummary, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, NodeSummary{
				NodeID:              n.NodeID.String(),
				DisplayName:         n.DisplayName,
				Hostname:            n.TsnetHostname,
				AuthPublicKey:       hex.EncodeToString(n.AuthPublicKey),
				EncryptionPublicKey: hex.EncodeToString(n.EncryptionPublicKey),
			})
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /v1/group/create", func(w http.ResponseWriter, r *http.Request) {
		var req createRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		ids := make([]identity.ID, 0, len(req.NodeIDs))
		for _, h := range req.NodeIDs {
			id, err := identity.ParseID(h)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			ids = append(ids, id)
		}
		v, err := s.groupCreate(r.Context(), ids)
		s.answer(w, v, err)
	})
	mux.HandleFunc("POST /v1/group/accept", func(w http.ResponseWriter, r *http.Request) {
		var req acceptRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		gid, err := group.ParseID(req.GroupID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		v, err := s.groupAccept(r.Context(), gid)
		s.answer(w, v, err)
	})
	mux.HandleFunc("POST /v1/group/decline", func(w http.ResponseWriter, r *http.Request) {
		var req declineRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		gid, err := group.ParseID(req.GroupID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		v, err := s.groupDecline(r.Context(), gid, req.Reason)
		s.answer(w, v, err)
	})
	s.conversationRoutes(mux)
	s.chainRoutes(mux)
	s.networkRoutes(mux)
	s.modelRoutes(mux)
	s.signRoutes(mux)
	s.broadcastRoutes(mux)
	s.sessionRoutes(mux)
	s.snapshotRoutes(mux)
	s.usageRoutes(mux)
	return mux
}

// localMaxBytes bounds a local request body: a maximal app.freeform text
// (§5.2) survives JSON escaping with room to spare.
const localMaxBytes = 512 << 10

// readLocal decodes a local JSON request; writing verbs also need the
// tailnet, so they answer 503 until it is up.
func (s *server) readLocal(w http.ResponseWriter, r *http.Request, v any) bool {
	if !s.tailnetUp.Load() {
		writeError(w, ErrTailnetNotUp)
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, localMaxBytes)).Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// errorKindHeader names the error class on a non-200 response so Client
// can hand the CLI the same sentinel the server saw.
const errorKindHeader = "X-DeFiOS-Error"

// errorKinds pairs each sentinel either HTTP face can report with its wire
// kind and status. The formation face (:4634) uses the status alone; the
// local face also sends the kind. First match wins.
var errorKinds = []struct {
	err    error
	kind   string
	status int
}{
	{ErrRejected, "rejected", http.StatusUnprocessableEntity},
	{group.ErrOneGroup, "one-group", http.StatusConflict},
	{group.ErrAlreadyValid, "already-valid", http.StatusConflict},
	{group.ErrUnknownGroup, "unknown-group", http.StatusNotFound},
	{group.ErrNoGroup, "no-group", http.StatusNotFound},
	{group.ErrBadProposal, "bad-proposal", http.StatusBadRequest},
	{group.ErrBadAccept, "bad-accept", http.StatusBadRequest},
	{group.ErrBadDecline, "bad-decline", http.StatusBadRequest},
	{ErrTailnetNotUp, "tailnet-down", http.StatusServiceUnavailable},
	{ErrNoReplica, "no-replica", http.StatusConflict},
	{ErrNoConversation, "no-conversation", http.StatusUnprocessableEntity},
	{ErrBadHandle, "bad-handle", http.StatusBadRequest},
	{ErrNothingToRetry, "nothing-to-retry", http.StatusConflict},
	{ErrChainBusy, "chain-busy", http.StatusConflict},
	{ErrNotAChain, "not-a-chain", http.StatusBadRequest},
	{network.ErrInvalid, "bad-network", http.StatusBadRequest},
	{network.ErrExists, "network-exists", http.StatusConflict},
	{network.ErrUnknown, "no-network", http.StatusNotFound},
	{model.ErrInvalid, "bad-model", http.StatusBadRequest},
	{model.ErrExists, "model-exists", http.StatusConflict},
	{model.ErrUnknown, "no-model", http.StatusNotFound},
	{signreq.ErrInvalid, "bad-sign-request", http.StatusBadRequest},
	{signreq.ErrCurveMismatch, "curve-mismatch", http.StatusUnprocessableEntity},
	{signreq.ErrUnknown, "no-sign-request", http.StatusNotFound},
	{signreq.ErrNotSigned, "not-signed", http.StatusConflict},
	{signreq.ErrAmbiguous, "ambiguous-sign-request", http.StatusConflict},
	{key.ErrUnknown, "no-key", http.StatusNotFound},
	{key.ErrAmbiguous, "ambiguous-key", http.StatusConflict},
	{cosl.ErrBadText, "bad-text", http.StatusBadRequest},
	{recap.ErrStale, "stale-recap", http.StatusConflict},
	{recap.ErrBadBase, "bad-recap-base", http.StatusBadRequest},
	{session.ErrBadCurve, "bad-curve", http.StatusBadRequest},
	{session.ErrUnknownSession, "unknown-session", http.StatusNotFound},
	{session.ErrNotParticipant, "not-participant", http.StatusForbidden},
	{session.ErrSignerSetFull, "signer-set-full", http.StatusConflict},
	{session.ErrUnsupported, "signing-unsupported", http.StatusUnprocessableEntity},
	{session.ErrClosedSession, "closed-session", http.StatusConflict},
	{raftx.ErrNoLeader, "no-leader", http.StatusServiceUnavailable},
	{raftx.ErrNotInserted, "not-inserted", http.StatusUnprocessableEntity},
}

// writeError answers err with its kind and status.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var req *requestError
	var unreachable *unreachableError
	switch {
	case errors.As(err, &req):
		status = http.StatusBadRequest
	case errors.As(err, &unreachable):
		status = http.StatusBadGateway
	}
	for _, e := range errorKinds {
		if errors.Is(err, e.err) {
			w.Header().Set(errorKindHeader, e.kind)
			status = e.status
			break
		}
	}
	http.Error(w, err.Error(), status)
}

// answer writes a GroupView, or the error.
func (s *server) answer(w http.ResponseWriter, v GroupView, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, v)
}

// localError is Client's inverse of writeError: the server's message,
// tagged with the sentinel its kind names.
func localError(path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	msg := strings.TrimSpace(string(body))
	kind := resp.Header.Get(errorKindHeader)
	for _, e := range errorKinds {
		if e.kind == kind {
			return &kindError{sentinel: e.err, msg: msg}
		}
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		return ErrTailnetNotUp
	}
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("serve: %s: %s", path, msg)
}

// kindError is a server-side error reconstituted on the client.
type kindError struct {
	sentinel error
	msg      string
}

func (e *kindError) Error() string { return e.msg }
func (e *kindError) Unwrap() error { return e.sentinel }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ErrNotRunning means no serve answers on the socket.
var ErrNotRunning = errors.New("serve is not running")

// Client is the CLI's side of the unix socket.
type Client struct {
	Socket string
}

// IsUp reports whether a serve answers on socket.
func IsUp(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (c Client) http() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second, // bounds discovery; a wedged serve must not hang the CLI
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", c.Socket)
			},
		},
	}
}

// do sends a GET (body nil) or a JSON POST and decodes the JSON reply.
func (c Client) do(ctx context.Context, path string, body, out any) error {
	method, payload := http.MethodGet, io.Reader(nil)
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		method, payload = http.MethodPost, bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://serve"+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return fmt.Errorf("%w (no socket at %s)", ErrNotRunning, c.Socket)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return localError(path, resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Status asks the running serve who it is.
func (c Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.do(ctx, "/v1/status", nil, &st)
	return st, err
}

// Nodes asks the running serve to discover tailnet peers and return the
// Node Identities that verify.
func (c Client) Nodes(ctx context.Context) ([]NodeSummary, error) {
	var nodes []NodeSummary
	err := c.do(ctx, "/v1/nodes", nil, &nodes)
	return nodes, err
}

// GroupCreate is `group create`: propose a Group of nodeIDs (this Node
// included) to the others.
func (c Client) GroupCreate(ctx context.Context, nodeIDs []identity.ID) (GroupView, error) {
	req := createRequest{NodeIDs: make([]string, len(nodeIDs))}
	for i, id := range nodeIDs {
		req.NodeIDs[i] = id.String()
	}
	var v GroupView
	err := c.do(ctx, "/v1/group/create", req, &v)
	return v, err
}

// GroupAccept is `group accept`.
func (c Client) GroupAccept(ctx context.Context, gid group.ID) (GroupView, error) {
	var v GroupView
	err := c.do(ctx, "/v1/group/accept", acceptRequest{GroupID: gid.String()}, &v)
	return v, err
}

// GroupDecline is `group decline`.
func (c Client) GroupDecline(ctx context.Context, gid group.ID, reason string) (GroupView, error) {
	var v GroupView
	err := c.do(ctx, "/v1/group/decline", declineRequest{GroupID: gid.String(), Reason: reason}, &v)
	return v, err
}
