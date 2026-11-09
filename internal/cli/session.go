package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"defios.com/internal/cosl"
	"defios.com/internal/engine/tssl"
	"defios.com/internal/identity"
	"defios.com/internal/keyalias"
	"defios.com/internal/raftx"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

func sessionCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "session",
		Short: "Start, accept, abort, and inspect keygen Sessions",
	}
	var (
		curve string
		t     int
		alias string
	)
	start := &cobra.Command{
		Use:   "start",
		Short: "Start a keygen Session (requires serve); this Node accepts it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSessionStart(cmd, g, curve, t, alias)
		},
	}
	start.Flags().StringVar(&curve, "curve", "", "ecdsa or eddsa")
	start.Flags().IntVar(&t, "t", 2, "min signers; n is the Roster")
	start.Flags().StringVar(&alias, "alias", "", "Key alias to propose to every Node party to the Key")
	root.AddCommand(start)

	var ownAlias string
	accept := &cobra.Command{
		Use:   "accept <session-id>",
		Short: "Accept a Session locally so this Node's engine may start (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionAccept(cmd, g, args[0], ownAlias)
		},
	}
	accept.Flags().StringVar(&ownAlias, "alias", "", "this Node's own Key alias for the Key, instead of the one proposed")
	root.AddCommand(accept)

	var reason string
	abort := &cobra.Command{
		Use:   "abort <session-id>",
		Short: "Append session.abort (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionAbort(cmd, g, args[0], reason)
		},
	}
	abort.Flags().StringVar(&reason, "reason", "", "why the Session is aborted")
	root.AddCommand(abort)

	list := &cobra.Command{
		Use:   "list",
		Short: "List Sessions on this replica: id, status, curve, t, n",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runSessionList(g) },
	}
	bindGroupFlag(g, list)
	root.AddCommand(list)
	inspect := &cobra.Command{
		Use:   "inspect <session-id>",
		Short: "Show one Session's participants, status, and completes",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runSessionInspect(g, args[0]) },
	}
	bindGroupFlag(g, inspect)
	root.AddCommand(inspect)
	return root
}

func runSessionStart(cmd *cobra.Command, g *globals, curve string, t int, alias string) error {
	if curve != "ecdsa" && curve != "eddsa" {
		return exitUsagef("--curve must be ecdsa or eddsa")
	}
	if t < 1 {
		return exitUsagef("--t must be >= 1")
	}
	alias, err := aliasFlag(cmd, alias)
	if err != nil {
		return err
	}
	client := serve.Client{Socket: g.paths().Socket}
	sum, err := client.SessionStart(cmd.Context(), curve, t, alias)
	if err != nil {
		return sessionCallError(err)
	}
	if g.json {
		return g.emitJSON(sum)
	}
	fmt.Fprintf(g.app.Stdout, "started %s  %s  t=%d n=%d%s\n", sum.SessionID, sum.Curve, sum.T, sum.N, proposedSuffix(palette{}, sum)) // a mutating verb's confirmation stays plain
	return nil
}

func runSessionAccept(cmd *cobra.Command, g *globals, hexID, alias string) error {
	id, err := parseSessionID(hexID)
	if err != nil {
		return err
	}
	if alias, err = aliasFlag(cmd, alias); err != nil {
		return err
	}
	client := serve.Client{Socket: g.paths().Socket}
	if err := client.SessionAccept(cmd.Context(), id, alias); err != nil {
		return sessionCallError(err)
	}
	if g.json {
		return g.emitJSON(struct {
			SessionID string `json:"session_id"`
		}{SessionID: id.String()})
	}
	fmt.Fprintf(g.app.Stdout, "accepted %s\n", id)
	return nil
}

func runSessionAbort(cmd *cobra.Command, g *globals, hexID, reason string) error {
	id, err := parseSessionID(hexID)
	if err != nil {
		return err
	}
	client := serve.Client{Socket: g.paths().Socket}
	if err := client.SessionAbort(cmd.Context(), id, reason); err != nil {
		return sessionCallError(err)
	}
	if g.json {
		return g.emitJSON(struct {
			SessionID string `json:"session_id"`
			Reason    string `json:"reason,omitempty"`
		}{SessionID: id.String(), Reason: reason})
	}
	fmt.Fprintf(g.app.Stdout, "aborted %s\n", id)
	return nil
}

func runSessionList(g *globals) error {
	store, err := openReplica(g)
	if err != nil {
		return err
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{MsgTypes: signreq.LookupMsgTypes})
	if err != nil {
		return exitOperational(err)
	}
	list, err := session.List(rows, signInfoOf(rows))
	if err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(list)
	}
	p := g.out()
	for _, s := range list {
		fmt.Fprintf(g.app.Stdout, "%s  %s  %s  t=%d n=%d%s\n", p.primary(s.SessionID), p.status(s.Status), s.Curve, s.T, s.N, proposedSuffix(p, s))
	}
	return nil
}

