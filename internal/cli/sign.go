package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/chainfill"
	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/talk"
)

func signCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "sign",
		Short: "Propose, inspect and broadcast Sign Requests",
		Long:  `A Sign Request asks the Group to sign one or more transactions with one of its Keys. Proposing it records your acceptance. The Key's first t Nodes to accept sign, then a signing Session starts.`,
	}

	var (
		keyQuery, txFile, reason string
		deadline                 uint64
	)
	propose := &cobra.Command{
		Use:   "propose --key <key> --reason <text> --transactions <file>",
		Short: "Propose that a Key sign transactions (requires serve)",
		Long: `--transactions is a JSON file (or - for stdin) listing the transactions, each naming one of this Node's
Networks by alias and exactly one intent in protobuf-JSON form. The request carries the chain the alias
names (chain id, bitcoin network or solana cluster), not the alias:

  [{"network": "ethereum-mainnet",
    "ethereum": {"to": "<base64 20 bytes>", "value": "1000", "nonce": "0",
                 "gasLimit": "21000", "maxFeePerGas": "30000000000", "maxPriorityFeePerGas": "1"}}]

Every chain must be of the Key's curve.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if keyQuery == "" || txFile == "" {
				return exitUsagef("--key and --transactions are required")
			}
			depth, _, err := jobDepth(g)
			if err != nil {
				return err
			}
			txs, err := readTransactions(g, txFile)
			if err != nil {
				return err
			}
			spec := signreq.Spec{Reason: reason, DeadlineSeconds: deadline, Transactions: txs, Depth: depth}
			v, err := serve.Client{Socket: g.paths().Socket}.SignPropose(cmd.Context(), keyQuery, spec)
			if err != nil {
				return signError(err)
			}
			if g.json {
				if err := g.emitJSON(v); err != nil {
					return err
				}
				return queuedExit(v.Queued)
			}
			if v.Queued {
				fmt.Fprintf(g.app.Stdout, "queued (msg_id %s): %s\n", v.MsgID, queuedNote)
				return exitQueued()
			}
			fmt.Fprintf(g.app.Stdout, "proposed as seq %d (msg_id %s)\n", v.Seq, v.MsgID)
			return nil
		},
	}
	propose.Flags().StringVar(&keyQuery, "key", "", "curve:hex or the Conversation ID")
	propose.Flags().StringVar(&reason, "reason", "", "why the Group should sign (required)")
	propose.Flags().StringVar(&txFile, "transactions", "", "JSON file of transactions, or - for stdin")
	propose.Flags().Uint64Var(&deadline, "deadline", signreq.DefaultDeadlineSeconds, "seconds the request stays open")

	list := &cobra.Command{
		Use:   "list",
		Short: "List Sign Requests, open and past (requires serve)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rows, err := serve.Client{Socket: g.paths().Socket}.SignList(cmd.Context())
			if err != nil {
				return signError(err)
			}
			if g.json {
				return g.emitJSON(rows)
			}
			p := g.out()
			for _, s := range rows {
				fmt.Fprintf(g.app.Stdout, "%s  %s  %s  you %s  txs=%d  broadcasts=%d%s  %s\n", p.primary(s.MsgID[:8]), p.status(s.Status), stancesOf(s), p.status(s.Stance), s.Transactions, s.Broadcasts, p.dim(fully(s.FullyBroadcast)), s.Reason)
			}
			return nil
		},
	}

	inspect := &cobra.Command{
		Use:   "inspect <msg-id>",
		Short: "Decode one Sign Request (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := serve.Client{Socket: g.paths().Socket}.SignInspect(cmd.Context(), args[0])
			if err != nil {
				return signError(err)
			}
			if g.json {
				return g.emitJSON(d)
			}
			out, p := g.app.Stdout, g.out()
			fmt.Fprintf(out, "sign_request %s\nstatus %s\nstance %s\nproposer %s\ncreated_at %s\ndeadline %ds\nconversation %s\nreason %s\n",
				p.primary(d.MsgID), p.status(d.Status), p.status(d.Stance), talk.Speaker(d.Proposer, d.ProposerHandle), p.dim(d.CreatedAt), d.DeadlineSeconds, d.Conversation, d.Reason)
			fmt.Fprintf(out, "accepted %s\n", acceptedOf(d.Summary))
			for _, a := range d.Accepts {
				fmt.Fprintf(out, "  %s %s\n", talk.Speaker(a.Node, a.Handle), p.dim(fmt.Sprintf("(seq %d)", a.Seq)))
			}
			fmt.Fprintf(out, "refused %d\n", len(d.Refusals))
			for _, a := range d.Refusals {
				fmt.Fprintf(out, "  %s: %s %s\n", talk.Speaker(a.Node, a.Handle), cosl.EscapeNewlines(a.Reason), p.dim(fmt.Sprintf("(seq %d)", a.Seq)))
			}
			if d.Session != "" {
				fmt.Fprintf(out, "session %s\n", d.Session)
			}
			for _, b := range d.BroadcastReceipts {
				fmt.Fprintf(out, "broadcast by %s at %s %s\n", b.Broadcaster, p.dim(b.At), p.dim(fmt.Sprintf("(seq %d)", b.Seq)))
				for _, t := range b.Transactions {
					fmt.Fprintf(out, "  tx %d  %s  %s\n", t.Index, t.Chain, t.TxHash)
				}
			}
			for i, t := range d.Details {
				fmt.Fprintf(out, "\n%s %s\n", p.primary(fmt.Sprintf("transaction %d: %s", i, t.Chain)), p.dim("("+t.Type+")"))
				for _, l := range t.Lines {
					fmt.Fprintf(out, "  %s\n", l)
				}
				for j, dg := range t.Digests {
					fmt.Fprintf(out, "  digest %d %s\n", j, dg)
				}
			}
			return nil
		},
	}

	accept := &cobra.Command{
		Use:   "accept <msg-id>",
		Short: "Accept a Sign Request, offering to sign it (requires serve)",
		Long: `Accepting appends a sign.accept and lets this Node's engine take part in signing. The proposer's
accept is implied by proposing. The first t Nodes to accept, in log order, are the signers: once they
exist one of them starts the signing Session, which signs each Digest and completes as soon as a
signature verifies against the Key. A Node that accepts after that is not a signer. A Node accepts only
a request for chains it has a Network for (see network); the refusal names the chain.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := serve.Client{Socket: g.paths().Socket}.SignAccept(cmd.Context(), args[0])
			if err != nil {
				return signError(err)
			}
			if g.json {
				if err := g.emitJSON(v); err != nil {
					return err
				}
				return queuedExit(v.Queued)
			}
			if v.Queued {
				fmt.Fprintf(g.app.Stdout, "accept of %s queued: %s\n", args[0], queuedNote)
				return exitQueued()
			}
			fmt.Fprintf(g.app.Stdout, "accepted %s\n", args[0])
			return nil
		},
	}

	var refuseReason string
	refuse := &cobra.Command{
		Use:   "refuse <msg-id> --reason <text>",
		Short: "Refuse a Sign Request, saying why (requires serve)",
		Long: `Refusing appends a sign.refuse carrying your reason, so every Node sees that this Node judged the request
and turned it down. A Node's first accept or refusal of a request is its stance and it is final: a Node
that refused cannot accept, one that accepted or proposed cannot refuse, and no Node can refuse once t
Nodes have accepted or the request has closed. A request is refused once more Nodes refuse it than the
Key can spare (n − t), since t accepts can then never exist.

Refusing is not declining: sign decline aborts a signing Session that has already started, an emergency
stop, and before one starts it does nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if refuseReason == "" {
				return exitUsagef("--reason is required")
			}
			v, err := serve.Client{Socket: g.paths().Socket}.SignRefuse(cmd.Context(), args[0], refuseReason)
			if err != nil {
				return signError(err)
			}
			if g.json {
				if err := g.emitJSON(v); err != nil {
					return err
				}
				return queuedExit(v.Queued)
			}
			if v.Queued {
				fmt.Fprintf(g.app.Stdout, "refusal of %s queued: %s\n", args[0], queuedNote)
				return exitQueued()
			}
			fmt.Fprintf(g.app.Stdout, "refused %s\n", args[0])
			return nil
		},
	}
	refuse.Flags().StringVar(&refuseReason, "reason", "", "why this Node refuses (required)")

	decline := &cobra.Command{
		Use:   "decline <msg-id>",
		Short: "Abort the signing Session for a Sign Request (requires serve)",
		Long: `Declining aborts the request's signing Session, an emergency stop. Before one has started there is nothing
to abort and nothing is written: an accept you already made stays on the log (the output says so). To
turn a request down before its Session, with a reason every Node sees, use sign refuse.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := serve.Client{Socket: g.paths().Socket}.SignDecline(cmd.Context(), args[0])
			if err != nil {
				return signError(err)
			}
			if g.json {
				return g.emitJSON(v)
			}
			out := g.app.Stdout
			switch v.Result {
			case session.DeclineAborted:
				fmt.Fprintf(out, "declined %s: its signing Session was aborted\n", args[0])
			case session.DeclineAcceptStands:
				fmt.Fprintf(out, "nothing declined: no Session has started and your accept of %s is already on the log, so you can still be named a signer\n", args[0])
			case session.DeclineClosed:
				fmt.Fprintf(out, "nothing declined: the Session for %s has already finished\n", args[0])
			default:
				fmt.Fprintf(out, "nothing to decline: no Session has started for %s and you have not accepted it\n", args[0])
			}
			return nil
		},
	}

	broadcast := &cobra.Command{
		Use:   "broadcast <msg-id>",
		Short: "Broadcast a signed request's transactions and record a receipt (requires serve)",
		Long: `Assembles each transaction of a signed Sign Request, groups them by chain, submits each chain's through the
rpc urls of this Node's Network for it (see network), and commits a sign.broadcast receipt for each chain.
Each endpoint is first checked to serve the request's chain (eth_chainId, getblockchaininfo, or a pinned
solana genesis hash); one that serves another is skipped and the next tried. Chains are independent: if one
fails, the others are still broadcast and receipted, the failure is reported, and the command exits non-zero.
Any Node may broadcast; the proposer goes first by convention. Broadcasting again is harmless: a node that
already has a transaction is not an error, the repeat shows as one more receipt, and a failed chain is retried.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := serve.Client{Socket: g.paths().Socket}.SignBroadcast(cmd.Context(), args[0])
			if err != nil {
				return signError(err)
			}
			if g.json {
				if err := g.emitJSON(v); err != nil {
					return err
				}
			} else {
				for _, n := range v.Chains {
					for _, t := range n.Transactions {
						repeat := ""
						if t.AlreadyKnown {
							repeat = " (already known to the network)"
						}
						fmt.Fprintf(g.app.Stdout, "%s  tx %d  %s%s\n", n.Chain, t.Index, t.TxHash, repeat)
					}
					switch {
					case n.Receipt != nil && n.Receipt.Queued:
						fmt.Fprintf(g.app.Stdout, "%s: receipt queued (msg_id %s): %s\n", n.Chain, n.Receipt.MsgID, queuedNote)
					case n.Receipt != nil:
						fmt.Fprintf(g.app.Stdout, "%s: receipt recorded as seq %d (msg_id %s)\n", n.Chain, n.Receipt.Seq, n.Receipt.MsgID)
					default:
						fmt.Fprintf(g.app.Stdout, "%s: %s %s\n", n.Chain, g.out().concern("FAILED, no receipt recorded:"), n.Error)
					}
				}
			}
			if v.Failed() {
				return exitOperationalf("some chains failed to broadcast; run sign broadcast again to retry them")
			}
			return queuedExit(slices.ContainsFunc(v.Chains, func(n serve.ChainBroadcast) bool { return n.Receipt != nil && n.Receipt.Queued }))
		},
	}

	var fillKey, fillFile string
	fill := &cobra.Command{
		Use:   "fill --key <key> --transactions <file>",
		Short: "Complete transactions from this Node's Networks, for sign propose (requires serve)",
		Long: `--transactions is read as sign propose reads it (a file, or - for stdin). Each field a transaction's
intent leaves out is filled from this Node's Network for its alias; a field present is never changed,
whatever its value (nonce 0 is kept). Nothing is proposed or written.

  ethereum  chainId, nonce, maxPriorityFeePerGas, maxFeePerGas (2 x base fee + tip), gasLimit
            (estimated from the Key's address; a revert is an error). to, value and data are yours;
            to is required. One chain's nonces rise in the list's order, the order they execute in:
            a filled one is one past the transaction before it, given or filled, and never below
            the pending nonce. Given nonces out of order, or a filled one that cannot fit below the
            next given one, are an error.
  solana    feePayer (the Key's address), recentBlockhash (finalized; it expires in about 60-90 s, so
            propose promptly), cluster and genesisHash. The instructions are yours.
  bitcoin   refused: choosing inputs and a fee needs an indexer, which is a Plugin's job.

Each endpoint is checked to serve the chain first, as broadcast does. The completed transactions go to
stdout, ready for sign propose --transactions -; what was filled, and from where, goes to stderr.
--json prints both as one object instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fillKey == "" || fillFile == "" {
				return exitUsagef("--key and --transactions are required")
			}
			txs, err := readTransactions(g, fillFile)
			if err != nil {
				return err
			}
			v, err := serve.Client{Socket: g.paths().Socket}.SignFill(cmd.Context(), fillKey, txs)
			if err != nil {
				return signError(err)
			}
			if g.json {
				return g.emitJSON(v)
			}
			out, err := json.MarshalIndent(v.Transactions, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintf(g.app.Stdout, "%s\n", out)
			for _, n := range v.Notes {
				filled := "nothing"
				if len(n.Filled) > 0 {
					filled = strings.Join(n.Filled, ", ")
				}
				from := ""
				if n.Endpoint != "" {
					from = " from " + n.Endpoint
				}
				fmt.Fprintf(g.app.Stderr, "transaction %d (%s, %s): filled %s%s\n", n.Index, n.Network, n.Chain, filled, from)
				if n.Warning != "" {
					fmt.Fprintf(g.app.Stderr, "transaction %d: %s\n", n.Index, n.Warning)
				}
			}
			return nil
		},
	}
	fill.Flags().StringVar(&fillKey, "key", "", "curve:hex or the Conversation ID")
	fill.Flags().StringVar(&fillFile, "transactions", "", "JSON file of transactions, or - for stdin")

	root.AddCommand(propose, fill, list, inspect, accept, refuse, decline, broadcast)
	return root
}

