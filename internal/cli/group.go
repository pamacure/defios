package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/serve"
)

func groupCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "group",
		Short: "Form and inspect this Node's Group",
	}
	root.AddCommand(&cobra.Command{
		Use:   "create <node-id>...",
		Short: "Propose a Group of these Nodes, this Node included (requires serve)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runGroupCreate(cmd, g, args) },
	})
	root.AddCommand(&cobra.Command{
		Use:   "accept <group-id>",
		Short: "Accept the pending Group proposal (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runGroupAccept(cmd, g, args[0]) },
	})
	var reason string
	decline := &cobra.Command{
		Use:   "decline <group-id>",
		Short: "Decline the pending Group proposal, freeing this Node (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runGroupDecline(cmd, g, args[0], reason) },
	}
	decline.Flags().StringVar(&reason, "reason", "", "why, for the other Nodes' logs")
	root.AddCommand(decline)
	root.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Print this Node's Group ID and status",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runGroupList(g) },
	})
	root.AddCommand(&cobra.Command{
		Use:   "inspect [group-id]",
		Short: "Print the Group's status and Roster",
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runGroupInspect(cmd, g, args) },
	})
	return root
}

// loadGroup reads this Node's Record from disk; no serve needed. A Record
// whose signatures no longer check is a verify failure (4), not a missing
// file (2).
func loadGroup(g *globals) (*group.Record, error) {
	p := g.paths()
	rec, err := group.Load(p.GroupsDir())
	switch {
	case err == nil:
		return rec, nil
	case errors.Is(err, group.ErrNoGroup):
		if _, serr := os.Stat(p.IdentityFile()); errors.Is(serr, os.ErrNotExist) {
			return nil, errNoNode(p)
		}
		return nil, exitOperational(err)
	case errors.Is(err, group.ErrBadProposal), errors.Is(err, group.ErrBadAccept):
		return nil, exitVerify(err)
	}
	return nil, exitOperational(err)
}

func parseGroupID(s string) (group.ID, error) {
	gid, err := group.ParseID(strings.ToLower(s))
	if err != nil {
		return gid, exitUsagef("%v", err)
	}
	return gid, nil
}

// groupError maps a formation failure from serve to an exit code: another
// Node's refusal is a protocol reject (3); everything else is operational.
func groupError(err error) error {
	if errors.Is(err, serve.ErrRejected) {
		return &ExitError{Code: ExitProtocol, Err: err}
	}
	return serveError(err)
}

// printView reports a formation verb's outcome. Nodes a message did not
// reach make the verb fail (2) even though the local state changed, so the
// operator knows to re-run it.
func printView(g *globals, v serve.GroupView) error {
	if g.json {
		if err := g.emitJSON(v); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(g.app.Stdout, "Group %s is %s\n", v.GroupID, v.Status)
	}
	if len(v.Undelivered) > 0 {
		return exitOperationalf("not delivered to %s; re-run once they are reachable", strings.Join(v.Undelivered, ", "))
	}
	return nil
}

func runGroupCreate(cmd *cobra.Command, g *globals, args []string) error {
	p := g.paths()
	self, err := identity.ReadFile(p.IdentityFile())
	if errors.Is(err, os.ErrNotExist) {
		return errNoNode(p)
	} else if err != nil {
		return exitOperational(err)
	}
	ids := make([]identity.ID, 0, len(args))
	others := 0
	seenSelf := false
	for _, a := range args {
		id, err := identity.ParseID(strings.ToLower(a))
		if err != nil {
			return exitUsagef("%v", err)
		}
		if id == self.NodeID {
			seenSelf = true
		} else {
			others++
		}
		ids = append(ids, id)
	}
	if !seenSelf {
		return exitUsagef("this Node (%s) must be on the roster", self.NodeID)
	}
	if others != 1 {
		return exitUsagef("MVP forms Groups of exactly one other Node; got %d", others)
	}
	v, err := serve.Client{Socket: p.Socket}.GroupCreate(cmd.Context(), ids)
	if err != nil {
		return groupError(err)
	}
	return printView(g, v)
}

func runGroupAccept(cmd *cobra.Command, g *globals, arg string) error {
	gid, err := parseGroupID(arg)
	if err != nil {
		return err
	}
	v, err := serve.Client{Socket: g.paths().Socket}.GroupAccept(cmd.Context(), gid)
	if err != nil {
		return groupError(err)
	}
	return printView(g, v)
}

func runGroupDecline(cmd *cobra.Command, g *globals, arg, reason string) error {
	gid, err := parseGroupID(arg)
	if err != nil {
		return err
	}
	v, err := serve.Client{Socket: g.paths().Socket}.GroupDecline(cmd.Context(), gid, reason)
	if err != nil {
		return groupError(err)
	}
	return printView(g, v)
}

