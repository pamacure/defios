package serve

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/raftx"
)

var (
	// ErrNoReplica means this Node has no valid Group yet, so there is no COSL
	// to append to.
	ErrNoReplica = errors.New("serve: this Node has no valid Group; nothing to append to")
	// ErrNoConversation means the id is not the Conversation ID of a Key this
	// replica has minted. A Conversation exists iff the Key exists (§8).
	ErrNoConversation = errors.New("serve: no such Conversation; the Key has not been minted")
)

// SendView is the local API's report of a committed COSL append.
type SendView struct {
	Seq   uint64 `json:"seq"`
	Term  uint64 `json:"term"`
	MsgID string `json:"msg_id"`
}

type sendRequest struct {
	Text           string `json:"text"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// conversationSend is `conversation send` (§8): author an app.freeform and
// commit it through Raft, bounded by the §4.2 apply timeout; with the other
// Node down that bound is the failure. A nil conversationID is Group-level.
func (s *server) conversationSend(ctx context.Context, text string, conversationID []byte) (SendView, error) {
	payload, err := cosl.Freeform(text)
	if err != nil {
		return SendView{}, err
	}
	if conversationID != nil && len(conversationID) != 32 {
		return SendView{}, fmt.Errorf("serve: conversation id is %d bytes, want 32", len(conversationID))
	}
	replica, store, running := s.current()
	rec, held := s.slot.Current()
	if !running || !held {
		return SendView{}, ErrNoReplica
	}
	if err := requireConversation(store, conversationID); err != nil {
		return SendView{}, err
	}
	author := cosl.Author{Node: s.opts.Node, GroupID: rec.GroupID()}
	signed, err := author.Sign(cosl.TypeAppFreeform, conversationID, payload)
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

// requireConversation allows Group-level talk (a nil id) and a Conversation
// whose id is derived from a Key already on this replica. The check is the
// writer's: §5.4 does not put Session or Conversation policy in the FSM.
func requireConversation(store *cosl.Store, conversationID []byte) error {
	if conversationID == nil {
		return nil
	}
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		return err
	}
	keys, err := key.List(rows)
	if err != nil {
		return err
	}
	for _, k := range keys {
		id := key.ConversationID(k.Curve, k.PublicKey)
		if bytes.Equal(id[:], conversationID) {
			return nil
		}
	}
	return ErrNoConversation
}

func (s *server) conversationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/conversation/send", func(w http.ResponseWriter, r *http.Request) {
		var req sendRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		var conv []byte
		if req.ConversationID != "" {
			var err error
			conv, err = hex.DecodeString(req.ConversationID)
			if err != nil || len(conv) != 32 {
				writeError(w, fmt.Errorf("serve: conversation id must be 32-byte hex"))
				return
			}
		}
		v, err := s.conversationSend(r.Context(), req.Text, conv)
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

// ConversationSend is `conversation send`. A nil conversationID appends
// Group-level app.freeform; otherwise the row is that Conversation.
func (c Client) ConversationSend(ctx context.Context, text string, conversationID []byte) (SendView, error) {
	var v SendView
	req := sendRequest{Text: text}
	if conversationID != nil {
		req.ConversationID = hex.EncodeToString(conversationID)
	}
	err := c.do(ctx, "/v1/conversation/send", req, &v)
	return v, err
}
