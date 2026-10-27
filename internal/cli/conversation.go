package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
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
	var opts logOptions
	log := &cobra.Command{
		Use:   "log",
		Short: "Print messages: seq, time, display name, text. The live Recap stands in for the talk it covers. --key scopes to that Key",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runConversationLog(g, keyQuery, opts) },
	}
	log.Flags().StringVar(&keyQuery, "key", "", "curve:hex, a unique public-key prefix, or the Conversation ID")
	log.Flags().IntVar(&opts.limit, "limit", 0, "only the latest N messages after the live Recap (with --full, the latest N rows)")
	log.Flags().BoolVar(&opts.full, "full", false, "also show what the live Recap supersedes; a pruned row shows as metadata only")
	log.Flags().BoolVar(&opts.lean, "lean", false, "only \"<display name>: <text>\" lines, the live Recap then every message after it, for an agent's context")
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
	prune := &cobra.Command{
		Use:   "prune",
		Short: "Destroy, on this Node only, the payloads of the talk the live Recap covers. --key scopes it to that Key",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runConversationPrune(g, keyQuery) },
	}
	prune.Flags().StringVar(&keyQuery, "key", "", "curve:hex, a unique public-key prefix, or the Conversation ID")
	bindGroupFlag(g, prune)
	root.AddCommand(prune)
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

// pruneLine is `conversation prune`'s result: how many messages it pruned
// now, up to which seq.
type pruneLine struct {
	BaseSeq  uint64 `json:"base_seq"`
	Messages int    `json:"messages"`
}

// runConversationPrune writes this Node's replica directly: Pruning is
// Node-local, appends nothing and tells no other Node, so it needs no serve
// (ADR 0012).
func runConversationPrune(g *globals, keyQuery string) error {
	store, err := openReplica(g)
	if err != nil {
		return err
	}
	defer store.Close()
	policy, err := prunePolicy(g)
	if err != nil {
		return err
	}
	var conv []byte
	if keyQuery != "" {
		if conv, err = conversationID(store, keyQuery); err != nil {
			return err
		}
	}
	baseSeq, n, err := recap.Prune(store, conv)
	if errors.Is(err, recap.ErrNoRecap) {
		return exitUsagef("%w; account for it first with `conversation recap`", err)
	}
	if err != nil {
		return exitOperational(err)
	}
	if !policy {
		fmt.Fprintln(g.app.Stderr, "warning: this Node's prune policy is off, so a later snapshot restore from a Node that kept this talk brings it back; set `prune = true` in this Group's config.toml to keep it pruned")
	}
	if g.json {
		return g.emitJSON(pruneLine{BaseSeq: baseSeq, Messages: n})
	}
	fmt.Fprintf(g.app.Stdout, "pruned %d messages up to seq %d\n", n, baseSeq)
	return nil
}

// prunePolicy is whether this Node's config.toml for the Current Group sets
// `prune = true`. A Node with no config.toml has the default, off.
func prunePolicy(g *globals) (bool, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return false, err
	}
	cfg, err := config.ReadFile(g.paths().GroupConfigFile(alias))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, exitOperational(err)
	}
	return cfg.Prune, nil
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
	Superseded        bool   `json:"superseded,omitempty"`
	Pruned            bool   `json:"pruned,omitempty"` // no text: only the row's metadata remains
}

// leanLine is one `conversation log --lean` entry: who said it and what.
type leanLine struct {
	SenderDisplayName string `json:"sender_display_name"`
	Text              string `json:"text"`
}

// logOptions are `conversation log`'s reader flags.
type logOptions struct {
	limit      int
	full, lean bool
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

func runConversationLog(g *globals, keyQuery string, opts logOptions) error {
	if opts.limit < 0 {
		return exitUsagef("--limit must be >= 0")
	}
	if opts.full && opts.lean {
		return exitUsagef("--full and --lean are opposite views; pass one")
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
	lines := make([]messageLine, 0, len(view.Full))
	for _, e := range logEntries(view, opts) {
		l, err := newEntryLine(rec, e)
		if err != nil {
			return exitOperational(err)
		}
		lines = append(lines, l)
	}
	if opts.lean {
		return emitLean(g, lines)
	}
	if g.json {
		return g.emitJSON(lines)
	}
	for _, l := range lines {
		text := cosl.EscapeNewlines(l.Text)
		switch {
		case l.Pruned:
			text = "[pruned]"
		case l.MsgType == cosl.TypeName(cosl.TypeAppRecap):
			text = fmt.Sprintf("[recap of %d messages to seq %d] %s", l.Covers, l.BaseSeq, text)
		}
		fmt.Fprintf(g.app.Stdout, "%d %s %s  %s\n", l.Seq, l.CreatedAt, l.SenderDisplayName, text)
	}
	return nil
}

// logEntries is what a view shows. --full is every row and --limit keeps
// its latest ones; otherwise --limit trims only the talk, since the live
// Recap always stands in for what it hides. --lean drops pruned talk before
// the limit counts it: it has nothing to show for one.
func logEntries(view recap.View, opts logOptions) []recap.Entry {
	if opts.full {
		return latest(view.Full, opts.limit)
	}
	talk := view.Talk
	if opts.lean {
		talk = slices.DeleteFunc(slices.Clone(talk), cosl.Row.Pruned)
	}
	talk = latest(talk, opts.limit)
	entries := make([]recap.Entry, 0, len(talk)+1)
	if view.Live != nil {
		entries = append(entries, recap.Entry{Row: view.Live.Row, Recap: view.Live})
	}
	for _, r := range talk {
		entries = append(entries, recap.Entry{Row: r})
	}
	return entries
}

// latest is the last n of xs, or all of them when n is 0.
func latest[T any](xs []T, n int) []T {
	if n > 0 && len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

// newEntryLine is e as a log row: a Recap, talk, or a pruned row's metadata.
func newEntryLine(rec *group.Record, e recap.Entry) (messageLine, error) {
	l := newMessageLine(rec, e.Row)
	l.Superseded = e.Superseded
	switch {
	case e.Recap != nil:
		l.Text, l.BaseSeq, l.Covers = e.Recap.Text, e.Recap.BaseSeq, e.Recap.Covers
	case e.Row.Pruned():
		l.Pruned = true
	default:
		env, err := e.Row.Decode()
		if err != nil {
			return messageLine{}, err
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			return messageLine{}, fmt.Errorf("cosl: seq %d: %w", e.Row.Seq, err)
		}
		ff := msg.(*coslv1.AppFreeform)
		l.Text, l.ContentType = ff.Text, ff.ContentType
	}
	return l, nil
}

// emitLean prints `<display name>: <text>`, one entry per line, and nothing
// else: timestamps are advisory and never ordering, while who said what is
// load-bearing in a multi-party Conversation.
func emitLean(g *globals, lines []messageLine) error {
	lean := make([]leanLine, len(lines))
	for i, l := range lines {
		lean[i] = leanLine{SenderDisplayName: l.SenderDisplayName, Text: l.Text}
	}
	if g.json {
		return g.emitJSON(lean)
	}
	for _, l := range lean {
		fmt.Fprintf(g.app.Stdout, "%s: %s\n", l.SenderDisplayName, cosl.EscapeNewlines(l.Text))
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
	keys, err := key.Read(store)
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
