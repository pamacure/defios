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
	"defios.com/internal/recap"
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
		Short: "Print messages: seq, time, display name, text. The live Recap stands in for the talk it covers. --key scopes to that Key",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runConversationLog(g, keyQuery, limit) },
	}
	log.Flags().StringVar(&keyQuery, "key", "", "curve:hex, a unique public-key prefix, or the Conversation ID")
	log.Flags().IntVar(&limit, "limit", 0, "only the latest N messages after the live Recap")
	bindGroupFlag(g, log)
	root.AddCommand(log)
	var baseSeq uint64
	var text string
	recapCmd := &cobra.Command{
		Use:   "recap --base-seq <n> --text <text>",
		Short: "Append a Recap accounting for talk up to --base-seq (requires serve). --key scopes it to that Key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConversationRecap(cmd, g, keyQuery, baseSeq, text)
		},
	}
	recapCmd.Flags().StringVar(&keyQuery, "key", "", "curve:hex, a unique public-key prefix, or the Conversation ID")
	recapCmd.Flags().Uint64Var(&baseSeq, "base-seq", 0, "the highest seq the Recap accounts for: a message of this Conversation")
	recapCmd.Flags().StringVar(&text, "text", "", "the account of the talk it covers")
	recapCmd.MarkFlagRequired("base-seq")
	recapCmd.MarkFlagRequired("text")
	root.AddCommand(recapCmd)
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

func runConversationRecap(cmd *cobra.Command, g *globals, keyQuery string, baseSeq uint64, text string) error {
	if err := cosl.CheckText(text); err != nil {
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
	v, err := serve.Client{Socket: g.paths().Socket}.ConversationRecap(cmd.Context(), text, baseSeq, conv)
	if err != nil {
		if errors.Is(err, recap.ErrStale) || errors.Is(err, recap.ErrBadBase) {
			return exitUsagef("%w", err)
		}
		return sendError(err)
	}
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "recap as seq %d (msg_id %s)\n", v.Seq, v.MsgID)
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

// messageLine is one `conversation log` row, human or JSON: an app.freeform,
// or the live Recap with the count of messages it covers.
type messageLine struct {
	Seq               uint64 `json:"seq"`
	Term              uint64 `json:"term"`
	CreatedAt         string `json:"created_at"`
	Sender            string `json:"sender"`
	SenderDisplayName string `json:"sender_display_name"`
	MsgID             string `json:"msg_id"`
	MsgType           string `json:"msg_type"`
	Text              string `json:"text"`
	ContentType       string `json:"content_type"`
	BaseSeq           uint64 `json:"base_seq,omitempty"` // app.recap only
	Covers            int    `json:"covers,omitempty"`   // app.recap only
}

func newMessageLine(rec *group.Record, r cosl.Row) messageLine {
	return messageLine{
		Seq:               r.Seq,
		Term:              r.Term,
		CreatedAt:         rfc3339(r.CreatedAt),
		Sender:            r.Sender.String(),
		SenderDisplayName: rosterName(rec.Roster(), r.Sender),
		MsgID:             hex.EncodeToString(r.MsgID[:]),
		MsgType:           cosl.TypeName(r.MsgType),
	}
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

	var q cosl.Query
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
	view, err := recap.Fold(rows)
	if err != nil {
		return exitOperational(err)
	}
	// --limit trims the talk only: the live Recap always stands in for what
	// it hides.
	talk := view.Talk
	if limit > 0 && len(talk) > limit {
		talk = talk[len(talk)-limit:]
	}
	lines := make([]messageLine, 0, len(talk)+1)
	if view.Live != nil {
		l := newMessageLine(rec, view.Live.Row)
		l.Text, l.BaseSeq, l.Covers = view.Live.Text, view.Live.BaseSeq, view.Live.Covers
		lines = append(lines, l)
	}
	for _, r := range talk {
		env, err := r.Decode()
		if err != nil {
			return exitOperational(err)
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			return exitOperational(fmt.Errorf("cosl: seq %d: %w", r.Seq, err))
		}
		ff := msg.(*coslv1.AppFreeform)
		l := newMessageLine(rec, r)
		l.Text, l.ContentType = ff.Text, ff.ContentType
		lines = append(lines, l)
	}
	if g.json {
		return g.emitJSON(lines)
	}
	for _, l := range lines {
		text := cosl.EscapeNewlines(l.Text)
		if l.MsgType == cosl.TypeName(cosl.TypeAppRecap) {
			text = fmt.Sprintf("[recap of %d messages to seq %d] %s", l.Covers, l.BaseSeq, text)
		}
		fmt.Fprintf(g.app.Stdout, "%d %s %s  %s\n", l.Seq, l.CreatedAt, l.SenderDisplayName, text)
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
