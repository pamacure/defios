package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/node"
	"defios.com/internal/serve"
)

func groupCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "group",
		Short: "Scaffold, select, form, and inspect this Node's Groups",
	}
	var newOpts groupNewOptions
	newCmd := &cobra.Command{
		Use:   "new <alias> --headscale <url>",
		Short: "Scaffold a Group under a local alias and make it the Current Group",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runGroupNew(g, args[0], newOpts) },
	}
	newCmd.Flags().StringVar(&newOpts.headscaleURL, "headscale", "", "headscale control URL for this Group's tailnet (required)")
	newCmd.Flags().StringVar(&newOpts.hostname, "hostname", "", "tsnet hostname (default: defios-<first 8 hex of Node ID>-<alias>)")
	newCmd.Flags().StringVar(&newOpts.name, "name", "", "display name for this Group (default: the init --name default)")
	root.AddCommand(newCmd)
	root.AddCommand(&cobra.Command{
		Use:   "use <alias>",
		Short: "Set the Current Group that serve and read verbs default to",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runGroupUse(g, args[0]) },
	})
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
		Short: "List this Node's Groups: alias, status, Group ID, and which is current",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runGroupList(cmd, g) },
	})
	inspect := &cobra.Command{
		Use:   "inspect [group-id]",
		Short: "Print the Group's status and Roster",
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runGroupInspect(cmd, g, args) },
	}
	bindGroupFlag(g, inspect)
	root.AddCommand(inspect)
	return root
}

type groupNewOptions struct {
	headscaleURL, hostname, name string
}

// runGroupNew scaffolds a Group under a local alias on internal/node, then
// focuses it as the Current Group (ADR 0007). It needs the device but no
// serve. A Node may hold any number of Groups at once (#27); only the alias
// must be unique.
func runGroupNew(g *globals, alias string, o groupNewOptions) error {
	if err := group.ValidateAlias(alias); err != nil {
		return exitUsagef("%v", err)
	}
	if o.headscaleURL == "" {
		return exitUsagef("--headscale is required")
	}
	h, err := openNode(g)
	if err != nil {
		return err
	}
	c, err := h.NewGroup(node.GroupSpec{Alias: alias, HeadscaleURL: o.headscaleURL, Hostname: o.hostname, DisplayName: o.name})
	var invalid *node.InvalidError
	switch {
	case errors.Is(err, node.ErrGroupExists):
		return exitOperationalf("Group alias %q already exists on this Node", alias)
	case errors.As(err, &invalid):
		return exitUsagef("%v", err)
	case err != nil:
		return exitOperational(err)
	}
	if err := writeCurrentGroup(g.paths(), alias); err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(map[string]any{
			"alias":         alias,
			"display_name":  c.DisplayName,
			"hostname":      c.Hostname,
			"headscale_url": c.HeadscaleURL,
			"current":       true,
		})
	}
	fmt.Fprintf(g.app.Stdout, "Group %q scaffolded and now current\n  display name: %s\n  hostname:     %s\n", alias, c.DisplayName, c.Hostname)
	return nil
}

// openNode opens this Node for a verb that drives it through internal/node
// without hosting it.
func openNode(g *globals) (*node.Host, error) {
	h, err := node.Open(g.paths(), node.Options{Getenv: g.app.Getenv})
	if err != nil {
		return nil, nodeError(g.paths(), err)
	}
	return h, nil
}

// runGroupUse sets the Current Group pointer to an already-scaffolded Group.
func runGroupUse(g *globals, alias string) error {
	if err := group.ValidateAlias(alias); err != nil {
		return exitUsagef("%v", err)
	}
	p := g.paths()
	aliases, err := configGroupAliases(p)
	if err != nil {
		return exitOperational(err)
	}
	if !contains(aliases, alias) {
		if len(aliases) == 0 {
			return exitOperationalf("no Group %q; run `defios group new %s --headscale <url>` first", alias, alias)
		}
		return exitOperationalf("no Group %q; have: %s", alias, strings.Join(aliases, ", "))
	}
	if err := writeCurrentGroup(p, alias); err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(map[string]any{"alias": alias, "current": true})
	}
	fmt.Fprintf(g.app.Stdout, "Current Group is now %q\n", alias)
	return nil
}

