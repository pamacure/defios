package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"defios.com/internal/agent"
	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/mention"
	"defios.com/internal/nodedir"
	"defios.com/internal/raftx"
	"defios.com/internal/recap"
	"defios.com/internal/serve"
	"defios.com/internal/talk"
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
	send.Flags().StringVar(&keyQuery, "key", "", keyQueryHelp)
	send.Flags().StringVar(&as.handle, "handle", "", "@handle: post as that agent, for an external runner; it must be included here")
	send.Flags().Uint32Var(&as.depth, "depth", 1, "with --handle, the reply's depth in its chain: 1 to max_depth; unset, DEFIOS_DEPTH if set")
	send.Flags().BoolVar(&as.detach, "detach", false, "return once the message commits, without streaming the agents it mentions")
	root.AddCommand(send)
	follow := &cobra.Command{
		Use:   "follow",
		Short: "Stream the whole output of agents in a Conversation, then their messages, on this Node, chain after chain, and the Triggers firing there with how their Jobs end (requires serve); Ctrl-C detaches and cancels nothing. --key scopes it to that Key",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runConversationFollow(cmd, g, keyQuery) },
	}
	follow.Flags().StringVar(&keyQuery, "key", "", keyQueryHelp)
	root.AddCommand(follow)
	cancel := &cobra.Command{
		Use:   "cancel [msg-id]",
		Short: "Cancel the chain of agents running in a Conversation on this Node, or the chain, running or queued, that the person's message with this msg id (or a unique prefix of it) started (requires serve). --key scopes it to that Key",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var chain string
			if len(args) == 1 {
				chain = args[0]
			}
			return runConversationCancel(cmd, g, keyQuery, chain)
		},
	}
	cancel.Flags().StringVar(&keyQuery, "key", "", keyQueryHelp)
	root.AddCommand(cancel)
	transcriptCmd := &cobra.Command{
		Use:   "transcript <msg-id>",
		Short: "Print the Transcript of the chain that the person's message with this msg id (or a unique prefix of it) started: each step's agent at work, then how it ended; --json prints its lines as stored. Needs no serve. --key scopes it to that Key",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runConversationTranscript(g, keyQuery, args[0]) },
	}
	transcriptCmd.Flags().StringVar(&keyQuery, "key", "", keyQueryHelp)
	bindGroupFlag(g, transcriptCmd)
	root.AddCommand(transcriptCmd)
	var opts logOptions
	log := &cobra.Command{
		Use:   "log",
		Short: "Print the messages this Node holds: seq, time, display name, text. --key scopes to that Key",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runConversationLog(g, keyQuery, opts) },
	}
	log.Flags().StringVar(&keyQuery, "key", "", keyQueryHelp)
	log.Flags().IntVar(&opts.limit, "limit", 0, "only the latest N messages (with --lean, after this Node's Recap)")
	log.Flags().BoolVar(&opts.full, "full", false, "also show the messages this Node has pruned, as metadata only")
	log.Flags().BoolVar(&opts.lean, "lean", false, "only \"<display name>: <text>\" lines, this Node's RECAP.md then every message after it, as its agents read them")
	log.Flags().BoolVar(&opts.chat, "chat", false, "the talk as chat: \"<display name>: <text>\" with its newlines, every message this Node holds from the first, a blank line between")
	log.Flags().BoolVar(&opts.transcripts, "transcripts", false, "with --chat, each agent message this Node's chain said preceded by its Transcript blocks: thinking, text, tool calls and their results")
	bindGroupFlag(g, log)
	root.AddCommand(log)
	var baseSeq uint64
	var text string
	recapCmd := &cobra.Command{
		Use:   "recap --base-seq <n> --text <text>",
		Short: "Write this Node's RECAP.md, its own account of the talk up to --base-seq, which its agents read in place of that talk. Nothing reaches the COSL. --key scopes it to that Key",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return runConversationRecap(g, keyQuery, baseSeq, text)
		},
	}
	recapCmd.Flags().StringVar(&keyQuery, "key", "", keyQueryHelp)
	recapCmd.Flags().Uint64Var(&baseSeq, "base-seq", 0, "the highest seq the Recap accounts for: a message of this Conversation")
	recapCmd.Flags().StringVar(&text, "text", "", "the account of the talk it covers")
	recapCmd.MarkFlagRequired("base-seq")
	recapCmd.MarkFlagRequired("text")
	bindGroupFlag(g, recapCmd)
	root.AddCommand(recapCmd)
	var cut pruneCut
	prune := &cobra.Command{
		Use:   "prune --before <seq> | --older-than <age>",
		Short: "Destroy, on this Node only, the payloads of the talk at or below a seq, or older than an age. --key scopes it to that Key",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runConversationPrune(g, keyQuery, cut) },
	}
	prune.Flags().StringVar(&keyQuery, "key", "", keyQueryHelp)
	prune.Flags().Uint64Var(&cut.before, "before", 0, "prune the talk at or below this seq")
	prune.Flags().StringVar(&cut.olderThan, "older-than", "", "prune the talk its sender wrote longer ago than this: whole days like 30d, or a Go duration like 36h")
	prune.MarkFlagsOneRequired("before", "older-than")
	prune.MarkFlagsMutuallyExclusive("before", "older-than")
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

