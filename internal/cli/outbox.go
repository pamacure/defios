package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"defios.com/internal/node"
	"defios.com/internal/outbox"
	"defios.com/internal/serve"
)

func outboxCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "outbox",
		Short: "See and withdraw what this Node wrote while no leader was known, waiting to commit (ADR 0025)",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List this Node's queued messages in queue order: msg id, time, type, Conversation, a Sign Request's deadline, summary (no serve needed)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runOutboxList(g) },
	}
	bindGroupFlag(g, list)
	drop := &cobra.Command{
		Use:   "drop <msg-id>",
		Short: "Erase one queued message, named by its msg id or a unique prefix of it; nothing else is dropped, not even a queued reply to it",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runOutboxDrop(cmd, g, args[0]) },
	}
	root.AddCommand(list, drop)
	return root
}

// queuedLine is one `outbox list` row, human or JSON.
type queuedLine struct {
	MsgID          string  `json:"msg_id"`
	Type           string  `json:"type"`
	ConversationID *string `json:"conversation_id"`
	CreatedAt      string  `json:"created_at"`
	Deadline       string  `json:"deadline,omitempty"` // a sign.request's
	Summary        string  `json:"summary"`
}

// runOutboxList prints the Outbox file: a running serve persists each
// change before it makes it, so the file is current.
func runOutboxList(g *globals) error {
	h, err := openNode(g)
	if err != nil {
		return err
	}
	alias, err := g.resolveAlias()
	if err != nil {
		return err
	}
	queued, err := h.Outbox(context.Background(), alias)
	if err != nil {
		return exitOperational(err)
	}
	lines := make([]queuedLine, 0, len(queued))
	for _, q := range queued {
		l := queuedLine{MsgID: q.MsgID, Type: q.Type, CreatedAt: rfc3339(q.CreatedAt.UnixNano()), Summary: q.Summary}
		if q.ConversationID != node.GroupTalk {
			c := q.ConversationID
			l.ConversationID = &c
		}
		if !q.Deadline.IsZero() {
			l.Deadline = rfc3339(q.Deadline.UnixNano())
		}
		lines = append(lines, l)
	}
	if g.json {
		return g.emitJSON(lines)
	}
	p := g.out()
	for _, l := range lines {
		conv := "group"
		if l.ConversationID != nil {
			conv = (*l.ConversationID)[:8]
		}
		deadline := ""
		if l.Deadline != "" {
			deadline = "deadline " + l.Deadline + "  "
		}
		fmt.Fprintf(g.app.Stdout, "%s %s  %s  %s  %s%s\n", p.primary(l.MsgID), p.dim(l.CreatedAt), p.status(l.Type), conv, deadline, l.Summary)
	}
	return nil
}

// runOutboxDrop erases one queued message: the serve whose replica owns
// the Outbox drops it, or with none the library edits the file under the
// Outbox's lock.
func runOutboxDrop(cmd *cobra.Command, g *globals, prefix string) error {
	if _, err := outbox.MsgIDPrefix(prefix); err != nil {
		return exitUsagef("%w", err)
	}
	h, err := openNode(g)
	if err != nil {
		return err
	}
	alias, err := g.resolveAlias()
	if err != nil {
		return err
	}
	id, err := h.DropQueued(cmd.Context(), alias, prefix)
	var none *node.NoReplicaError
	switch {
	case errors.As(err, &none):
		return exitOperational(err)
	case errors.Is(err, outbox.ErrAmbiguous):
		return exitUsagef("%w", err)
	case err != nil:
		return serveError(err)
	}
	v := serve.DropView{MsgID: id}
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "dropped %s: it never reached the log\n", v.MsgID)
	return nil
}
