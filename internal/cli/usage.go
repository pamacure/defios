package cli

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"defios.com/internal/cosl"
	"defios.com/internal/node"
	"defios.com/internal/serve"
)

// usageView is what `node usage` shows: the Node's disk, read straight
// from its trees so it needs no serve, and serve's own process when one is
// running. Sizes are apparent (byte length), in bytes.
type usageView struct {
	Binary usagePath    `json:"binary"`
	Data   dataUsage    `json:"data"`
	Config configUsage  `json:"config"`
	Groups []groupUsage `json:"groups"`
	Total  int64        `json:"total_bytes"` // binary + data + config
	Serve  *serve.Usage `json:"serve"`       // nil when serve is not running
}

type usagePath struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// dataUsage is the data tree: the Node-wide identity and Vault shared file
// plus the data side of every Group shown.
type dataUsage struct {
	Path        string `json:"path"`
	Identity    int64  `json:"identity_bytes"`
	VaultShared int64  `json:"vault_shared_bytes"`
	Bytes       int64  `json:"bytes"`
}

// configUsage is the config tree: its Node-wide root files plus the
// Workspace of every Group shown.
type configUsage struct {
	Path  string `json:"path"`
	Root  int64  `json:"root_bytes"`
	Bytes int64  `json:"bytes"`
}

// groupUsage is one Group's share of both trees.
type groupUsage struct {
	Alias     string    `json:"alias"`
	Current   bool      `json:"current"`
	COSL      coslUsage `json:"cosl"`
	Raft      int64     `json:"raft_bytes"`
	Tsnet     int64     `json:"tsnet_bytes"`
	Vault     int64     `json:"vault_bytes"` // its shares and Secrets files
	Workspace int64     `json:"workspace_bytes"`
	Other     int64     `json:"other_bytes"` // the rest of group/<alias>/: roster, pending files
	Bytes     int64     `json:"bytes"`
}

type coslUsage struct {
	Bytes int64 `json:"bytes"`
	cosl.Counts
}

func runNodeUsage(cmd *cobra.Command, g *globals) error {
	h, err := openNode(g)
	if err != nil {
		return err
	}
	alias := ""
	if g.group != "" {
		if alias, err = g.resolveAlias(); err != nil {
			return err
		}
	}
	u, err := h.Usage(cmd.Context(), alias)
	if err != nil {
		return nodeError(g.paths(), err)
	}
	current, err := readCurrentGroup(g.paths())
	if err != nil {
		return exitOperational(err)
	}
	v := usageViewOf(u, current)
	if g.json {
		return g.emitJSON(v)
	}
	printUsage(g, v)
	return nil
}

// usageViewOf is u as node usage shows it, current marking the Current
// Group.
func usageViewOf(u node.Usage, current string) usageView {
	v := usageView{
		Binary: usagePath{Path: u.Binary.Path, Bytes: u.Binary.Bytes},
		Data:   dataUsage{Path: u.Data.Path, Identity: u.Data.Identity, VaultShared: u.Data.VaultShared, Bytes: u.Data.Bytes},
		Config: configUsage{Path: u.Config.Path, Root: u.Config.Root, Bytes: u.Config.Bytes},
		Total:  u.Total,
		Serve:  u.Serve,
	}
	for _, gu := range u.Groups {
		v.Groups = append(v.Groups, groupUsage{
			Alias: gu.Alias, Current: gu.Alias == current,
			COSL: coslUsage{Bytes: gu.COSL.Bytes, Counts: gu.COSL.Counts},
			Raft: gu.Raft, Tsnet: gu.Tsnet, Vault: gu.Vault, Workspace: gu.Workspace, Other: gu.Other, Bytes: gu.Bytes,
		})
	}
	return v
}

func printUsage(g *globals, v usageView) {
	type row struct {
		label, detail string
		n             int64
		heading       bool
	}
	var rows []row
	r := func(label, detail string, n int64) { rows = append(rows, row{label: label, detail: detail, n: n}) }
	r("binary", v.Binary.Path, v.Binary.Bytes)
	r("identity", "", v.Data.Identity)
	r("vault", "shared", v.Data.VaultShared)
	r("config", "root files", v.Config.Root)
	for _, gu := range v.Groups {
		rows = append(rows, row{label: groupHeading(gu), n: gu.Bytes, heading: true})
		r("  COSL", fmt.Sprintf("%d rows, %d pruned", gu.COSL.Rows, gu.COSL.Pruned), gu.COSL.Bytes)
		r("  Raft", "", gu.Raft)
		r("  tsnet", "", gu.Tsnet)
		r("  vault", "shares, Secrets", gu.Vault)
		r("  workspace", "", gu.Workspace)
		r("  other", "", gu.Other)
	}
	r("data", v.Data.Path, v.Data.Bytes)
	r("config", v.Config.Path, v.Config.Bytes)
	r("total", "", v.Total)

	// Aligned by hand, not tabwriter: color codes would count toward its
	// cell widths.
	var lw, dw int
	for _, x := range rows {
		lw = max(lw, utf8.RuneCountInString(x.label))
		dw = max(dw, utf8.RuneCountInString(x.detail))
	}
	pad := func(s string, w int) string { return strings.Repeat(" ", w+2-utf8.RuneCountInString(s)) }
	p := g.out()
	w := g.app.Stdout
	for _, x := range rows {
		label := x.label
		if x.heading {
			label = p.primary(label)
		}
		fmt.Fprintf(w, "%s%s%s%s%10s\n", label, pad(x.label, lw), p.dim(x.detail), pad(x.detail, dw), humanBytes(x.n))
	}

	fmt.Fprintln(w)
	s := v.Serve
	if s == nil {
		fmt.Fprintln(w, p.caution("serve not running"))
		return
	}
	fmt.Fprintf(w, "%s\n", p.primary(fmt.Sprintf("serve (%s)", s.Alias)))
	fmt.Fprintf(w, "  pid         %d\n", s.PID)
	fmt.Fprintf(w, "  uptime      %s\n", s.Uptime.Truncate(time.Second))
	if s.UserCPU != nil && s.SystemCPU != nil {
		fmt.Fprintf(w, "  cpu         %s user, %s system\n", s.UserCPU.Round(time.Millisecond), s.SystemCPU.Round(time.Millisecond))
	}
	switch {
	case s.RSS != nil && s.PeakRSS != nil:
		fmt.Fprintf(w, "  rss         %s %s\n", humanBytes(int64(*s.RSS)), p.dim(fmt.Sprintf("(peak %s)", humanBytes(int64(*s.PeakRSS)))))
	case s.PeakRSS != nil:
		fmt.Fprintf(w, "  rss         peak %s\n", humanBytes(int64(*s.PeakRSS)))
	}
	fmt.Fprintf(w, "  go heap     %s in use, %s from the OS, %d GCs\n", humanBytes(int64(s.HeapInUse)), humanBytes(int64(s.Sys)), s.NumGC)
	fmt.Fprintf(w, "  goroutines  %d\n", s.Goroutines)
}

func groupHeading(u groupUsage) string {
	if u.Current {
		return "group " + u.Alias + " (current)"
	}
	return "group " + u.Alias
}

// humanBytes is n in B, KiB, MiB or GiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f, suffix := float64(n)/unit, "KiB"
	for _, s := range []string{"MiB", "GiB", "TiB"} {
		if f < unit {
			break
		}
		f, suffix = f/unit, s
	}
	return fmt.Sprintf("%.1f %s", f, suffix)
}