// acceptedOf is "n/t", or "n/? (Key not on this log)" when t is unknown.
func acceptedOf(s signreq.Summary) string {
	if !s.KeyKnown {
		return fmt.Sprintf("%d/? (Key not on this log)", len(s.Accepted))
	}
	return fmt.Sprintf("%d/%d", len(s.Accepted), s.Needed)
}

// stancesOf is "n/t accepted, r refused".
func stancesOf(s signreq.Summary) string {
	return fmt.Sprintf("%s accepted, %d refused", acceptedOf(s), len(s.Refused))
}

func fully(b bool) string {
	if b {
		return " (fully broadcast)"
	}
	return ""
}

func readTransactions(g *globals, path string) ([]signreq.TxSpec, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(g.app.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, exitUsagef("%w", err)
	}
	var txs []signreq.TxSpec
	if err := json.Unmarshal(raw, &txs); err != nil {
		return nil, exitUsagef("transactions: %w", err)
	}
	return txs, nil
}

// signError maps a rejected proposal to usage (1), a transport failure to
// operational, as the other write verbs do.
func signError(err error) error {
	for _, target := range []error{
		signreq.ErrInvalid, signreq.ErrCurveMismatch, chainfill.ErrBitcoin, session.ErrUnknownSession, session.ErrSignerSetFull, session.ErrAlreadyRefused, session.ErrAlreadyAccepted, session.ErrUnsupported, signreq.ErrUnknown, signreq.ErrAmbiguous, signreq.ErrNotSigned,
		key.ErrUnknown, key.ErrAmbiguous, network.ErrUnknown, network.ErrInvalid, network.ErrNoChain,
	} {
		if errors.Is(err, target) {
			return exitUsagef("%w", err)
		}
	}
	return sendError(err)
}
