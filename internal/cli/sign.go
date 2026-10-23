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
		Short: "Propose and inspect Sign Requests",
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
			for _, s := range rows {
				fmt.Fprintf(g.app.Stdout, "%s  %s  %d/%d accepted  txs=%d  %s\n", s.MsgID[:8], s.Status, len(s.Accepted), s.Needed, s.Transactions, s.Reason)
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
			out := g.app.Stdout
			fmt.Fprintf(out, "sign_request %s\nstatus %s\nproposer %s\ncreated_at %s\ndeadline %ds\nconversation %s\nreason %s\n",
				d.MsgID, d.Status, d.Proposer, d.CreatedAt, d.DeadlineSeconds, d.Conversation, d.Reason)
			fmt.Fprintf(out, "accepted %d/%d\n", len(d.Accepted), d.Needed)
			for _, a := range d.Accepted {
				fmt.Fprintf(out, "  %s\n", a)
			}
			if d.Session != "" {
				fmt.Fprintf(out, "session %s\n", d.Session)
			}
			if d.ReasonRef != "" {
				fmt.Fprintf(out, "reason_ref %s\n", d.ReasonRef)
			}
			for i, t := range d.Details {
				fmt.Fprintf(out, "\ntransaction %d: %s (%s)\n", i, t.Network, t.Type)
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
		Long:  `Declining aborts the request's signing Session. Before one has started there is nothing to abort: not accepting is declining.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := (serve.Client{Socket: g.paths().Socket}).SignDecline(cmd.Context(), args[0]); err != nil {
				return signError(err)
			}
			fmt.Fprintf(g.app.Stdout, "declined %s\n", args[0])
			return nil
		},
	}

	root.AddCommand(propose, list, inspect, accept, decline)
	return root
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
		signreq.ErrInvalid, signreq.ErrCurveMismatch, session.ErrUnknownSession, session.ErrUnsupported, signreq.ErrUnknown, signreq.ErrAmbiguous,
		key.ErrUnknown, key.ErrAmbiguous, network.ErrUnknown, network.ErrInvalid,
	} {
		if errors.Is(err, target) {
			return exitUsagef("%w", err)
		}
	}
	return sendError(err)
}