func runSessionInspect(g *globals, hexID string) error {
	id, err := parseSessionID(hexID)
	if err != nil {
		return err
	}
	store, err := openReplica(g)
	if err != nil {
		return err
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{MsgTypes: signreq.LookupMsgTypes})
	if err != nil {
		return exitOperational(err)
	}
	detail, err := session.Inspect(rows, id, signInfoOf(rows))
	if errors.Is(err, session.ErrUnknownSession) {
		return exitOperational(err)
	}
	if err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(detail)
	}
	p := g.out()
	fmt.Fprintf(g.app.Stdout, "session %s\nstatus %s\ncurve %s\nt %d\nn %d\n", p.primary(detail.SessionID), p.status(detail.Status), detail.Curve, detail.T, detail.N)
	if detail.ProposedAlias != "" {
		fmt.Fprintf(g.app.Stdout, "proposed_alias %s\n", p.primary(detail.ProposedAlias))
	}
	if detail.Reason != "" {
		fmt.Fprintf(g.app.Stdout, "reason %s\n", detail.Reason)
	}
	fmt.Fprintf(g.app.Stdout, "participants\n")
	for _, part := range detail.Participants {
		fmt.Fprintf(g.app.Stdout, "  %s\n", part)
	}
	fmt.Fprintf(g.app.Stdout, "completes\n")
	if len(detail.Completes) == 0 {
		fmt.Fprintf(g.app.Stdout, "  none\n")
		return nil
	}
	for _, c := range detail.Completes {
		fmt.Fprintf(g.app.Stdout, "  %s %s:%s\n", c.Sender, c.Curve, c.PublicKey)
	}
	return nil
}

// aliasFlag is --alias in stored form, or a usage error. Set but empty is
// refused, not taken for no flag at all.
func aliasFlag(cmd *cobra.Command, alias string) (string, error) {
	if !cmd.Flags().Changed("alias") {
		return "", nil
	}
	stored, err := keyalias.Normalize(alias)
	if err != nil {
		return "", exitUsagef("%w", err)
	}
	return stored, nil
}

// proposedSuffix ends a one-line Session with the Key alias it proposes.
func proposedSuffix(p palette, s session.Summary) string {
	if s.ProposedAlias == "" {
		return ""
	}
	return "  proposed_alias=" + p.primary(s.ProposedAlias)
}

func parseSessionID(s string) (session.ID, error) {
	id, err := session.ParseID(strings.ToLower(s))
	if err != nil {
		return session.ID{}, exitUsagef("%w", err)
	}
	return id, nil
}

// sessionCallError maps a serve failure onto an exit code.
func sessionCallError(err error) error {
	switch {
	case errors.Is(err, session.ErrBadCurve), errors.Is(err, serve.ErrBadAlias), errors.Is(err, session.ErrNotKeygen):
		return exitUsagef("%w", err)
	case errors.Is(err, raftx.ErrNotInserted):
		return &ExitError{Code: ExitProtocol, Err: err}
	}
	return serveError(err)
}

// sessionTimeout reads DEFIOS_SESSION_TIMEOUT, the test hook for the
// §7.4 wait. Empty means the default.
func sessionTimeout(getenv func(string) string) (time.Duration, error) {
	v := getenv("DEFIOS_SESSION_TIMEOUT")
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("DEFIOS_SESSION_TIMEOUT %q is not a positive duration", v)
	}
	return d, nil
}

// sessionEngine reads DEFIOS_SESSION_ENGINE. Empty is the tss-lib adapter,
// which runs ecdsa or eddsa from the Session's curve. "unavailable" refuses
// Start. "exchange" is the trivial round engine the local-accept test drives.
func sessionEngine(getenv func(string) string, self identity.ID) (func() session.Engine, error) {
	switch getenv("DEFIOS_SESSION_ENGINE") {
	case "", "tss":
		return func() session.Engine { return tssl.New([32]byte(self)) }, nil
	case "unavailable":
		return func() session.Engine { return session.Unavailable{} }, nil
	case "exchange":
		return func() session.Engine { return session.NewExchangeEngine([32]byte(self)) }, nil
	default:
		return nil, fmt.Errorf("DEFIOS_SESSION_ENGINE %q is unknown", getenv("DEFIOS_SESSION_ENGINE"))
	}
}

// signEngine is the signing engine for the same DEFIOS_SESSION_ENGINE
// setting. Only the tss-lib adapter signs; the test engines do not.
func signEngine(getenv func(string) string, self identity.ID) func() session.Engine {
	switch getenv("DEFIOS_SESSION_ENGINE") {
	case "", "tss":
		return func() session.Engine { return tssl.NewSigner([32]byte(self)) }
	}
	return nil
}

// signInfoOf lets the listings verify a signing Session's completes against
// the Key and Digests its sign.request implies, as serve does. The listings
// read signreq.LookupMsgTypes only: the Session rows they fold are among them.
func signInfoOf(rows []cosl.Row) session.ReadOption {
	return session.WithSignInfo(func(id [32]byte) (session.SignInfo, error) {
		sg, err := signreq.Lookup(rows, id)
		if err != nil {
			return session.SignInfo{}, err
		}
		return session.SignInfo{Curve: sg.Curve, PublicKey: sg.PublicKey, T: sg.T, Digests: sg.Digests, Deadline: sg.Deadline}, nil
	})
}
