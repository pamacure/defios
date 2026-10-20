package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	"defios.com/internal/raftx"
	"defios.com/internal/serve"
	"defios.com/internal/session"
)

func sessionCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "session",
		Short: "Start, accept, abort, and inspect keygen Sessions",
	}
	var (
		curve string
		t     int
	)
	start := &cobra.Command{
		Use:   "start",
		Short: "Start a keygen Session (requires serve); this Node accepts it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSessionStart(cmd, g, curve, t)
		},
	}
	start.Flags().StringVar(&curve, "curve", "", "ecdsa or eddsa")
	start.Flags().IntVar(&t, "t", 2, "min signers; n is the Roster")
	root.AddCommand(start)

	root.AddCommand(&cobra.Command{
		Use:   "accept <session-id>",
		Short: "Accept a Session locally so this Node's engine may start (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionAccept(cmd, g, args[0])
		},
	})

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

	root.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List Sessions on this replica: id, status, curve, t, n",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runSessionList(g) },
	})
	root.AddCommand(&cobra.Command{
		Use:   "inspect <session-id>",
		Short: "Show one Session's participants, status, and completes",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runSessionInspect(g, args[0]) },
	})
	return root
}

func runSessionStart(cmd *cobra.Command, g *globals, curve string, t int) error {
	if curve != "ecdsa" && curve != "eddsa" {
		return exitUsagef("--curve must be ecdsa or eddsa")
	}
	if t < 1 {
		return exitUsagef("--t must be >= 1")
	}
	client := serve.Client{Socket: g.paths().Socket}
	sum, err := client.SessionStart(cmd.Context(), curve, t)
	if err != nil {
		return sessionCallError(err)
	}
	if g.json {
		return g.emitJSON(sum)
	}
	fmt.Fprintf(g.app.Stdout, "started %s  %s  t=%d n=%d\n", sum.SessionID, sum.Curve, sum.T, sum.N)
	return nil
}

func runSessionAccept(cmd *cobra.Command, g *globals, hexID string) error {
	id, err := parseSessionID(hexID)
	if err != nil {
		return err
	}
	client := serve.Client{Socket: g.paths().Socket}
	if err := client.SessionAccept(cmd.Context(), id); err != nil {
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
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		return exitOperational(err)
	}
	list, err := session.List(rows)
	if err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(list)
	}
	for _, s := range list {
		fmt.Fprintf(g.app.Stdout, "%s  %s  %s  t=%d n=%d\n", s.SessionID, s.Status, s.Curve, s.T, s.N)
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
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		return exitOperational(err)
	}
	detail, err := session.Inspect(rows, id)
	if errors.Is(err, session.ErrUnknownSession) {
		return exitOperational(err)
	}
	if err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(detail)
	}
	fmt.Fprintf(g.app.Stdout, "session %s\nstatus %s\ncurve %s\nt %d\nn %d\n", detail.SessionID, detail.Status, detail.Curve, detail.T, detail.N)
	if detail.Reason != "" {
		fmt.Fprintf(g.app.Stdout, "reason %s\n", detail.Reason)
	}
	fmt.Fprintf(g.app.Stdout, "participants\n")
	for _, p := range detail.Participants {
		fmt.Fprintf(g.app.Stdout, "  %s\n", p)
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
	case errors.Is(err, session.ErrBadCurve):
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

// sessionEngine reads DEFIOS_SESSION_ENGINE. Empty is the stand-in that
// refuses Start; "exchange" is the trivial round engine the local-accept
// test drives.
func sessionEngine(getenv func(string) string, self identity.ID) (func() session.Engine, error) {
	switch getenv("DEFIOS_SESSION_ENGINE") {
	case "", "unavailable":
		return func() session.Engine { return session.Unavailable{} }, nil
	case "exchange":
		return func() session.Engine { return session.NewExchangeEngine([32]byte(self)) }, nil
	default:
		return nil, fmt.Errorf("DEFIOS_SESSION_ENGINE %q is unknown", getenv("DEFIOS_SESSION_ENGINE"))
	}
}
