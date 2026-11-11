package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/node"
	"defios.com/internal/plugin"
)

func pluginCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "plugin",
		Short: "Install, list, update and remove the Plugins this Node's agents may be granted",
		Long: `A Plugin is a package of Skills and MCP Servers, installed Node-wide from a git URL or a local path
under <data root>/plugins/<plugin>/ and pinned in <config root>/plugins.lock. DeFiOS's built-in Plugins
come with the binary and need no install. Nothing in a Plugin reaches an agent until its profile grants
it. A running serve picks a change up on the next summon.`,
	}
	root.AddCommand(pluginInstallCommand(g))
	root.AddCommand(pluginListCommand(g))
	root.AddCommand(pluginUpdateCommand(g))
	root.AddCommand(pluginRemoveCommand(g))
	return root
}

func pluginInstallCommand(g *globals) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "install <source>",
		Short: "Install a Plugin from a git URL (url@ref for a branch, tag or commit) or a local path",
		Long: `Install a Plugin from a git URL, cloned and pinned to its commit (url@ref picks a branch, tag or
commit), or from a local path, copied and pinned to a hash of its contents. It shows the Plugin's Skills
and the command line of each MCP Server it would run, and asks first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := openNode(g)
			if err != nil {
				return err
			}
			s, err := h.StagePluginInstall(cmd.Context(), args[0])
			if err != nil {
				return pluginError(err)
			}
			defer s.Close()
			w := g.textOut()
			describePlugin(w, s.Plugin)
			if err := confirm(g, yes, w, fmt.Sprintf("Install %s?", s.Plugin.Name), "plugin "+s.Plugin.Name+" not installed"); err != nil {
				return err
			}
			if err := s.Commit(); err != nil {
				return exitOperational(err)
			}
			if g.json {
				return g.emitJSON(lockedJSON(s.Plugin.Name, s.Entry))
			}
			fmt.Fprintf(w, "Installed %s %s at %s\n", s.Plugin.Name, s.Plugin.Version, s.Entry.Pin)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "install without asking")
	return cmd
}

// pluginError maps a failure of the library's Plugin operations: a source
// that is none is a usage error, the rest operational.
func pluginError(err error) error {
	var invalid *node.InvalidError
	if errors.As(err, &invalid) {
		return exitUsagef("%w", invalid.Err)
	}
	return exitOperational(err)
}

// lockedJSON is an installed Plugin's lock entry as install and update print
// it under --json.
func lockedJSON(name string, e node.PluginPin) map[string]any {
	return map[string]any{"name": name, "version": e.Version, "source": e.Source, "pin": e.Pin}
}

// textOut is where a verb's prose goes: stdout, or stderr under --json so it
// never mixes with the JSON.
func (g *globals) textOut() io.Writer {
	if g.json {
		return g.app.Stderr
	}
	return g.app.Stdout
}

// describePlugin prints what a Plugin brings: its Skills, and the command
// line each MCP Server would run with the variables it asks for.
func describePlugin(w io.Writer, pl node.Plugin) {
	fmt.Fprintf(w, "Plugin %s %s: %s\n", pl.Name, pl.Version, pl.Description)
	if len(pl.Skills) == 0 {
		fmt.Fprintln(w, "  Skills: none")
	} else {
		fmt.Fprintln(w, "  Skills:")
		for _, s := range pl.Skills {
			fmt.Fprintf(w, "    %s: %s\n", s.Name, s.Description)
		}
	}
	if len(pl.Servers) == 0 {
		fmt.Fprintln(w, "  MCP Servers: none")
		return
	}
	fmt.Fprintln(w, "  MCP Servers, each a program it would run on this machine:")
	for _, s := range pl.Servers {
		fmt.Fprintf(w, "    %s: %s\n", s.Name, s.CommandLine)
		if len(s.Env) > 0 {
			fmt.Fprintf(w, "      asks for: %s\n", strings.Join(s.Env, ", "))
		}
	}
}

// confirm asks question on w and reads the answer from stdin; only y or yes
// goes on. yes skips asking. No answer at all is a usage error pointing at
// --yes; any other answer is declined.
func confirm(g *globals, yes bool, w io.Writer, question, declined string) error {
	if yes {
		return nil
	}
	fmt.Fprintf(w, "%s [y/N] ", question)
	line, err := bufio.NewReader(g.app.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(w)
		return exitUsagef("no answer on stdin; pass --yes to go on without asking")
	}
	if ans := strings.ToLower(strings.TrimSpace(line)); ans == "y" || ans == "yes" {
		return nil
	}
	return exitOperationalf("%s", declined)
}

// pluginRow is a Plugin as plugin list shows it.
type pluginRow struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Source  string   `json:"source"`
	Pin     string   `json:"pin,omitempty"`
	BuiltIn bool     `json:"built_in"`
	Missing bool     `json:"missing"`
	Skills  []string `json:"skills"`
	Servers []string `json:"servers"`
}

func pluginListCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the built-in and installed Plugins, with their Skills and MCP Servers",
		Long: `List the built-in and installed Plugins, with their Skills and MCP Servers. A Plugin in plugins.lock
