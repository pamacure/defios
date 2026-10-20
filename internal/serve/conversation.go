package serve

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"defios.com/internal/cosl"
	"defios.com/internal/raftx"
)

// ErrNoReplica means this Node has no valid Group yet, so there is no COSL
// to append to.
var ErrNoReplica = errors.New("serve: this Node has no valid Group; nothing to append to")

// SendView is the local API's report of a committed COSL append.
type SendView struct {
	Seq   uint64 `json:"seq"`
	Term  uint64 `json:"term"`
	MsgID string `json:"msg_id"`
}

type sendRequest struct {
	Text string `json:"text"`
}

// conversationSend is `conversation send` (§8): author a Group-level
// app.freeform and commit it through Raft, bounded by the §4.2 apply
// timeout; with the other Node down that bound is the failure.
func (s *server) conversationSend(ctx context.Context, text string) (SendView, error) {
	payload, err := cosl.Freeform(text)
	if err != nil {
		return SendView{}, err
	}
	replica, _, running := s.current()
	rec, held := s.slot.Current()
	if !running || !held {
		return SendView{}, ErrNoReplica
	}
	author := cosl.Author{Node: s.opts.Node, GroupID: rec.GroupID()}
	signed, err := author.Sign(cosl.TypeAppFreeform, nil, payload)
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

func (s *server) conversationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/conversation/send", func(w http.ResponseWriter, r *http.Request) {
		var req sendRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		v, err := s.conversationSend(r.Context(), req.Text)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, v)
	})
}

// snapshotRoutes exposes Replica.Snapshot on the local API. No CLI verb
// fronts it: Raft snapshots on its own (§4.2); e2e tests use this to make
// a Node fall beyond the retained log quickly.
func (s *server) snapshotRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/raft/snapshot", func(w http.ResponseWriter, r *http.Request) {
		replica, _, ok := s.current()
		if !ok {
			writeError(w, ErrNoReplica)
			return
		}
		if err := replica.Snapshot(); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, struct{}{})
	})
}

// Snapshot asks the running serve to snapshot and compact its Raft log now.
func (c Client) Snapshot(ctx context.Context) error {
	return c.do(ctx, "/v1/raft/snapshot", struct{}{}, &struct{}{})
}

// ConversationSend is `conversation send`: append text as Group-level
// app.freeform.
func (c Client) ConversationSend(ctx context.Context, text string) (SendView, error) {
	var v SendView
	err := c.do(ctx, "/v1/conversation/send", sendRequest{Text: text}, &v)
	return v, err
}
