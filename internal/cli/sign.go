package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
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
		Long: `--transactions is a JSON file (or - for stdin) listing the transactions, each naming a Network by alias
and exactly one intent in protobuf-JSON form:

  [{"network": "ethereum-mainnet",
    "ethereum": {"to": "<base64 20 bytes>", "value": "1000", "nonce": "0",
                 "gasLimit": "21000", "maxFeePerGas": "30000000000", "maxPriorityFeePerGas": "1"}}]

Every referenced Network must be of the Key's curve.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if keyQuery == "" || txFile == "" {
				return exitUsagef("--key and --transactions are required")
			}
			txs, err := readTransactions(g, txFile)
			if err != nil {
				return err
			}
			spec := signreq.Spec{Reason: reason, DeadlineSeconds: deadline, Transactions: txs}
			v, err := serve.Client{Socket: g.paths().Socket}.SignPropose(cmd.Context(), keyQuery, spec)
			if err != nil {
				return signError(err)
			}
			if g.json {
				return g.emitJSON(v)
			}
			fmt.Fprintf(g.app.Stdout, "proposed as seq %d (msg_id %s)\n", v.Seq, v.MsgID)
			return nil
		},
	}
	propose.Flags().StringVar(&keyQuery, "key", "", "curve:hex, a unique public-key prefix, or the Conversation ID")
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
				fmt.Fprintf(g.app.Stdout, "%s  %s  %s accepted  txs=%d  broadcasts=%d%s  %s\n", p.primary(s.MsgID[:8]), p.status(s.Status), acceptedOf(s), s.Transactions, s.Broadcasts, p.dim(fully(s.FullyBroadcast)), s.Reason)
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
			fmt.Fprintf(out, "sign_request %s\nstatus %s\nproposer %s\ncreated_at %s\ndeadline %ds\nconversation %s\nreason %s\n",
				p.primary(d.MsgID), p.status(d.Status), d.Proposer, p.dim(d.CreatedAt), d.DeadlineSeconds, d.Conversation, d.Reason)
			fmt.Fprintf(out, "accepted %s\n", acceptedOf(d.Summary))
			for _, a := range d.Accepted {
				fmt.Fprintf(out, "  %s\n", a)
			}
			if d.Session != "" {
				fmt.Fprintf(out, "session %s\n", d.Session)
			}
			for _, b := range d.BroadcastReceipts {
				fmt.Fprintf(out, "broadcast by %s at %s %s\n", b.Broadcaster, p.dim(b.At), p.dim(fmt.Sprintf("(seq %d)", b.Seq)))
				for _, t := range b.Transactions {
					fmt.Fprintf(out, "  tx %d  %s  %s\n", t.Index, t.Network, t.TxHash)
				}
			}
			for i, t := range d.Details {
				fmt.Fprintf(out, "\n%s %s\n", p.primary(fmt.Sprintf("transaction %d: %s", i, t.Network)), p.dim("("+t.Type+")"))
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
signature verifies against the Key. A Node that accepts after that is not a signer.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := (serve.Client{Socket: g.paths().Socket}).SignAccept(cmd.Context(), args[0]); err != nil {
				return signError(err)
			}
			fmt.Fprintf(g.app.Stdout, "accepted %s\n", args[0])
			return nil
		},
	}

	decline := &cobra.Command{
		Use:   "decline <msg-id>",
		Short: "Abort the signing Session for a Sign Request (requires serve)",
		Long:  `Declining aborts the request's signing Session. Before one has started there is nothing to abort: not accepting is declining, and an accept you already made stays on the log (the output says so).`,
		Args:  cobra.ExactArgs(1),
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
		Long: `Assembles each transaction of a signed Sign Request, submits it through its Network's rpc url (this Node's
local override first, see network), and commits a sign.broadcast receipt for each Network. Networks are
independent: if one fails, the others are still broadcast and receipted, the failure is reported, and the
command exits non-zero. Any Node may broadcast; the proposer goes first by convention. Broadcasting again is
harmless: a node that already has a transaction is not an error, the repeat shows as one more receipt, and a
failed Network is retried. Only http(s) rpc urls are used.`,
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
				for _, n := range v.Networks {
					for _, t := range n.Transactions {
						repeat := ""
						if t.AlreadyKnown {
							repeat = " (already known to the network)"
						}
						fmt.Fprintf(g.app.Stdout, "%s  tx %d  %s%s\n", n.Network, t.Index, t.TxHash, repeat)
					}
					if n.Receipt != nil {
						fmt.Fprintf(g.app.Stdout, "%s: receipt recorded as seq %d (msg_id %s)\n", n.Network, n.Receipt.Seq, n.Receipt.MsgID)
					} else {
						fmt.Fprintf(g.app.Stdout, "%s: %s %s\n", n.Network, g.out().concern("FAILED, no receipt recorded:"), n.Error)
					}
				}
			}
			if v.Failed() {
				return exitOperationalf("some Networks failed to broadcast; run sign broadcast again to retry them")
			}
			return nil
		},
	}

	root.AddCommand(propose, list, inspect, accept, decline, broadcast)
	return root
}

// acceptedOf is "n/t", or "n/? (Key not on this log)" when t is unknown.
func acceptedOf(s signreq.Summary) string {
	if !s.KeyKnown {
		return fmt.Sprintf("%d/? (Key not on this log)", len(s.Accepted))
	}
	return fmt.Sprintf("%d/%d", len(s.Accepted), s.Needed)
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
		signreq.ErrInvalid, signreq.ErrCurveMismatch, session.ErrUnknownSession, session.ErrSignerSetFull, session.ErrUnsupported, signreq.ErrUnknown, signreq.ErrAmbiguous, signreq.ErrNotSigned,
		key.ErrUnknown, key.ErrAmbiguous, network.ErrUnknown, network.ErrInvalid,
	} {
		if errors.Is(err, target) {
			return exitUsagef("%w", err)
		}
	}
	return sendError(err)
}
