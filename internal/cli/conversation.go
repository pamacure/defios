package cli

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/key"
	"defios.com/internal/raftx"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
)

func conversationCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "conversation",
		Short: "Talk with the other Nodes of this Group over the COSL",
	}
	var keyQuery string
	send := &cobra.Command{
		Use:   "send <text>",
		Short: "Append a message (requires serve). --key scopes it to that Key",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runConversationSend(cmd, g, keyQuery, args[0]) },
	}
	send.Flags().StringVar(&keyQuery, "key", "", "curve:hex, a unique public-key prefix, or the Conversation ID")
	root.AddCommand(send)
	var limit int
	log := &cobra.Command{
		Use:   "log",
		Short: "Print messages: seq, time, display name, text. --key scopes to that Key",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runConversationLog(g, keyQuery, limit) },
	}
	log.Flags().StringVar(&keyQuery, "key", "", "curve:hex, a unique public-key prefix, or the Conversation ID")
	log.Flags().IntVar(&limit, "limit", 0, "only the latest N messages")
	root.AddCommand(log)
	return root
}

func runConversationSend(cmd *cobra.Command, g *globals, keyQuery, text string) error {
	if _, err := cosl.Freeform(text); err != nil {
		return exitUsagef("%w", err)
	}
	var conv []byte
	if keyQuery != "" {
		id, err := resolveConversation(g, keyQuery)
		if err != nil {
			return err
		}
		conv = id
	}
	v, err := serve.Client{Socket: g.paths().Socket}.ConversationSend(cmd.Context(), text, conv)
	if err != nil {
		return sendError(err)
	}
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "sent as seq %d (msg_id %s)\n", v.Seq, v.MsgID)
	return nil
}

// sendError maps an append failure from serve to an exit code: a commit
// the FSM rejected is a protocol reject (3); bad text is usage (1);
// everything else, including no leader, is operational (2).
func sendError(err error) error {
	switch {
	case errors.Is(err, raftx.ErrNotInserted):
		return &ExitError{Code: ExitProtocol, Err: err}
	case errors.Is(err, cosl.ErrBadText):
		return exitUsagef("%w", err)
	case errors.Is(err, raftx.ErrNoLeader):
		return exitOperationalf("%w; not committed now, but if this Node was leading it may still land when the other Node returns: check `conversation log` before re-sending", err)
	}
	return serveError(err)
}

// messageLine is one `conversation log` row, human or JSON.
type messageLine struct {
	Seq               uint64 `json:"seq"`
	Term              uint64 `json:"term"`
	CreatedAt         string `json:"created_at"`
	Sender            string `json:"sender"`
	SenderDisplayName string `json:"sender_display_name"`
	MsgID             string `json:"msg_id"`
	Text              string `json:"text"`
	ContentType       string `json:"content_type"`
}

func runConversationLog(g *globals, keyQuery string, limit int) error {
	if limit < 0 {
		return exitUsagef("--limit must be >= 0")
	}
	_, rec, err := loadGroup(g)
	if err != nil {
		return err
	}
	store, err := openReplica(g)
	if err != nil {
		return err
	}
	defer store.Close()

	typ := cosl.TypeAppFreeform
	q := cosl.Query{MsgType: &typ, Limit: limit}
	if keyQuery == "" {
		q.GroupLevel = true
	} else {
		id, err := conversationID(store, keyQuery)
		if err != nil {
			return err
		}
		q.ConversationID = id
	}
	rows, err := store.Log(q)
	if err != nil {
		return exitOperational(err)
	}
	lines := make([]messageLine, 0, len(rows))
	for _, r := range rows {
		env, err := r.Decode()
		if err != nil {
			return exitOperational(err)
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			return exitOperational(fmt.Errorf("cosl: seq %d: %w", r.Seq, err))
		}
		ff := msg.(*coslv1.AppFreeform)
		lines = append(lines, messageLine{
			Seq:               r.Seq,
			Term:              r.Term,
			CreatedAt:         rfc3339(r.CreatedAt),
			Sender:            r.Sender.String(),
			SenderDisplayName: rosterName(rec.Roster(), r.Sender),
			MsgID:             hex.EncodeToString(r.MsgID[:]),
			Text:              ff.Text,
			ContentType:       ff.ContentType,
		})
	}
	if g.json {
		return g.emitJSON(lines)
	}
	for _, l := range lines {
		fmt.Fprintf(g.app.Stdout, "%d %s %s  %s\n", l.Seq, l.CreatedAt, l.SenderDisplayName, cosl.EscapeNewlines(l.Text))
	}
	return nil
}

// resolveConversation is the Conversation of a minted Key. Unknown and
// ambiguous queries are usage errors: a Conversation exists iff the Key does.
func resolveConversation(g *globals, query string) ([]byte, error) {
	store, err := openReplica(g)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return conversationID(store, query)
}

func conversationID(store *cosl.Store, query string) ([]byte, error) {
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		return nil, exitOperational(err)
	}
	keys, err := key.List(rows)
	if err != nil {
		return nil, exitOperational(err)
	}
	k, err := key.Resolve(keys, query)
	if err != nil {
		return nil, exitUsagef("%w", err)
	}
	id := key.ConversationID(k.Curve, k.PublicKey)
	return append([]byte(nil), id[:]...), nil
}

// rosterName is the display name the Roster pins for a sender; the FSM
// only admits Roster Nodes, so the fallback is for a replica older than
// its Roster.
func rosterName(roster group.Roster, sender identity.ID) string {
	if n, ok := roster.Lookup(sender); ok {
		return n.DisplayName
	}
	return sender.String()[:8]
}