// handleAndDepth is the Handle the flags name, without its "@", or "" for no
// agent's message, and the depth it is written at: --depth, else
// DEFIOS_DEPTH under an exec Job (ADR 0032), else 1 for an agent and 0 for
// a person. Whether it may answer here, and at that depth, is serve's to
// check against config.toml.
func (a sendAs) handleAndDepth(g *globals) (string, uint32, error) {
	depth, isJob, err := jobDepth(g)
	if err != nil {
		return "", 0, err
	}
	if a.handle == "" {
		if a.depthSet {
			return "", 0, exitUsagef("--depth is only for an agent's message; pass --handle @handle")
		}
		return "", depth, nil
	}
	h, ok := strings.CutPrefix(a.handle, "@")
	if !ok {
		return "", 0, exitUsagef("--handle %q must be @handle", a.handle)
	}
	if err := config.ValidateHandle(h); err != nil {
		return "", 0, exitUsagef("--handle: %w", err)
	}
	if a.depthSet || !isJob {
		depth = a.depth
	}
	if depth < 1 {
		return "", 0, exitUsagef("--depth %d must be at least 1", depth)
	}
	return h, depth, nil
}

// jobDepth is DEFIOS_DEPTH, the depth serve gives an exec Job to write at
// (ADR 0032), and whether it is set.
func jobDepth(g *globals) (uint32, bool, error) {
	v := g.app.Getenv("DEFIOS_DEPTH")
	if v == "" {
		return 0, false, nil
	}
	d, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, false, exitUsagef("DEFIOS_DEPTH %q must be a depth: 0 or more", v)
	}
	return uint32(d), true, nil
}

func runConversationSend(cmd *cobra.Command, g *globals, keyQuery string, as sendAs, text string) error {
	if _, err := cosl.Freeform(text); err != nil {
		return exitUsagef("%w", err)
	}
	handle, depth, err := as.handleAndDepth(g)
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
	v, err := client.ConversationSendAs(ctx, text, handle, depth, conv)
	if err != nil {
		return sendError(err)
	}
	att := attached{client: client, conv: conv, stream: stream, chain: v.MsgID, cancelled: "; your message stays"}
	if !v.Queued {
		return att.run(ctx, g, v, fmt.Sprintf("sent as seq %d (msg_id %s)", v.Seq, v.MsgID))
	}
	// A queued message's chain runs now, on this Node's view of the talk
	// (ADR 0025); the exit still says it is not on the log.
	if err := att.run(ctx, g, v, fmt.Sprintf("queued (msg_id %s): %s", v.MsgID, queuedNote)); err != nil {
		return err
	}
	return exitQueued()
}

// attached is a command that started or queued a chain and, with a stream
// opened before it did, prints it until it ends.
type attached struct {
	client serve.Client
	conv   []byte
	stream *serve.ChainStream // nil when detached
	chain  string             // the hex msg_id that started it
	// cancelled ends what Ctrl-C reports once the chain is cancelled.
	cancelled string
}

