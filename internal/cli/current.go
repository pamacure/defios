package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"defios.com/internal/nodedir"
)

// contains reports whether aliases holds x.
func contains(aliases []string, x string) bool {
	for _, a := range aliases {
		if a == x {
			return true
		}
	}
	return false
}

// configGroupAliases lists the aliases of Groups scaffolded on this Node,
// i.e. the subtrees under the config tree that hold a config.toml. Sorted.
func configGroupAliases(p nodedir.Paths) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(p.GroupsConfigDir(), "*", "config.toml"))
	if err != nil {
		return nil, err
	}
	aliases := make([]string, 0, len(matches))
	for _, m := range matches {
		aliases = append(aliases, filepath.Base(filepath.Dir(m)))
	}
	sort.Strings(aliases)
	return aliases, nil
}

// groupAliases lists every Group on this Node: the union of the alias
// subdirectories under the config and data trees (`group new` creates both,
// so an unformed Group appears here before it has a Record). Sorted. In
// --dir mode the two trees coincide. Unlike configGroupAliases this does not
// require a config.toml, so a Group set up data-first (as some tests do) is
// still seen.
func groupAliases(p nodedir.Paths) ([]string, error) {
	seen := map[string]struct{}{}
	for _, root := range []string{p.GroupsConfigDir(), p.GroupsDir()} {
		entries, err := os.ReadDir(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				seen[e.Name()] = struct{}{}
			}
		}
	}
	aliases := make([]string, 0, len(seen))
	for a := range seen {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	return aliases, nil
}

// readCurrentGroup reads the Current Group pointer. "" means unset.
func readCurrentGroup(p nodedir.Paths) (string, error) {
	raw, err := os.ReadFile(p.CurrentGroupFile())
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// writeCurrentGroup sets the Current Group pointer to alias, 0600.
func writeCurrentGroup(p nodedir.Paths, alias string) error {
	return os.WriteFile(p.CurrentGroupFile(), []byte(alias+"\n"), 0o600)
}

// currentAlias resolves the Current Group an in-Group command acts on. With
// no Group scaffolded yet it points the operator at `group new`; with Groups
// present but no valid Current Group pointer, at `group use` (ADR 0006).
func currentAlias(p nodedir.Paths) (string, error) {
	aliases, err := configGroupAliases(p)
	if err != nil {
		return "", exitOperational(err)
	}
	if len(aliases) == 0 {
		return "", exitOperationalf("this Node has no Group; run `defios group new <alias> --headscale <url>`")
	}
	current, err := readCurrentGroup(p)
	if err != nil {
		return "", exitOperational(err)
	}
	if contains(aliases, current) {
		return current, nil
	}
	return "", exitOperationalf("no Current Group; run `defios group use <alias>` (have: %s)", strings.Join(aliases, ", "))
}

// readAlias resolves the Group a read or local verb acts on when none is
// named: the Current Group when the pointer names a Group on this Node, else
// the sole Group when there is exactly one (so reads work before `group
// use`), else an error naming the choices (#27). Callers that must not error
// for a Node with no Group (e.g. `node inspect` printing "none") check
// groupAliases first.
func readAlias(p nodedir.Paths) (string, error) {
	aliases, err := groupAliases(p)
	if err != nil {
		return "", exitOperational(err)
	}
	if len(aliases) == 0 {
		return "", exitOperationalf("this Node has no Group; run `defios group new <alias> --headscale <url>`")
	}
	current, err := readCurrentGroup(p)
	if err != nil {
		return "", exitOperational(err)
	}
	if contains(aliases, current) {
		return current, nil
	}
	if len(aliases) == 1 {
		return aliases[0], nil
	}
	return "", exitOperationalf("no Current Group; run `defios group use <alias>` (have: %s)", strings.Join(aliases, ", "))
}

// deviceDisplayName reads the device-level default display name written by
// `init --name`. "" when the file is absent.
func deviceDisplayName(p nodedir.Paths) (string, error) {
	raw, err := os.ReadFile(p.DisplayNameFile())
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
