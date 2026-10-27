package serve

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/raftx"
	"defios.com/internal/recap"
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

type recapRequest struct {
	Text           string `json:"text"`
	BaseSeq        uint64 `json:"base_seq"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// requestConversation is a request's Conversation ID, nil for Group-level
// talk.
func requestConversation(hexID string) ([]byte, error) {
	if hexID == "" {
		return nil, nil
	}
	conv, err := hex.DecodeString(hexID)
	if err != nil || len(conv) != 32 {
		return nil, fmt.Errorf("serve: conversation id must be 32-byte hex")
	}
	return conv, nil
}

// conversationSend is `conversation send` (§8): author an app.freeform and
// commit it. A nil conversationID is Group-level.
func (s *server) conversationSend(ctx context.Context, text string, conversationID []byte) (SendView, error) {
	payload, err := cosl.Freeform(text)
	if err != nil {
		return SendView{}, err
	}
	return s.conversationAppend(ctx, cosl.TypeAppFreeform, conversationID, payload, nil)
}

// conversationRecap is `conversation recap`: author an app.recap accounting
// for the Conversation's talk up to baseSeq and commit it. The writer checks
// it against the folded Conversation first so a Recap the fold would skip
// fails loudly; like the Networks writer it accepts that a Recap committing
// between this read and the append can still make the fold skip it.
func (s *server) conversationRecap(ctx context.Context, text string, baseSeq uint64, conversationID []byte) (SendView, error) {
	payload, err := cosl.Recap(text, baseSeq)
	if err != nil {
		return SendView{}, err
	}
	return s.conversationAppend(ctx, cosl.TypeAppRecap, conversationID, payload, func(store *cosl.Store) error {
		rows, err := store.Log(cosl.Query{GroupLevel: conversationID == nil, ConversationID: conversationID})
		if err != nil {
			return err
		}
		v, err := recap.Fold(rows)
		if err != nil {
			return err
		}
		return recap.Check(v, baseSeq)
	})
}

// conversationAppend signs payload as typ in the Conversation (Group-level
// when conversationID is nil) and commits it through Raft, bounded by the
// §4.2 apply timeout; with the other Node down that bound is the failure.
// check, when set, is the writer's precondition against the replica.
func (s *server) conversationAppend(ctx context.Context, typ uint32, conversationID []byte, payload proto.Message, check func(*cosl.Store) error) (SendView, error) {
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
	if check != nil {
		if err := check(store); err != nil {
			return SendView{}, err
		}
	}
	author := cosl.Author{Node: s.opts.Node, GroupID: rec.GroupID()}
	signed, err := author.Sign(typ, conversationID, payload)
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
		conv, err := requestConversation(req.ConversationID)
		if err != nil {
			writeError(w, err)
			return
		}
		v, err := s.conversationSend(r.Context(), req.Text, conv)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, v)
	})
	mux.HandleFunc("POST /v1/conversation/recap", func(w http.ResponseWriter, r *http.Request) {
		var req recapRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		conv, err := requestConversation(req.ConversationID)
		if err != nil {
			writeError(w, err)
			return
		}
		v, err := s.conversationRecap(r.Context(), req.Text, req.BaseSeq, conv)
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

// ConversationRecap is `conversation recap`: commit an app.recap accounting
// for the Conversation's talk up to baseSeq. A nil conversationID recaps
// Group-level talk.
func (c Client) ConversationRecap(ctx context.Context, text string, baseSeq uint64, conversationID []byte) (SendView, error) {
	var v SendView
	req := recapRequest{Text: text, BaseSeq: baseSeq}
	if conversationID != nil {
		req.ConversationID = hex.EncodeToString(conversationID)
	}
	err := c.do(ctx, "/v1/conversation/recap", req, &v)
	return v, err
}