// run prints view, the command's own reply, as line or under --json as
// JSON; then, attached, the chain until it ends. Ctrl-C cancels the chain,
// running or still queued.
func (a attached) run(ctx context.Context, g *globals, view any, line string) error {
	switch {
	case g.json && a.stream == nil:
		return g.emitJSON(view)
	case g.json:
		if err := json.NewEncoder(g.app.Stdout).Encode(view); err != nil {
			return err
		}
	default:
		fmt.Fprintln(g.app.Stdout, line)
	}
	if a.stream == nil {
		return nil
	}
	err := followChain(g, a.stream, a.chain)
	if ctx.Err() != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, cerr := a.client.ConversationCancel(cctx, a.conv, a.chain); cerr != nil {
			return exitOperationalf("interrupted, but cancelling the agents failed: %w", cerr)
		}
		return exitOperationalf("cancelled: nothing is committed for the agent that was answering, and the rest of the chain is dropped%s", a.cancelled)
	}
	return err
}

// followChain prints the chain the message with msg_id chain started, until
// it ends: each agent's message as it commits, labelled @handle:, with skips
// and silences, but not its output; or under --json those events as lines.
// Other chains in the Conversation are not this send's.
func followChain(g *globals, stream *serve.ChainStream, chain string) error {
	cp := newChainPrinter(g, false)
	var failed error
	for {
		ev, err := stream.Next()
		if err != nil {
			return exitOperationalf("lost the serve while the agents were answering: %w; check `conversation log`", err)
		}
		if ev.Chain != chain {
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
// streams, and the Triggers firing there with the Jobs they run, until
// Ctrl-C, which only detaches: the chain runs on and its
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
		case serve.EventTriggerFire, serve.EventJobEnd:
			fmt.Fprintln(g.app.Stdout, triggerLine(p, ev))
		case serve.EventChainStart:
			fmt.Fprintln(g.app.Stdout, p.dim(fmt.Sprintf("[agents answering msg_id %s]", shortChain(ev.Chain))))
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

// runConversationCancel cancels the chain running in the Conversation, or
// with chain the one, running or queued, whose msg_id starts with it, and
// says whether there was one.
func runConversationCancel(cmd *cobra.Command, g *globals, keyQuery, chain string) error {
	chain, err := chainPrefix(chain)
	if err != nil {
		return err
	}
	conv, err := queriedConversation(g, keyQuery)
	if err != nil {
		return err
	}
	v, err := serve.Client{Socket: g.paths().Socket}.ConversationCancel(cmd.Context(), conv, chain)
	if errors.Is(err, serve.ErrAmbiguousChain) {
		return exitUsagef("%w", err)
	}
	if err != nil {
		return serveError(err)
	}
	if g.json {
		return g.emitJSON(v)
	}
	switch {
	case v.Cancelled && chain == "":
		fmt.Fprintln(g.app.Stdout, "cancelled the running chain: nothing is committed for the agent that was answering, and the rest of the chain is dropped")
	case v.Cancelled:
		fmt.Fprintf(g.app.Stdout, "cancelled the chain of msg_id %s: nothing is committed for the agent that was answering, and the rest of the chain is dropped\n", shortChain(v.Chain))
	case chain == "":
		fmt.Fprintln(g.app.Stdout, "no chain was running")
	default:
		fmt.Fprintf(g.app.Stdout, "no chain of msg_id %s is running or queued\n", chain)
	}
	return nil
}

// chainPrefix is a chain's msg_id prefix as given, in lower case, or a
// usage error if it is not hex. "" is no chain named.
func chainPrefix(chain string) (string, error) {
	p, err := serve.ChainPrefix(chain)
	if err != nil {
		return "", exitUsagef("%w", err)
	}
	return p, nil
}

// shortChain is a chain's msg_id as printed: a prefix that names it.
func shortChain(chain string) string { return chain[:min(8, len(chain))] }

// isOutput reports whether ev is part of an agent's output as it streams,
// which only follow prints: an agent's start, a delta or a reset.
func isOutput(ev serve.ChainEvent) bool {
	switch ev.Type {
	case serve.EventAgentStart, serve.EventAgentDelta, serve.EventAgentReset:
		return true
	}
	return false
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
// that commits, labelled @handle:. Under --json each event is a line, all
// of them with output set; without it, all but the output (isOutput).
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
		if !c.output && isOutput(ev) {
			return nil
		}
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

// recapLine is `conversation recap`'s result: the Recap's base seq and
// the RECAP.md it wrote.
type recapLine struct {
	BaseSeq uint64 `json:"base_seq"`
	Path    string `json:"path"`
}

// runConversationRecap writes this Node's RECAP.md for the Conversation
// (ADR 0024). Like Pruning it is Node-local and appends nothing, so it needs
// no serve: it reads this Node's replica only to check that baseSeq names an
// app.freeform of the Conversation.
func runConversationRecap(g *globals, keyQuery string, baseSeq uint64, text string) error {
	if err := cosl.CheckText(text); err != nil {
		return exitUsagef("%w", err)
	}
	store, err := openReplica(g)
	if err != nil {
		return err
	}
	defer store.Close()
	conv, err := optionalConversation(g, store, keyQuery)
	if err != nil {
		return err
	}
	rows, err := store.Log(cosl.Query{GroupLevel: conv == nil, ConversationID: conv})
	if err != nil {
		return exitOperational(err)
	}
	if err := recap.Check(rows, baseSeq); err != nil {
		return exitUsagef("%w", err)
	}
	path, err := recapPath(g, conv)
	if err != nil {
		return err
	}
	if err := recap.Write(path, recap.Recap{BaseSeq: baseSeq, Text: text}); err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(recapLine{BaseSeq: baseSeq, Path: path})
	}
	fmt.Fprintf(g.app.Stdout, "wrote this Node's Recap of the talk up to seq %d to %s\n", baseSeq, path)
	return nil
}

// optionalConversation is the Conversation keyQuery names, or nil for
// Group-level talk when it is empty.
func optionalConversation(g *globals, store *cosl.Store, keyQuery string) ([]byte, error) {
	if keyQuery == "" {
		return nil, nil
	}
	return conversationID(g, store, keyQuery)
}

// recapPath is this Node's RECAP.md for conv, nil for Group-level talk, in
// the Group the command targets.
func recapPath(g *globals, conv []byte) (string, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return "", err
	}
	return g.paths().RecapFile(alias, nodedir.ConversationName(conv)), nil
}

// readRecap is this Node's Recap of conv, nil if it has none. One that does
// not parse is an operational error naming the file.
func readRecap(g *globals, conv []byte) (*recap.Recap, error) {
	path, err := recapPath(g, conv)
	if err != nil {
		return nil, err
	}
	r, err := recap.Read(path)
	if err != nil {
		return nil, exitOperational(err)
	}
	return r, nil
}

// pruneCut is which talk `conversation prune` destroys: at or below seq
// before, or older than olderThan; exactly one is set.
type pruneCut struct {
	before    uint64
	olderThan string
}

// pruneLine is `conversation prune`'s result: how many messages it pruned
// now, and which cut it pruned by.
type pruneLine struct {
	Before    uint64 `json:"before,omitempty"`
	OlderThan string `json:"older_than,omitempty"`
	Messages  int    `json:"messages"`
}

// runConversationPrune writes this Node's replica directly: Pruning is
// Node-local, appends nothing and tells no other Node, so it needs no serve
// (ADR 0012, ADR 0024). It warns when it destroys talk this Node's RECAP.md
// does not account for: this Node's agents lose that talk with no account
// in its place.
func runConversationPrune(g *globals, keyQuery string, cut pruneCut) error {
	var age config.Age
	if cut.olderThan != "" {
		var err error
		if age, err = config.ParseAge(cut.olderThan); err != nil {
			return exitUsagef("--older-than: %w", err)
		}
	}
	store, err := openReplica(g)
	if err != nil {
		return err
	}
	defer store.Close()
	policy, err := prunePolicy(g)
	if err != nil {
		return err
	}
	conv, err := optionalConversation(g, store, keyQuery)
	if err != nil {
		return err
	}
	rc, err := readRecap(g, conv)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Duration(age))
	cuts := func(r cosl.Row) bool { return r.Seq <= cut.before }
	if age != 0 {
		cuts = func(r cosl.Row) bool { return r.CreatedAt < cutoff.UnixNano() }
	}
	rows, err := store.Log(cosl.Query{GroupLevel: conv == nil, ConversationID: conv, MsgTypes: []uint32{cosl.TypeAppFreeform}})
	if err != nil {
		return exitOperational(err)
	}
	lost := slices.ContainsFunc(rows, func(r cosl.Row) bool { return !r.Pruned() && cuts(r) && !rc.Covers(r.Seq) })
	var n int
	if age != 0 {
		n, err = store.PruneOlderThan(conv, cutoff)
	} else {
		n, err = store.Prune(conv, cut.before)
	}
	if err != nil {
		return exitOperational(err)
	}
	warn := paletteFor(g.app.Stderr, g.app.Getenv).caution("warning:")
	switch {
	case lost && rc == nil:
		fmt.Fprintln(g.app.Stderr, warn+" this Node has no RECAP.md for this talk, so its agents lose what was pruned with no account of it in its place; `conversation recap` writes one")
	case lost:
		fmt.Fprintf(g.app.Stderr, "%s this pruned talk past seq %d, the base_seq of this Node's RECAP.md, so its agents lose it with no account of it in its place\n", warn, rc.BaseSeq)
	}
	if policy == 0 {
		fmt.Fprintln(g.app.Stderr, warn+` this Node has no prune policy, so a later snapshot restore from a Node that kept this talk brings it back; set an age, such as prune = "90d", in this Group's config.toml to keep it pruned`)
	}
	if g.json {
		line := pruneLine{Before: cut.before, Messages: n}
		if age != 0 {
			line = pruneLine{OlderThan: age.String(), Messages: n}
		}
		return g.emitJSON(line)
	}
	if age != 0 {
		fmt.Fprintf(g.app.Stdout, "pruned %d messages older than %s\n", n, age)
	} else {
		fmt.Fprintf(g.app.Stdout, "pruned %d messages up to seq %d\n", n, cut.before)
	}
	return nil
}

// prunePolicy is the age this Node's config.toml for the Current Group sets
// as `prune`. A Node with no config.toml has none: it is archival.
func prunePolicy(g *globals) (config.Age, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return 0, err
	}
	e, err := config.Load(g.paths(), alias, nil, nil)
	if err != nil {
		return 0, exitOperational(err)
	}
	return e.Group.Prune, nil
}