// loadGroup reads the Current Group's Record from disk; no serve needed.
func loadGroup(g *globals) (string, *group.Record, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return "", nil, err
	}
	rec, err := loadAliasRecord(g, alias)
	return alias, rec, err
}

// loadAliasRecord reads one Group's Record by its alias. A Record whose
// signatures no longer check is a verify failure (4), not a missing file (2).
func loadAliasRecord(g *globals, alias string) (*group.Record, error) {
	p := g.paths()
	rec, err := group.LoadAlias(p.GroupsDir(), alias)
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

// groupError maps a formation failure to an exit code: another Node's
// refusal is a protocol reject (3); everything else is operational.
func groupError(err error) error {
	if errors.Is(err, serve.ErrRejected) {
		return &ExitError{Code: ExitProtocol, Err: err}
	}
	return serveError(err)
}

// printView reports a formation verb's outcome. Nodes a message did not
// reach make the verb fail (2) even though the local state changed, so the
// operator knows to re-run it.
func printView(g *globals, f node.Formation) error {
	if g.json {
		if err := g.emitJSON(serve.GroupView{GroupID: f.GroupID, Status: f.Status, Undelivered: f.Undelivered}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(g.app.Stdout, "Group %s is %s\n", f.GroupID, f.Status)
	}
	if len(f.Undelivered) > 0 {
		return exitOperationalf("not delivered to %s; re-run once they are reachable", strings.Join(f.Undelivered, ", "))
	}
	return nil
}

// The formation verbs act on the Group serve serves.

func runGroupCreate(cmd *cobra.Command, g *globals, args []string) error {
	h, err := openNode(g)
	if err != nil {
		return err
	}
	others := 0
	seenSelf := false
	for _, a := range args {
		id, err := identity.ParseID(strings.ToLower(a))
		if err != nil {
			return exitUsagef("%v", err)
		}
		if id.String() == h.NodeID() {
			seenSelf = true
		} else {
			others++
		}
	}
	if !seenSelf {
		return exitUsagef("this Node (%s) must be on the roster", h.NodeID())
	}
	if others != 1 {
		return exitUsagef("MVP forms Groups of exactly one other Node; got %d", others)
	}
	f, err := h.Propose(cmd.Context(), node.ServedGroup, args)
	if err != nil {
		return groupError(err)
	}
	return printView(g, f)
}

func runGroupAccept(cmd *cobra.Command, g *globals, arg string) error {
	gid, err := parseGroupID(arg)
	if err != nil {
		return err
	}
	h, err := openNode(g)
	if err != nil {
		return err
	}
	f, err := h.Accept(cmd.Context(), node.ServedGroup, gid.String())
	if err != nil {
		return groupError(err)
	}
	return printView(g, f)
}

func runGroupDecline(cmd *cobra.Command, g *globals, arg, reason string) error {
	gid, err := parseGroupID(arg)
	if err != nil {
		return err
	}
	h, err := openNode(g)
	if err != nil {
		return err
	}
	f, err := h.Decline(cmd.Context(), node.ServedGroup, gid.String(), reason)
	if err != nil {
		return groupError(err)
	}
	return printView(g, f)
}

// groupLine is one `group list` row: one of this Node's Groups by its local
// alias, its status, its Group ID once formed, and whether it is current.
type groupLine struct {
	Alias   string `json:"alias"`
	Status  string `json:"status"` // unformed | pending | valid | error
	GroupID string `json:"group_id,omitempty"`
	Current bool   `json:"current"`
}

// runGroupList prints one row per Group scaffolded on this Node (#27). An
// unformed Group (scaffolded, no Record yet) shows "unformed" and no Group
// ID. Works without serve: the status comes from disk.
func runGroupList(cmd *cobra.Command, g *globals) error {
	p := g.paths()
	var groups []node.GroupSummary
	h, err := node.Open(p, node.Options{Getenv: g.app.Getenv})
	switch {
	case errors.Is(err, node.ErrNoNode): // no Node, so no Group
	case err != nil:
		return nodeError(p, err)
	default:
		if groups, err = h.Groups(cmd.Context()); err != nil {
			return exitOperational(err)
		}
	}
	current, err := readCurrentGroup(p)
	if err != nil {
		return exitOperational(err)
	}
	lines := []groupLine{}
	for _, gr := range groups {
		lines = append(lines, groupLine{Alias: gr.Alias, Status: gr.Status, GroupID: gr.GroupID, Current: gr.Alias == current})
	}
	if g.json {
		return g.emitJSON(lines)
	}
	pal := g.out()
	for _, l := range lines {
		marker := " "
		if l.Current {
			marker = "*"
		}
		gid := l.GroupID
		if gid == "" {
			gid = "-"
		}
		fmt.Fprintf(g.app.Stdout, "%s %s  %s  %s\n", marker, pal.primary(l.Alias), pal.status(l.Status), pal.dim(gid))
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
	// With no argument inspect the Current Group (or the --group alias);
	// with a Group ID inspect whichever of this Node's Groups has it,
	// current or not (#27). The Group ID arg and --group alias are two ways
	// to name a non-current Group, so refuse both at once.
	if len(args) == 1 && g.group != "" {
		return exitUsagef("give a Group ID or --group <alias>, not both")
	}
	var alias, gid string
	if len(args) == 1 {
		id, err := parseGroupID(args[0])
		if err != nil {
			return err
		}
		gid = id.String()
	} else {
		var err error
		if alias, err = g.resolveAlias(); err != nil {
			return err
		}
	}
	h, err := openNode(g)
	if err != nil {
		return err
	}
	if gid != "" {
		if alias, err = h.GroupByID(gid); err != nil {
			return recordError(err)
		}
	}
	d, err := h.InspectGroup(cmd.Context(), alias)
	if err != nil {
		return recordError(err)
	}
	v := groupView{GroupID: d.GroupID, Status: d.Status}
	for _, n := range d.Roster {
		v.Roster = append(v.Roster, rosterRow{
			NodeID: n.NodeID, DisplayName: n.DisplayName, Hostname: n.Hostname, Reachable: n.Reachable,
			Joined: n.Joined, AuthPublicKey: n.AuthPublicKey, EncryptionPublicKey: n.EncryptionPublicKey,
		})
	}
	if g.json {
		return g.emitJSON(v)
	}
	w, pal := g.app.Stdout, g.out()
	fmt.Fprintf(w, "Group ID: %s\n", pal.primary(v.GroupID))
	fmt.Fprintf(w, "Status:   %s\n", pal.status(v.Status.String()))
	fmt.Fprintln(w, "Roster:")
	for _, r := range v.Roster {
		fmt.Fprintf(w, "  %s  %s  %s  reachable=%s  joined=%s\n", pal.primary(r.NodeID), r.DisplayName, r.Hostname, pal.yesNo(r.Reachable), pal.yesNo(r.Joined))
		fmt.Fprintf(w, "    auth public key:       %s\n", r.AuthPublicKey)
		fmt.Fprintf(w, "    encryption public key: %s\n", r.EncryptionPublicKey)
	}
	return nil
}

// recordError maps a Group Record failure: one whose signatures no longer
// check is a verify failure (4); anything else is operational.
func recordError(err error) error {
	if errors.Is(err, group.ErrBadProposal) || errors.Is(err, group.ErrBadAccept) {
		return exitVerify(err)
	}
	return exitOperational(err)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
