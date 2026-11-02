package cli

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"defios.com/internal/cosl"
	"defios.com/internal/outbox"
	"defios.com/internal/serve"
	"defios.com/internal/signreq"
	coslv1 "defios.com/proto/cosl/v1"
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
	alias, err := g.resolveAlias()
	if err != nil {
		return err
	}
	queued, err := outbox.Read(g.paths().OutboxFile(alias))
	if err != nil {
		return exitOperational(err)
	}
	lines := make([]queuedLine, 0, len(queued))
	for _, s := range queued {
		env, err := cosl.Unmarshal(s.Bytes)
		if err != nil {
			return exitOperational(err)
		}
		l := queuedLine{
			MsgID:     hex.EncodeToString(s.MsgID[:]),
			Type:      cosl.TypeName(env.MsgType),
			CreatedAt: rfc3339(env.CreatedAt),
			Summary:   cosl.Summary(env.MsgType, env.Payload),
		}
		if env.ConversationId != nil {
			c := hex.EncodeToString(env.ConversationId)
			l.ConversationID = &c
		}
		if env.MsgType == cosl.TypeSignRequest {
			msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
			if err != nil {
				return exitOperational(err)
			}
			l.Deadline = rfc3339(signreq.Deadline(env.CreatedAt, msg.(*coslv1.SignRequest)).UnixNano())
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

// runOutboxDrop erases one queued message. A running serve's replica owns
// the Outbox and holds its lock, so serve drops it then; otherwise the file
// is edited here, under that lock.
func runOutboxDrop(cmd *cobra.Command, g *globals, prefix string) error {
	if _, err := outbox.MsgIDPrefix(prefix); err != nil {
		return exitUsagef("%w", err)
	}
	p := g.paths()
	alias, err := g.resolveAlias()
	if err != nil {
		return err
	}
	var v serve.DropView
	unlock, err := outbox.Lock(p.OutboxFile(alias), false)
	switch {
	case err == nil:
		v, err = dropFromFile(g, alias, prefix)
		unlock()
	case errors.Is(err, outbox.ErrBusy):
		v, err = serve.Client{Socket: p.Socket}.OutboxDrop(cmd.Context(), prefix)
	}
	var exit *ExitError
	switch {
	case errors.As(err, &exit):
		return err
	case errors.Is(err, outbox.ErrAmbiguous):
		return exitUsagef("%w", err)
	case err != nil:
		return serveError(err)
	}
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "dropped %s: it never reached the log\n", v.MsgID)
	return nil
}

// dropFromFile is runOutboxDrop with the Outbox's lock held: it opens the
// Outbox file itself, and the replica to tell a committed message.
func dropFromFile(g *globals, alias, prefix string) (serve.DropView, error) {
	store, err := openReplica(g)
	if err != nil {
		return serve.DropView{}, err
	}
	defer store.Close()
	ob, err := outbox.Open(outbox.Config{Path: g.paths().OutboxFile(alias)})
	if err != nil {
		return serve.DropView{}, err
	}
	s, err := outbox.Withdraw(ob, store, prefix)
	if err != nil {
		return serve.DropView{}, err
	}
	return serve.DropView{MsgID: hex.EncodeToString(s.MsgID[:])}, nil
}