// queuedNote says what a queued write means.
const queuedNote = "no leader is known, so it waits in this Node's Outbox and commits when a majority of the Group is back; it is not on the log yet"

// queuedExit is exitQueued when queued, else success.
func queuedExit(queued bool) error {
	if queued {
		return exitQueued()
	}
	return nil
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

// messageLine is one `conversation log` row, human or JSON: an app.freeform.
// A queued one, in this Node's Outbox, has no seq or term (ADR 0025).
type messageLine struct {
	Seq               uint64 `json:"seq,omitempty"`
	Term              uint64 `json:"term,omitempty"`
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
	Pruned      bool    `json:"pruned,omitempty"`     // no text: only the row's metadata remains
	Unreadable  bool    `json:"unreadable,omitempty"` // its payload does not parse: no text
	Queued      bool    `json:"queued,omitempty"`     // in this Node's Outbox, not on the log yet
}

// leanLine is one `conversation log --lean` entry: who said what, or this
// Node's Recap with the seq it accounts up to; and one `--chat` entry.
type leanLine struct {
	SenderDisplayName string `json:"sender_display_name,omitempty"` // talk only
	Handle            string `json:"handle,omitempty"`              // an agent's only
	BaseSeq           uint64 `json:"base_seq,omitempty"`            // the Recap only
	Text              string `json:"text"`
}

// logOptions are `conversation log`'s reader flags.
type logOptions struct {
	limit            int
	full, lean, chat bool
	transcripts      bool // with chat
}

// runConversationLog prints this Node's view of the Conversation's talk
// (ADR 0025), unfolded (ADR 0024): every message this replica holds that is
// not pruned, and under --full the pruned ones too as placeholders, then
// those queued in its Outbox. --lean is what this Node's agents read, and
// --chat the talk as chat.
func runConversationLog(g *globals, keyQuery string, opts logOptions) error {
	if opts.limit < 0 {
		return exitUsagef("--limit must be >= 0")
	}
	if countTrue(opts.full, opts.lean, opts.chat) > 1 {
		return exitUsagef("--full, --lean and --chat are different views; pass one")
	}
	if opts.transcripts && !opts.chat {
		return exitUsagef("--transcripts expands --chat; pass both")
	}
	alias, rec, err := loadGroup(g)
	if err != nil {
		return err
	}
	store, err := openReplica(g)
	if err != nil {
		return err
	}
	defer store.Close()
	conv, err := optionalConversation(g, store, keyQuery)
	if err != nil {
		return err
	}
	roster := rec.Roster()
	reader := talk.Reader{Paths: g.paths(), Alias: alias, Store: store, Mine: talk.File(g.paths().OutboxFile(alias)), Roster: &roster}
	v, err := reader.Read(conv)
	if err != nil {
		return exitOperational(err)
	}
	switch {
	case opts.lean:
		l, err := v.Lean()
		if err != nil {
			return exitOperational(err)
		}
		return emitLean(g, l, opts.limit)
	case opts.chat:
		msgs := v.Chat().Last(opts.limit)
		chat := make([]talk.Expanded, len(msgs))
		for i, m := range msgs {
			chat[i].Message = m
		}
		if opts.transcripts {
			if chat, err = reader.Expand(conv, msgs); err != nil {
				return exitOperational(err)
			}
		}
		return emitChat(g, chat, opts.transcripts)
	}
	msgs := v.Messages
	if !opts.full {
		msgs = slices.DeleteFunc(slices.Clone(msgs), func(m talk.Message) bool { return m.Pruned })
	}
	msgs = msgs.Last(opts.limit)
	lines := make([]messageLine, len(msgs))
	for i, m := range msgs {
		lines[i] = newTalkLine(m)
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
		case l.Unreadable:
			text = p.dim("[unreadable]")
		}
		who := p.primary(l.SenderDisplayName)
		if l.Handle != nil {
			who += " " + p.primary(handleLabel(*l.Handle))
		}
		at := p.primary(fmt.Sprint(l.Seq))
		if l.Queued {
			at = p.dim("queued")
		}
		fmt.Fprintf(g.app.Stdout, "%s %s %s  %s\n", at, p.dim(l.CreatedAt), who, text)
	}
	return nil
}

