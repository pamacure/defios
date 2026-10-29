package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"defios.com/internal/agent"
	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/key"
	"defios.com/internal/mention"
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
	var as sendAs
	send := &cobra.Command{
		Use:   "send <text>",
		Short: "Append a message (requires serve) and print the messages of the agents it mentions as they commit; Ctrl-C cancels them. --key scopes it to that Key; --handle posts it as an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			as.depthSet = cmd.Flags().Changed("depth")
			return runConversationSend(cmd, g, keyQuery, as, args[0])
		},
	}
	send.Flags().StringVar(&keyQuery, "key", "", "curve:hex, a unique public-key prefix, or the Conversation ID")
	send.Flags().StringVar(&as.handle, "handle", "", "@handle: post as that agent, for an external runner; it must be included here")
	send.Flags().Uint32Var(&as.depth, "depth", 1, "with --handle, the reply's depth in its chain: 1 to max_depth")
	send.Flags().BoolVar(&as.detach, "detach", false, "return once the message commits, without streaming the agents it mentions")
	root.AddCommand(send)
	follow := &cobra.Command{
		Use:   "follow",
		Short: "Stream the whole output of agents in a Conversation, then their messages, on this Node, chain after chain (requires serve); Ctrl-C detaches and cancels nothing. --key scopes it to that Key",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runConversationFollow(cmd, g, keyQuery) },
	}
	follow.Flags().StringVar(&keyQuery, "key", "", "curve:hex, a unique public-key prefix, or the Conversation ID")
	root.AddCommand(follow)
	cancel := &cobra.Command{
		Use:   "cancel",
		Short: "Cancel the chain of agents running in a Conversation on this Node (requires serve). --key scopes it to that Key",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runConversationCancel(cmd, g, keyQuery) },
	}
	cancel.Flags().StringVar(&keyQuery, "key", "", "curve:hex, a unique public-key prefix, or the Conversation ID")
	root.AddCommand(cancel)
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

// sendAs is `conversation send`'s --handle, --depth and --detach.
type sendAs struct {
	handle   string
	depth    uint32
	depthSet bool
	detach   bool
}

// parseHandle is the Handle the flags name, without its "@", or "" for a
// person. Whether it may answer here, and at that depth, is serve's to check
// against config.toml.
func (a sendAs) parseHandle() (string, error) {
	if a.handle == "" {
		if a.depthSet {
			return "", exitUsagef("--depth is only for an agent's message; pass --handle @handle")
		}
		return "", nil
	}
	h, ok := strings.CutPrefix(a.handle, "@")
	if !ok {
		return "", exitUsagef("--handle %q must be @handle", a.handle)
	}
	if err := config.ValidateHandle(h); err != nil {
		return "", exitUsagef("--handle: %w", err)
	}
	if a.depth < 1 {
		return "", exitUsagef("--depth %d must be at least 1", a.depth)
	}
	return h, nil
}

func runConversationSend(cmd *cobra.Command, g *globals, keyQuery string, as sendAs, text string) error {
	if _, err := cosl.Freeform(text); err != nil {
		return exitUsagef("%w", err)
	}
	handle, err := as.parseHandle()
	if err != nil {
		return err
	}
	conv, err := queriedConversation(g, keyQuery)
	if err != nil {
		return err
	}
	client := serve.Client{Socket: g.paths().Socket}
	ctx := cmd.Context()
	// A person's mention runs a chain of agents. Watch before sending, so
	// the chain cannot start unseen (ADR 0015).
	var stream *serve.ChainStream
	if handle == "" && !as.detach && len(mention.Handles(text)) > 0 {
		if stream, err = client.ConversationStream(ctx, conv); err != nil {
			return sendError(err)
		}
		defer stream.Close()
	}
	var v serve.SendView
	if handle == "" {
		v, err = client.ConversationSend(ctx, text, conv)
	} else {
		v, err = client.ConversationSendAs(ctx, text, handle, as.depth, conv)
	}
	if err != nil {
		return sendError(err)
	}
	if g.json && stream != nil {
		if err := json.NewEncoder(g.app.Stdout).Encode(v); err != nil {
			return err
		}
	} else if g.json {
		return g.emitJSON(v)
	} else {
		fmt.Fprintf(g.app.Stdout, "sent as seq %d (msg_id %s)\n", v.Seq, v.MsgID)
	}
	if stream == nil {
		return nil
	}
	err = followChain(g, stream, v.Seq)
	if ctx.Err() != nil {
		// Ctrl-C: cancel this message's chain, running or still queued.
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, cerr := client.ConversationCancel(cctx, conv, v.Seq); cerr != nil {
			return exitOperationalf("interrupted, but cancelling the agents failed: %w", cerr)
		}
		return exitOperationalf("cancelled: nothing is committed for the agent that was answering, and the rest of the chain is dropped; your message stays")
	}
	return err
}

