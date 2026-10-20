package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

// Formation over tsnet :4634 (§6.2). Each POST carries one protobuf; the
// receiver verifies it through its Slot and answers 200 when held, else
// the status errorKinds gives the refusal.
const (
	formationPostTimeout = 10 * time.Second
	formationMaxBytes    = 256 << 10
)

// ErrRejected means another Node refused a formation message: a protocol
// reject, as opposed to not reaching it at all.
var ErrRejected = errors.New("serve: another Node refused the formation message")

// rejectError carries the refusing Node's status and reason.
type rejectError struct {
	node   identity.ID
	status int
	reason string
}

func (e *rejectError) Error() string {
	return fmt.Sprintf("serve: node %s refused (%d): %s", e.node.String()[:8], e.status, e.reason)
}
func (e *rejectError) Is(target error) bool { return target == ErrRejected }

// formationRoutes adds the POST endpoints to the :4634 mux.
func (s *server) formationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/group/propose", func(w http.ResponseWriter, r *http.Request) {
		var p groupv1.CreateProposal
		if !readProto(w, r, &p) {
			return
		}
		prop, err := s.slot.ReceiveProposal(&p)
		if err != nil {
			s.opts.Logf("serve: proposal refused: %v", err)
			writeError(w, err)
			return
		}
		s.opts.Logf("serve: holding proposal for Group %s from %s (pending)", prop.GroupID, prop.Initiator.String()[:8])
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/group/accept", func(w http.ResponseWriter, r *http.Request) {
		var a groupv1.CreateAccept
		if !readProto(w, r, &a) {
			return
		}
		who, err := s.slot.ReceiveAccept(&a)
		if err != nil {
			s.opts.Logf("serve: accept refused: %v", err)
			writeError(w, err)
			return
		}
		s.logStatus("accept from " + who.String()[:8])
		w.WriteHeader(http.StatusOK)
		s.onValid()
	})
	mux.HandleFunc("POST /v1/group/decline", func(w http.ResponseWriter, r *http.Request) {
		var d groupv1.CreateDecline
		if !readProto(w, r, &d) {
			return
		}
		who, err := s.slot.ReceiveDecline(&d)
		if err != nil {
			s.opts.Logf("serve: decline refused: %v", err)
			writeError(w, err)
			return
		}
		s.opts.Logf("serve: Group declined by %s (%q); slot is free", who.String()[:8], d.Reason)
		w.WriteHeader(http.StatusOK)
	})
}

// logStatus reports the Slot after an event.
func (s *server) logStatus(event string) {
	if rec, ok := s.slot.Current(); ok {
		s.opts.Logf("serve: %s; Group %s is %s", event, rec.GroupID(), rec.Status())
	}
}