with nothing installed, as a restore leaves one it could not fetch, is listed as missing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := openNode(g)
			if err != nil {
				return err
			}
			listed, err := h.Plugins(cmd.Context())
			if err != nil {
				return exitOperational(err)
			}
			rows := []pluginRow{}
			for _, l := range listed {
				row := pluginRow{
					Name: l.Name, Version: l.Version, Source: l.Source, Pin: l.Pin,
					BuiltIn: l.BuiltIn, Missing: l.Missing, Skills: []string{},
				}
				if row.BuiltIn {
					row.Source = "built-in"
				}
				for _, s := range l.Skills {
					row.Skills = append(row.Skills, s.Name)
				}
				for _, s := range l.Servers {
					row.Servers = append(row.Servers, s.Name)
				}
				rows = append(rows, row)
			}
			if g.json {
				return g.emitJSON(rows)
			}
			pal := g.out()
			for _, r := range rows {
				fmt.Fprintf(g.app.Stdout, "%s %s  %s\n", pal.primary(r.Name), r.Version, r.Source)
				if r.Missing {
					fmt.Fprintf(g.app.Stdout, "  %s: in plugins.lock, not installed; `defios plugin update %s` fetches it\n", pal.concern("missing"), r.Name)
					continue
				}
				fmt.Fprintf(g.app.Stdout, "  skills:      %s\n", listOrNone(r.Skills))
				fmt.Fprintf(g.app.Stdout, "  mcp servers: %s\n", listOrNone(r.Servers))
			}
			return nil
		},
	}
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func pluginUpdateCommand(g *globals) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "update [<plugin>]",
		Short: "Update an installed Plugin, or every one, from its source",
		Long: `Update an installed Plugin, or every one, from the source it was installed from: a git Plugin at the
ref it was installed at, a local-path Plugin copied again. It shows the Plugin, and for git the change to
its plugin.toml, and asks first. A Plugin in plugins.lock that is not installed is fetched again.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := openNode(g)
			if err != nil {
				return err
			}
			names := args
			if len(names) == 0 {
				if names, err = h.InstalledPlugins(cmd.Context()); err != nil {
					return exitOperational(err)
				}
			}
			rows := []map[string]any{}
			var failed error
			for _, name := range names {
				e, err := updatePlugin(g, cmd, h, name, yes)
				if err != nil {
					if len(names) == 1 {
						return err
					}
					fmt.Fprintf(g.app.Stderr, "%s %s: %v\n", paletteFor(g.app.Stderr, g.app.Getenv).concern("defios:"), name, err)
					failed = &ExitError{Code: ExitOperational}
					continue
				}
				rows = append(rows, lockedJSON(name, e))
			}
			if g.json {
				if err := g.emitJSON(rows); err != nil {
					return err
				}
			}
			return failed
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "update without asking")
	return cmd
}

// updatePlugin updates one Plugin, once confirmed, and returns its lock
// entry.
func updatePlugin(g *globals, cmd *cobra.Command, h *node.Host, name string, yes bool) (node.PluginPin, error) {
	s, err := h.StagePluginUpdate(cmd.Context(), name)
	if err != nil {
		return node.PluginPin{}, pluginError(err)
	}
	defer s.Close()
	w := g.textOut()
	if s.UpToDate {
		fmt.Fprintf(w, "%s is up to date at %s\n", name, s.Entry.Pin)
		return s.Entry, nil
	}
	describePlugin(w, s.Plugin)
	switch {
	case s.DiffWhole:
		fmt.Fprintf(w, "%s is no longer in %s; its new %s:\n%s", s.Was.Pin, s.Was.Source, plugin.ManifestFile, s.Diff)
	case s.Diff != "":
		fmt.Fprint(w, s.Diff)
	}
	if err := confirm(g, yes, w, fmt.Sprintf("Update %s from %s to %s?", name, s.Was.Version, s.Entry.Version), "plugin "+name+" not updated"); err != nil {
		return node.PluginPin{}, err
	}
	if err := s.Commit(); err != nil {
		return node.PluginPin{}, exitOperational(err)
	}
	fmt.Fprintf(w, "Updated %s to %s at %s\n", name, s.Entry.Version, s.Entry.Pin)
	return s.Entry, nil
}

func pluginRemoveCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <plugin>",
		Short: "Remove an installed Plugin and its lock entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := openNode(g)
			if err != nil {
				return err
			}
			var granted *node.PluginGrantedError
			if err := h.RemovePlugin(cmd.Context(), args[0]); errors.As(err, &granted) {
				return exitUsagef("%v; revoke it there first with `defios agent revoke`", granted)
			} else if err != nil {
				return exitOperational(err)
			}
			if g.json {
				return g.emitJSON(map[string]any{"name": args[0], "removed": true})
			}
			fmt.Fprintf(g.app.Stdout, "Removed %s\n", args[0])
			return nil
		},
	}
}

// reportPlugins prints what a restore did with each Plugin in the lock.
func reportPlugins(w io.Writer, plugins []plugin.Reinstalled) {
	for _, r := range plugins {
		switch r.Status {
		case plugin.ReinstalledAtPin:
			fmt.Fprintf(w, "Plugin %s reinstalled at its pin.\n", r.Name)
		case plugin.ReinstallMissing:
			fmt.Fprintf(w, "Plugin %s is missing: it was installed from a local path, which a restore cannot fetch; `defios plugin update %s` copies it again once the path is there.\n", r.Name, r.Name)
		default:
			fmt.Fprintf(w, "Plugin %s failed to reinstall: %v; `defios plugin update %s` tries again.\n", r.Name, r.Err, r.Name)
		}
	}
}

// reinstalledJSON is a restore's Plugins as init --from-backup --json
// prints them.
func reinstalledJSON(plugins []plugin.Reinstalled) []map[string]any {
	rows := []map[string]any{}
	for _, r := range plugins {
		row := map[string]any{"name": r.Name, "status": r.Status}
		if r.Err != nil {
			row["error"] = r.Err.Error()
		}
		rows = append(rows, row)
	}
	return rows
}