// followChain prints the chain the message at seq started, until it ends:
// each agent's message as it commits, labelled @handle:, with skips and
// silences, but not its output; or under --json each event as a line.
// Other chains in the Conversation are not this send's.
func followChain(g *globals, stream *serve.ChainStream, seq uint64) error {
	cp := newChainPrinter(g, false)
	var failed error
	for {
		ev, err := stream.Next()
		if err != nil {
			return exitOperationalf("lost the serve while the agents were answering: %w; check `conversation log`", err)
		}
		if ev.Chain != seq {
			continue
		}
		if err := cp.print(ev); err != nil {
			return err
		}
		switch ev.Type {
		case serve.EventAgentError:
			failed = exitOperationalf("@%s: %s; nothing is committed for it", ev.Handle, ev.Error)
		case serve.EventChainEnd:
			if failed == nil && ev.Error != "" {
				return exitOperationalf("%s; check `conversation log`", ev.Error)
			}
			if failed == nil && ev.Cancelled {
				return exitOperationalf("the agents were cancelled; nothing is committed for the one that was answering")
			}
			return failed
		}
	}
}

// runConversationFollow prints every chain in the Conversation as it
// streams, until Ctrl-C, which only detaches: the chain runs on and its
// replies still commit (ADR 0015).
func runConversationFollow(cmd *cobra.Command, g *globals, keyQuery string) error {
	conv, err := queriedConversation(g, keyQuery)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	stream, err := serve.Client{Socket: g.paths().Socket}.ConversationStream(ctx, conv)
	if err != nil {
		return serveError(err)
	}
	defer stream.Close()
	cp := newChainPrinter(g, true)
	p := g.out()
	for {
		ev, err := stream.Next()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return exitOperationalf("lost the serve: %w", err)
		}
		if err := cp.print(ev); err != nil {
			return err
		}
		if g.json {
			continue
		}
		switch ev.Type {
		case serve.EventChainStart:
			fmt.Fprintln(g.app.Stdout, p.dim(fmt.Sprintf("[agents answering seq %d]", ev.Chain)))
		case serve.EventAgentError:
			fmt.Fprintf(g.app.Stderr, "%s @%s: %s; nothing is committed for it\n", paletteFor(g.app.Stderr, g.app.Getenv).concern("defios:"), ev.Handle, ev.Error)
		case serve.EventChainEnd:
			if ev.Error != "" {
				fmt.Fprintln(g.app.Stdout, p.concern("["+ev.Error+"]"))
			} else if ev.Cancelled {
				fmt.Fprintln(g.app.Stdout, p.dim("[cancelled; nothing is committed for the agent that was answering]"))
			}
		}
	}
}

// runConversationCancel cancels the chain running in the Conversation, and
// says whether there was one.
func runConversationCancel(cmd *cobra.Command, g *globals, keyQuery string) error {
	conv, err := queriedConversation(g, keyQuery)
	if err != nil {
		return err
	}
	v, err := serve.Client{Socket: g.paths().Socket}.ConversationCancel(cmd.Context(), conv, 0)
	if err != nil {
		return serveError(err)
	}
	if g.json {
		return g.emitJSON(v)
	}
	if v.Cancelled {
		fmt.Fprintln(g.app.Stdout, "cancelled the running chain: nothing is committed for the agent that was answering, and the rest of the chain is dropped")
	} else {
		fmt.Fprintln(g.app.Stdout, "no chain was running")
	}
	return nil
}

// queriedConversation is the Conversation --key names, or nil for
// Group-level talk.
func queriedConversation(g *globals, keyQuery string) ([]byte, error) {
	if keyQuery == "" {
		return nil, nil
	}
	return resolveConversation(g, keyQuery)
}

// chainPrinter prints chain events as they stream: with output set, each
// agent's output as it is generated, a line per kind labelled
// @handle (kind):, thinking and tool calls dimmed; then what it says when
// that commits, labelled @handle:. Under --json each event is a line.
type chainPrinter struct {
	g      *globals
	enc    *json.Encoder
	output bool   // print the agents' output, not only their messages
	handle string // the agent answering, once one starts
	kind   string // of the line of output open, if one is
}

func newChainPrinter(g *globals, output bool) *chainPrinter {
	return &chainPrinter{g: g, enc: json.NewEncoder(g.app.Stdout), output: output}
}