// readProto decodes the request body into msg, answering 400 itself.
func readProto(w http.ResponseWriter, r *http.Request, msg proto.Message) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, formationMaxBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	if err := proto.Unmarshal(raw, msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// post delivers msg to one Roster Node's formation endpoint.
func (s *server) post(ctx context.Context, to discovered, path string, msg proto.Message) error {
	raw, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, formationPostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, formationURL(to.peer, path), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := s.opts.Tailnet.HTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("node %s unreachable: %w", to.NodeID.String()[:8], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return &rejectError{node: to.NodeID, status: resp.StatusCode, reason: strings.TrimSpace(string(body))}
}

// groupCreate is `group create` (§6.2 steps 1–3): discover the other
// Nodes, sign the proposal, hold it, deliver it. A delivery failure
// cancels the local proposal so the Slot stays free.
func (s *server) groupCreate(ctx context.Context, nodeIDs []identity.ID) (GroupView, error) {
	self := s.opts.Node.NodeID
	others := make([]identity.ID, 0, len(nodeIDs))
	seenSelf := false
	for _, id := range nodeIDs {
		if id == self {
			seenSelf = true
			continue
		}
		others = append(others, id)
	}
	if !seenSelf {
		return GroupView{}, &requestError{"serve: this Node must be on the roster"}
	}
	if len(others) != 1 {
		return GroupView{}, &requestError{fmt.Sprintf("serve: MVP forms Groups of exactly one other Node; got %d", len(others))}
	}
	if rec, ok := s.slot.Current(); ok {
		return GroupView{}, fmt.Errorf("%w (%s, %s)", group.ErrOneGroup, rec.GroupID(), rec.Status())
	}

	located, err := s.locate(ctx)
	if err != nil {
		return GroupView{}, err
	}
	roster := []*groupv1.Identity{s.self}
	targets := make([]discovered, 0, len(others))
	for _, id := range others {
		d, ok := located[id]
		if !ok {
			return GroupView{}, &unreachableError{id}
		}
		roster = append(roster, d.identity)
		targets = append(targets, d)
	}
	p, err := s.slot.Propose(roster, time.Now())
	if err != nil {
		return GroupView{}, err
	}
	gid := group.GroupID(nodeIDs)
	for _, to := range targets {
		if err := s.post(ctx, to, "/v1/group/propose", p); err != nil {
			s.opts.Logf("serve: proposal for Group %s not delivered: %v; cancelled", gid, err)
			if cerr := s.slot.Cancel(gid); cerr != nil {
				s.opts.Logf("serve: cancelling proposal: %v", cerr)
			}
			return GroupView{}, err
		}
	}
	s.logStatus("proposal delivered")
	return s.view(nil), nil
}

// groupAccept is `group accept` (§6.2 step 4): sign, deliver to every other
// Roster Node, and only then hold the Accept locally, so this Node is not
// valid while another is still pending. The view names the Nodes still
// to reach so the operator can re-run the verb (re-delivery is a no-op).
func (s *server) groupAccept(ctx context.Context, gid group.ID) (GroupView, error) {
	a, err := s.slot.Accept(gid)
	if err != nil {
		return GroupView{}, err
	}
	rec, _ := s.slot.Current()
	undelivered := s.deliverTo(ctx, rec.Roster(), "/v1/group/accept", a)
	if len(undelivered) == 0 {
		if _, err := s.slot.ReceiveAccept(a); err != nil {
			return GroupView{}, err
		}
	}
	s.logStatus("accepted")
	s.onValid()
	return s.view(undelivered), nil
}

// groupDecline is `group decline`: sign, free the Slot, deliver.
func (s *server) groupDecline(ctx context.Context, gid group.ID, reason string) (GroupView, error) {
	rec, ok := s.slot.Current()
	if !ok {
		return GroupView{}, group.ErrNoGroup
	}
	roster := rec.Roster()
	d, err := s.slot.Decline(gid, reason)
	if err != nil {
		return GroupView{}, err
	}
	undelivered := s.deliverTo(ctx, roster, "/v1/group/decline", d)
	s.opts.Logf("serve: declined Group %s; slot is free", gid)
	return GroupView{GroupID: gid.String(), Status: "declined", Undelivered: undelivered}, nil
}

// deliverTo posts msg to every other Node on roster and returns the Node
// IDs it could not reach or that refused.
func (s *server) deliverTo(ctx context.Context, roster group.Roster, path string, msg proto.Message) []string {
	located, err := s.locate(ctx)
	if err != nil {
		s.opts.Logf("serve: %v", err)
	}
	var undelivered []string
	for _, n := range roster.Nodes() {
		if n.NodeID == s.opts.Node.NodeID {
			continue
		}
		to, ok := located[n.NodeID]
		if !ok {
			s.opts.Logf("serve: node %s (%s) is not reachable on the tailnet", n.NodeID.String()[:8], n.TsnetHostname)
			undelivered = append(undelivered, n.NodeID.String())
			continue
		}
		if err := s.post(ctx, to, path, msg); err != nil {
			s.opts.Logf("serve: %s to %s: %v", path, n.NodeID.String()[:8], err)
			undelivered = append(undelivered, n.NodeID.String())
		}
	}
	return undelivered
}

// view is the Slot as the local API reports it.
func (s *server) view(undelivered []string) GroupView {
	rec, ok := s.slot.Current()
	if !ok {
		return GroupView{Status: "none", Undelivered: undelivered}
	}
	return GroupView{GroupID: rec.GroupID().String(), Status: rec.Status().String(), Undelivered: undelivered}
}

// requestError is a malformed local request (400).
type requestError struct{ msg string }

func (e *requestError) Error() string { return e.msg }

// unreachableError names a Roster Node no tailnet machine advertised an
// Identity for (502).
type unreachableError struct{ node identity.ID }

func (e *unreachableError) Error() string {
	return fmt.Sprintf("serve: node %s is not reachable on the tailnet", e.node)
}
