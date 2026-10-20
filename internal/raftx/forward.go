package raftx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	raftv1 "defios.com/proto/raft/v1"
)

// Forward gate (§4.3 step 2): a follower POSTs ForwardRequest to the
// leader's :4633; the leader Applies and answers ForwardResponse. HTTP
// framing matches the formation port; the body is the §4.3 protobuf.
const (
	forwardPath     = "/v1/forward"
	appliedPath     = "/v1/applied" // GET: the leader's applied index, decimal
	forwardMaxBytes = 1 << 20
	forwardTimeout  = ApplyTimeout + 5*time.Second
)

// statusNotLeader tells the forwarder to look the leader up again;
// statusTransient tells it the leader could not apply right now (queue
// full, shutting down). Both mean retry. Only a committed-but-rejected
// outcome (ErrNotInserted) travels in the response body.
const (
	statusNotLeader = http.StatusConflict
	statusTransient = http.StatusServiceUnavailable
)

type forwardGate struct {
	replica *Replica
	srv     *http.Server
	client  *http.Client
}

func newForwardGate(r *Replica) (*forwardGate, error) {
	l, err := r.opts.Transport.Listen("tcp", fmt.Sprintf(":%d", ForwardPort))
	if err != nil {
		return nil, fmt.Errorf("raftx: listen :%d: %w", ForwardPort, err)
	}
	g := &forwardGate{replica: r}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+forwardPath, g.handle)
	mux.HandleFunc("GET "+appliedPath, func(w http.ResponseWriter, _ *http.Request) {
		if g.replica.raft.State() != raft.Leader {
			http.Error(w, raft.ErrNotLeader.Error(), statusNotLeader)
			return
		}
		fmt.Fprint(w, g.replica.raft.AppliedIndex())
	})
	g.srv = &http.Server{Handler: mux}
	g.client = &http.Client{
		Timeout: forwardTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return r.opts.Transport.Dial(ctx, network, addr)
			},
		},
	}
	go g.srv.Serve(l)
	return g, nil
}

func (g *forwardGate) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	g.srv.Shutdown(ctx)
	g.client.CloseIdleConnections()
}

// handle is the leader's side.
func (g *forwardGate) handle(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, forwardMaxBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req raftv1.ForwardRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msgID, err := msgIDOf(req.Envelope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if g.replica.raft.State() != raft.Leader {
		http.Error(w, raft.ErrNotLeader.Error(), statusNotLeader)
		return
	}
	pos, err := g.replica.apply(r.Context(), cosl.Signed{MsgID: msgID, Bytes: req.Envelope})
	switch {
	case err == nil:
	case errors.Is(err, errRetry):
		http.Error(w, raft.ErrNotLeader.Error(), statusNotLeader)
		return
	case errors.Is(err, ErrNotInserted):
	default:
		http.Error(w, err.Error(), statusTransient)
		return
	}
	resp := &raftv1.ForwardResponse{Index: pos.Index, Term: pos.Term}
	if err != nil {
		resp.Error = err.Error()
	}
	out, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Write(out)
}

// send is the follower's side: deliver s to the leader's gate.
func (g *forwardGate) send(ctx context.Context, leader group.RosterNode, s cosl.Signed) (Position, error) {
	body, err := proto.Marshal(&raftv1.ForwardRequest{Envelope: s.Bytes})
	if err != nil {
		return Position{}, fmt.Errorf("raftx: forward: %w", err)
	}
	url := "http://" + net.JoinHostPort(leader.TsnetHostname, fmt.Sprint(ForwardPort)) + forwardPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Position{}, fmt.Errorf("raftx: forward: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	httpResp, err := g.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Position{}, ErrNoLeader
		}
		return Position{}, errRetry // leader unreachable right now
	}
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4<<10))
	switch httpResp.StatusCode {
	case http.StatusOK:
	case statusNotLeader, statusTransient:
		return Position{}, errRetry
	default:
		return Position{}, fmt.Errorf("raftx: forward: %s: %s", httpResp.Status, bytes.TrimSpace(raw))
	}
	var resp raftv1.ForwardResponse
	if err := proto.Unmarshal(raw, &resp); err != nil {
		return Position{}, fmt.Errorf("raftx: forward: %w", err)
	}
	pos := Position{Index: resp.Index, Term: resp.Term}
	if resp.Error != "" {
		return pos, &notInsertedError{msg: resp.Error}
	}
	return pos, nil
}

// applied asks the leader how far it has applied.
func (g *forwardGate) applied(ctx context.Context, leader group.RosterNode) (uint64, error) {
	url := "http://" + net.JoinHostPort(leader.TsnetHostname, fmt.Sprint(ForwardPort)) + appliedPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("raftx: applied: %w", err)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, errRetry
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK {
		return 0, errRetry
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("raftx: applied: %w", err)
	}
	return n, nil
}

// notInsertedError is the leader's ErrNotInserted as the forwarder sees
// it: the leader's message, the same sentinel.
type notInsertedError struct{ msg string }

func (e *notInsertedError) Error() string   { return e.msg }
func (e *notInsertedError) Is(t error) bool { return t == ErrNotInserted }

// msgIDOf reads the msg_id from signed envelope bytes without verifying;
// the FSM verifies on apply.
func msgIDOf(envelope []byte) ([32]byte, error) {
	var id [32]byte
	env, err := cosl.Unmarshal(envelope)
	if err != nil {
		return id, err
	}
	if len(env.MsgId) != len(id) {
		return id, errors.New("raftx: msg_id is not 32 bytes")
	}
	copy(id[:], env.MsgId)
	return id, nil
}