func (c *chainPrinter) print(ev serve.ChainEvent) error {
	if c.g.json {
		return c.enc.Encode(ev)
	}
	p := c.g.out()
	out := c.g.app.Stdout
	label := func() string { return p.primary("@"+c.handle+":") + " " }
	endLine := func() {
		if c.kind != "" {
			fmt.Fprintln(out)
		}
		c.kind = ""
	}
	switch ev.Type {
	case serve.EventAgentStart:
		c.handle, c.kind = ev.Handle, ""
	case serve.EventAgentDelta:
		if !c.output {
			break
		}
		show := func(s string) string { return s }
		if ev.Kind != agent.KindText {
			show = p.dim
		}
		if ev.Kind != c.kind {
			endLine()
			fmt.Fprint(out, show("@"+c.handle+" ("+ev.Kind+"):")+" ")
			c.kind = ev.Kind
		}
		fmt.Fprint(out, show(ev.Text))
	case serve.EventAgentReset:
		if c.output {
			endLine()
			fmt.Fprintln(out, p.dim("[retrying; the model call starts over]"))
		}
	case serve.EventAgentCommit:
		endLine()
		if ev.Text != "" {
			fmt.Fprintln(out, label()+ev.Text)
		}
		c.handle = ""
	case serve.EventAgentSilent:
		endLine()
		fmt.Fprintln(out, p.dim("@"+ev.Handle+" stayed silent"))
		c.handle = ""
	case serve.EventAgentSkipped:
		fmt.Fprintln(out, p.dim("@"+ev.Handle+" skipped: "+ev.Reason))
	case serve.EventAgentError, serve.EventChainEnd:
		endLine() // the reply was cut off
		c.handle = ""
	}
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
		fmt.Fprintln(g.app.Stderr, paletteFor(g.app.Stderr, g.app.Getenv).caution("warning:")+" this Node's prune policy is off, so a later snapshot restore from a Node that kept this talk brings it back; set `prune = true` in this Group's config.toml to keep it pruned")
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
// the FSM rejected is a protocol reject (3); bad text or a Handle that may
// not write here is usage (1);
// everything else, including no leader, is operational (2).
func sendError(err error) error {
	switch {
	case errors.Is(err, raftx.ErrNotInserted):
		return &ExitError{Code: ExitProtocol, Err: err}
	case errors.Is(err, cosl.ErrBadText), errors.Is(err, serve.ErrBadHandle):
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
	// Handle is the agent that wrote it, "" for a person, and Depth its
	// place in a chain, 0 for a person (ADR 0015). A pruned row has
	// neither: its payload, which named them, is gone.
	Handle      *string `json:"handle,omitempty"`
	Depth       *uint32 `json:"depth,omitempty"`
	Text        string  `json:"text"`
	ContentType string  `json:"content_type"`
	BaseSeq     uint64  `json:"base_seq,omitempty"` // app.recap only
	Covers      int     `json:"covers,omitempty"`   // app.recap only
	Superseded  bool    `json:"superseded,omitempty"`
	Pruned      bool    `json:"pruned,omitempty"` // no text: only the row's metadata remains
}

// leanLine is one `conversation log --lean` entry: who said it and what.
type leanLine struct {
	SenderDisplayName string `json:"sender_display_name"`
	Handle            string `json:"handle,omitempty"` // an agent's only
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
		return emitJSONL(g, lines)
	}
	p := g.out()
	for _, l := range lines {
		text := cosl.EscapeNewlines(l.Text)
		switch {
		case l.Pruned:
			text = p.dim("[pruned]")
		case l.MsgType == cosl.TypeName(cosl.TypeAppRecap):
			text = p.dim(fmt.Sprintf("[recap of %d messages to seq %d]", l.Covers, l.BaseSeq)) + " " + text
		}
		who := p.primary(l.SenderDisplayName)
		if l.Handle != nil {
			who += " " + p.primary(handleLabel(*l.Handle))
		}
		fmt.Fprintf(g.app.Stdout, "%s %s %s  %s\n", p.primary(fmt.Sprint(l.Seq)), p.dim(l.CreatedAt), who, text)
	}
	return nil
}

// handleLabel is how a message's Handle shows: @handle for an agent's, user
// for a person's.
func handleLabel(handle string) string {
	if handle == "" {
		return "user"
	}
	return "@" + handle
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
		l.Handle, l.Depth = new(string), new(uint32)
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
		l.Handle, l.Depth = &ff.Handle, &ff.Depth
	}
	return l, nil
}

// emitLean prints `<display name>: <text>`, or `<display name>/@handle:
// <text>` for an agent's, one entry per line, and nothing
// else: timestamps are advisory and never ordering, while who said what is
// load-bearing in a multi-party Conversation.
func emitLean(g *globals, lines []messageLine) error {
	lean := make([]leanLine, len(lines))
	for i, l := range lines {
		lean[i] = leanLine{SenderDisplayName: l.SenderDisplayName, Text: l.Text}
		if l.Handle != nil {
			lean[i].Handle = *l.Handle
		}
	}
	if g.json {
		return emitJSONL(g, lean)
	}
	p := g.out()
	for _, l := range lean {
		fmt.Fprintf(g.app.Stdout, "%s %s\n", p.primary(recap.Speaker(l.SenderDisplayName, l.Handle)+":"), cosl.EscapeNewlines(l.Text))
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
