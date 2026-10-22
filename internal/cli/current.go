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

// soleGroupDataAlias returns the alias of the one Group data subtree that
// holds a roster.json. With a Node limited to one Group this is that Group's
// alias; "" when no Group has a Record on disk.
func soleGroupDataAlias(p nodedir.Paths) (string, error) {
	matches, err := filepath.Glob(filepath.Join(p.GroupsDir(), "*", "roster.json"))
	if err != nil || len(matches) == 0 {
		return "", err
	}
	return filepath.Base(filepath.Dir(matches[0])), nil
}

// soleSharesFile returns the one per-Group shares file in the Vault. With a
// Node limited to one Group this is that Group's shares; "" when no Group has
// minted a Key yet.
func soleSharesFile(p nodedir.Paths) (string, error) {
	matches, err := filepath.Glob(p.VaultSharesFile("*"))
	if err != nil || len(matches) == 0 {
		return "", err
	}
	return matches[0], nil
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
