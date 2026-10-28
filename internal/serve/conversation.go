package serve

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/mention"
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
	// ErrBadHandle means a message names a Handle, as the agent writing it or
	// in a person's mention, that this Group's config.toml does not include
	// where it is sent, or an agent's message is at a depth outside 1 to
	// max_depth
	// (ADR 0015).
	ErrBadHandle = errors.New("serve: that Handle may not write here")
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
	// Handle is the agent that wrote it, empty for a person, and Depth its
	// place in a chain.
	Handle string `json:"handle,omitempty"`
	Depth  uint32 `json:"depth,omitempty"`
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
// commit it. A nil conversationID is Group-level. An empty handle is a
// person's message; otherwise it is that agent's at depth, and this Node's
// config must include the Handle there (ADR 0015).
func (s *server) conversationSend(ctx context.Context, text, handle string, depth uint32, conversationID []byte) (SendView, error) {
	payload, err := cosl.AgentFreeform(text, handle, depth)
	if err != nil {
		return SendView{}, err
	}
	return s.conversationAppend(ctx, cosl.TypeAppFreeform, conversationID, payload, func(store *cosl.Store) error {
		return s.checkHandle(store, conversationID, handle, depth, text)
	})
}

// checkHandle allows a person's message at depth 0 whose mentions all name
// Handles included where it is sent, and an agent's whose Handle is included
// there, at a depth from 1 to max_depth. An agent's mentions are not checked:
// the chain skips those it cannot summon (ADR 0015). Like requireConversation
// it is the writer's check, not the FSM's: another Node's config is not this
// Node's to know.
func (s *server) checkHandle(store *cosl.Store, conversationID []byte, handle string, depth uint32, text string) error {
	if handle == "" {
		return s.checkMentions(store, conversationID, depth, mention.Handles(text))
	}
	cfg, conv, err := s.readConfig(store, conversationID)
	if err != nil {
		return err
	}
	if _, ok := cfg.Agents[handle]; !ok {
		return fmt.Errorf("%w: no [agents.%s] is defined in this Group's config.toml", ErrBadHandle, handle)
	}
	inc, err := s.included(cfg, conv)
	if err != nil {
		return err
	}
	if !includes(inc, handle) {
		return fmt.Errorf("%w: @%s is not in [agent] profiles where this is sent", ErrBadHandle, handle)
	}
	if depth < 1 || depth > uint32(inc.MaxDepth) {
		return fmt.Errorf("%w: depth %d is outside 1 to max_depth %d", ErrBadHandle, depth, inc.MaxDepth)
	}
	return nil
}

// checkMentions refuses a person's message with a depth, or one that
// mentions a Handle not included where it is sent, naming it and what is.
// A message that mentions nobody reads no config.
func (s *server) checkMentions(store *cosl.Store, conversationID []byte, depth uint32, mentions []string) error {
	if depth != 0 {
		return fmt.Errorf("%w: a person's message has no depth; name the agent", ErrBadHandle)
	}
	if len(mentions) == 0 {
		return nil
	}
	cfg, conv, err := s.readConfig(store, conversationID)
	if err != nil {
		return err
	}
	inc, err := s.included(cfg, conv)
	if err != nil {
		return err
	}
	for _, m := range mentions {
		if !includes(inc, m) {
			return fmt.Errorf("%w: @%s is not in [agent] profiles where this is sent; included: %s", ErrBadHandle, m, listHandles(inc.Profiles))
		}
	}
	return nil
}

func (s *server) included(cfg config.Config, conv config.Workspace) (config.Included, error) {
	home, _ := os.UserHomeDir() // only for profile directories, unused here
	return cfg.Included(conv, s.opts.Paths.Config, home)
}

func includes(inc config.Included, handle string) bool {
	return slices.ContainsFunc(inc.Profiles, func(p config.ResolvedProfile) bool { return p.Handle == handle })
}

// listHandles is profiles' Handles as "@a, @b", or "none".
func listHandles(profiles []config.ResolvedProfile) string {
	if len(profiles) == 0 {
		return "none"
	}
	hs := make([]string, len(profiles))
	for i, p := range profiles {
		hs[i] = "@" + p.Handle
	}
	return strings.Join(hs, ", ")
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
	keys, err := key.Read(store)
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
		v, err := s.conversationSend(r.Context(), req.Text, req.Handle, req.Depth, conv)
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
	return c.ConversationSendAs(ctx, text, "", 0, conversationID)
}

// ConversationSendAs is `conversation send --handle`: as ConversationSend,
// written by the agent with this Handle at depth in its chain. An empty
// handle, at depth 0, is a person's message.
func (c Client) ConversationSendAs(ctx context.Context, text, handle string, depth uint32, conversationID []byte) (SendView, error) {
	var v SendView
	req := sendRequest{Text: text, Handle: handle, Depth: depth}
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