// groupLine is one `group list` row.
type groupLine struct {
	GroupID string       `json:"group_id"`
	Status  group.Status `json:"status"`
}

func runGroupList(g *globals) error {
	rec, err := loadGroup(g)
	lines := []groupLine{}
	switch {
	case err == nil:
		lines = append(lines, groupLine{GroupID: rec.GroupID().String(), Status: rec.Status()})
	case errors.Is(err, group.ErrNoGroup):
	default:
		return err
	}
	if g.json {
		return g.emitJSON(lines)
	}
	for _, l := range lines {
		fmt.Fprintf(g.app.Stdout, "%s  %s\n", l.GroupID, l.Status)
	}
	return nil
}

// rosterRow is one `group inspect` Roster row (§6.2).
type rosterRow struct {
	NodeID              string `json:"node_id"`
	DisplayName         string `json:"display_name"`
	Hostname            string `json:"hostname"`
	Reachable           bool   `json:"reachable"`
	Joined              bool   `json:"joined"` // membership.join on the COSL
	AuthPublicKey       string `json:"auth_public_key"`
	EncryptionPublicKey string `json:"encryption_public_key"`
}

type groupView struct {
	GroupID string       `json:"group_id"`
	Status  group.Status `json:"status"`
	Roster  []rosterRow  `json:"roster"`
}

func runGroupInspect(cmd *cobra.Command, g *globals, args []string) error {
	var want *group.ID
	if len(args) == 1 {
		gid, err := parseGroupID(args[0])
		if err != nil {
			return err
		}
		want = &gid
	}
	rec, err := loadGroup(g)
	if err != nil {
		return err
	}
	if want != nil && *want != rec.GroupID() {
		return exitOperationalf("%w: this Node's Group is %s", group.ErrUnknownGroup, rec.GroupID())
	}
	p := g.paths()

	// Reachability and refreshed names come from live Identities when
	// serve is up; the pinned keys never do (§6.2).
	live := map[identity.ID]serve.NodeSummary{}
	serveUp := serve.IsUp(p.Socket)
	if serveUp {
		if nodes, err := (serve.Client{Socket: p.Socket}).Nodes(cmd.Context()); err == nil {
			for _, n := range nodes {
				if id, err := identity.ParseID(n.NodeID); err == nil {
					live[id] = n
				}
			}
		}
	}
	self, _ := identity.ReadFile(p.IdentityFile())
	joined, err := joinedNodes(p.CoslFile(rec.GroupID().String()))
	if err != nil {
		return exitOperational(err)
	}

	v := groupView{GroupID: rec.GroupID().String(), Status: rec.Status()}
	for _, n := range rec.Roster().Nodes() {
		row := rosterRow{
			NodeID:              n.NodeID.String(),
			DisplayName:         n.DisplayName,
			Hostname:            n.TsnetHostname,
			Joined:              joined[n.NodeID],
			AuthPublicKey:       hex.EncodeToString(n.AuthPublicKey),
			EncryptionPublicKey: hex.EncodeToString(n.EncryptionPublicKey),
		}
		if n.NodeID == self.NodeID {
			row.Reachable = serveUp
		} else if l, ok := live[n.NodeID]; ok && l.AuthPublicKey == row.AuthPublicKey && l.EncryptionPublicKey == row.EncryptionPublicKey {
			row.Reachable = true
			row.DisplayName, row.Hostname = l.DisplayName, l.Hostname
		}
		v.Roster = append(v.Roster, row)
	}
	if g.json {
		return g.emitJSON(v)
	}
	w := g.app.Stdout
	fmt.Fprintf(w, "Group ID: %s\n", v.GroupID)
	fmt.Fprintf(w, "Status:   %s\n", v.Status)
	fmt.Fprintln(w, "Roster:")
	for _, r := range v.Roster {
		fmt.Fprintf(w, "  %s  %s  %s  reachable=%s  joined=%s\n", r.NodeID, r.DisplayName, r.Hostname, yesNo(r.Reachable), yesNo(r.Joined))
		fmt.Fprintf(w, "    auth public key:       %s\n", r.AuthPublicKey)
		fmt.Fprintf(w, "    encryption public key: %s\n", r.EncryptionPublicKey)
	}
	return nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// joinedNodes reads which Nodes have a membership.join on the COSL. No
// replica yet means nobody has.
func joinedNodes(coslPath string) (map[identity.ID]bool, error) {
	joined := map[identity.ID]bool{}
	if _, err := os.Stat(coslPath); errors.Is(err, os.ErrNotExist) {
		return joined, nil
	} else if err != nil {
		return nil, err
	}
	store, err := cosl.Open(coslPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	typ := cosl.TypeMembershipJoin
	rows, err := store.Log(cosl.Query{MsgType: &typ})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		joined[r.Sender] = true
	}
	return joined, nil
}