// countTrue is how many of bs are true.
func countTrue(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// handleLabel is how a message's Handle shows: @handle for an agent's, user
// for a person's.
func handleLabel(handle string) string {
	if handle == "" {
		return "user"
	}
	return "@" + handle
}

// latest is the last n of xs, or all of them when n is 0.
func latest[T any](xs []T, n int) []T {
	if n > 0 && len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

// newTalkLine is a message as a log row: a pruned or unreadable one's
// metadata only.
func newTalkLine(m talk.Message) messageLine {
	r := m.Row
	l := messageLine{
		Seq:               r.Seq,
		Term:              r.Term,
		CreatedAt:         rfc3339(r.CreatedAt),
		Sender:            r.Sender.String(),
		SenderDisplayName: messageSender(m),
		MsgID:             m.MsgID(),
		MsgType:           cosl.TypeName(r.MsgType),
		Pruned:            m.Pruned,
		Unreadable:        m.Unreadable,
		Queued:            r.Queued(),
	}
	if m.Readable() {
		l.Text, l.ContentType = m.Text, m.ContentType
		l.Handle, l.Depth = &m.Handle, &m.Depth
	}
	return l
}

// emitLean prints the Conversation as this Node's agents read it: its
// RECAP.md as `[recap to seq <n>] <text>`, then the talk after it that
// this replica holds, the latest limit of it, as `<display name>: <text>`,
// or `<display name>/@handle: <text>` for an agent's, one entry per line
// and nothing else: timestamps are advisory and never ordering, while who
// said what is load-bearing in a multi-party Conversation.
func emitLean(g *globals, v talk.Lean, limit int) error {
	msgs := v.Messages.Last(limit)
	lean := make([]leanLine, 0, len(msgs)+1)
	if v.Recap != nil {
		lean = append(lean, leanLine{BaseSeq: v.Recap.BaseSeq, Text: v.Recap.Text})
	}
	for _, m := range msgs {
		lean = append(lean, leanLine{SenderDisplayName: messageSender(m), Handle: m.Handle, Text: m.Text})
	}
	if g.json {
		return emitJSONL(g, lean)
	}
	p := g.out()
	for _, l := range lean {
		who := p.primary(talk.Speaker(l.SenderDisplayName, l.Handle) + ":")
		if l.BaseSeq != 0 {
			who = p.dim(fmt.Sprintf("[recap to seq %d]", l.BaseSeq))
		}
		fmt.Fprintf(g.app.Stdout, "%s %s\n", who, cosl.EscapeNewlines(l.Text))
	}
	return nil
}

// chatLine is one --chat entry; with --transcripts, also the blocks of
// the run that said it, none for the rest.
type chatLine struct {
	leanLine
	Blocks *[]json.RawMessage `json:"blocks,omitempty"`
}

// emitChat prints the talk as chat: each message as `<speaker>: <text>`,
// its newlines kept, a blank line between messages; with --json, one line
// per message. With transcripts, each agent message this Node's chain said
// is preceded by its run's blocks as conversation transcript shows them,
// indented apart from what was said, and each --json line has its blocks.
func emitChat(g *globals, msgs []talk.Expanded, transcripts bool) error {
	chat := make([]chatLine, len(msgs))
	for i, m := range msgs {
		chat[i].leanLine = leanLine{SenderDisplayName: messageSender(m.Message), Handle: m.Handle, Text: m.Text}
		if transcripts {
			blocks := append([]json.RawMessage{}, m.Blocks...)
			chat[i].Blocks = &blocks
		}
	}
	if g.json {
		return emitJSONL(g, chat)
	}
	p := g.out()
	for i, l := range chat {
		if i > 0 {
			fmt.Fprintln(g.app.Stdout)
		}
		if l.Blocks != nil {
			printBlocks(g, "  ", "@"+l.Handle, *l.Blocks)
		}
		fmt.Fprintf(g.app.Stdout, "%s %s\n", p.primary(talk.Speaker(l.SenderDisplayName, l.Handle)+":"), l.Text)
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
	return conversationID(g, store, query)
}

// conversationID is the Conversation of the Key query names in the targeted
// Group. A query naming no Key or two is a usage error; a config.toml that
// could not be read for the alias is not.
func conversationID(g *globals, store *cosl.Store, query string) ([]byte, error) {
	keys, err := key.Read(store)
	if err != nil {
		return nil, exitOperational(err)
	}
	groupAlias, err := g.resolveAlias()
	if err != nil {
		return nil, err
	}
	k, err := config.FindKey(g.paths(), groupAlias, keys, query)
	if errors.Is(err, key.ErrUnknown) || errors.Is(err, key.ErrAmbiguous) {
		return nil, exitUsagef("%w", err)
	}
	if err != nil {
		return nil, exitOperational(err)
	}
	id := key.ConversationID(k.Curve, k.PublicKey)
	return append([]byte(nil), id[:]...), nil
}

// messageSender is the display name the Roster pins for m's sender; the FSM
// only admits Roster Nodes, so the fallback is for a replica older than
// its Roster.
func messageSender(m talk.Message) string {
	if m.Sender != "" {
		return m.Sender
	}
	return m.Row.Sender.String()[:8]
}
