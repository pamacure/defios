// Package backup writes and opens a Backup: one Node sealed into a single
// age (scrypt) file under its Vault passphrase, holding everything needed to
// bring the same Node back on another machine and nothing its peers give
// back or that belongs to one machine alone (ADR 0027).
//
// Inside the age layer is a tar archive. Its first entry is manifest.json,
// which carries the format version; every other entry is a file named
// data/<path> or config/<path>, relative to the Node's data and config
// trees, so a restore writes them into this machine's resolved paths.
package backup

import (
	"archive/tar"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/nodedir"
	"defios.com/internal/vault"
)

// FormatVersion is the archive format this build writes and the only one it
// opens.
const FormatVersion = 1

const manifestName = "manifest.json"

var (
	// ErrNoNode means there is no Node to back up: no identity or no Vault.
	ErrNoNode = errors.New("backup: no Node here")
	// ErrUnknownFormat is a Backup whose format version this build does not
	// know.
	ErrUnknownFormat = errors.New("backup: unknown format")
)

// Manifest is what a Backup says about itself: its format and the Node,
// Groups and Keys it holds.
type Manifest struct {
	Version int     `json:"version"`
	NodeID  string  `json:"node_id"`
	Groups  []Group `json:"groups"`
}

// Group is one Group a Backup holds: its alias, its Group ID once a Record
// binds one, and the Keys this Node holds a share of in it.
type Group struct {
	Alias   string `json:"alias"`
	GroupID string `json:"group_id,omitempty"`
	Keys    []Key  `json:"keys"`
}

// Key is one Key this Node holds a share of, by curve and hex public key.
type Key struct {
	Curve     string `json:"curve"`
	PublicKey string `json:"public_key"`
}

// Archive is an opened Backup: its manifest and its files by archive name.
type Archive struct {
	Manifest Manifest
	Files    map[string][]byte
}

// LeftoverError names a vault.new/ or vault.old/ that a passphrase change
// left behind; until it is resolved the Vault is not one thing to back up.
type LeftoverError struct{ Dir string }

func (e *LeftoverError) Error() string {
	return fmt.Sprintf("backup: %s is left over from a passphrase change; finish or undo it first", e.Dir)
}

// Write seals the Node at p into a new file dst (0600) under passphrase. It
// opens every Vault file with passphrase first and writes nothing if any
// fails (vault.ErrBadPassphrase, wrapped with the file's name). An existing
// dst is refused with an error matching os.ErrExist.
func Write(p nodedir.Paths, passphrase, dst string) (Manifest, error) {
	for _, dir := range []string{p.VaultDir() + ".new", p.VaultDir() + ".old"} {
		if _, err := os.Stat(dir); err == nil {
			return Manifest{}, &LeftoverError{Dir: dir}
		}
	}
	if _, err := os.Lstat(dst); err == nil {
		return Manifest{}, fmt.Errorf("backup: %s: %w", dst, os.ErrExist)
	}
	files, m, err := collect(p, passphrase)
	if err != nil {
		return Manifest{}, err
	}
	sealed, err := seal(m, files, passphrase)
	if err != nil {
		return Manifest{}, err
	}
	if err := writeNew(dst, sealed); err != nil {
		return Manifest{}, fmt.Errorf("backup: %w", err)
	}
	return m, nil
}

// writeNew claims dst with an exclusive create, so an existing file is never
// overwritten, then writes raw beside it and renames it over the claim, so a
// crash leaves an empty dst rather than a truncated Backup.
func writeNew(dst string, raw []byte) error {
	claim, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	claim.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		os.Remove(dst)
		return err
	}
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
		os.Remove(dst)
	}
	return err
}

// collect gathers the archive's files and builds its manifest.
func collect(p nodedir.Paths, passphrase string) (map[string][]byte, Manifest, error) {
	files := map[string][]byte{}
	pub, err := identity.ReadFile(p.IdentityFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, Manifest{}, ErrNoNode
	} else if err != nil {
		return nil, Manifest{}, fmt.Errorf("backup: %w", err)
	}
	if files["data/identity"], err = os.ReadFile(p.IdentityFile()); err != nil {
		return nil, Manifest{}, fmt.Errorf("backup: %w", err)
	}
	m := Manifest{Version: FormatVersion, NodeID: pub.NodeID.String()}

	groups := map[string]*Group{}
	groupOf := func(alias string) *Group {
		if groups[alias] == nil {
			groups[alias] = &Group{Alias: alias, Keys: []Key{}}
		}
		return groups[alias]
	}

	entries, err := os.ReadDir(p.VaultDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, Manifest{}, ErrNoNode
	} else if err != nil {
		return nil, Manifest{}, fmt.Errorf("backup: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		kind, alias, ok := vaultFile(name)
		if !e.Type().IsRegular() || !ok {
			continue // a Seal's temp file, or not the Vault's
		}
		raw, err := os.ReadFile(filepath.Join(p.VaultDir(), name))
		if err != nil {
			return nil, Manifest{}, fmt.Errorf("backup: %w", err)
		}
		contents, err := vault.OpenBytes(raw, passphrase)
		if err != nil {
			return nil, Manifest{}, fmt.Errorf("backup: vault/%s: %w", name, err)
		}
		files["data/vault/"+name] = raw
		switch kind {
		case "shares":
			g := groupOf(alias)
			for _, sh := range contents.Shares {
				g.Keys = append(g.Keys, Key{Curve: sh.Curve, PublicKey: hex.EncodeToString(sh.PublicKey)})
			}
		case "secrets":
			groupOf(alias)
		}
	}
	if files["data/vault/shared"] == nil {
		return nil, Manifest{}, ErrNoNode
	}

	records, err := filepath.Glob(filepath.Join(p.GroupsDir(), "*", "roster.json"))
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("backup: %w", err)
	}
	for _, record := range records {
		alias := filepath.Base(filepath.Dir(record))
		if group.ValidateAlias(alias) != nil {
			continue
		}
		rec, err := group.LoadAlias(p.GroupsDir(), alias)
		if err != nil {
			return nil, Manifest{}, fmt.Errorf("backup: %w", err)
		}
		if files["data/group/"+alias+"/roster.json"], err = os.ReadFile(record); err != nil {
			return nil, Manifest{}, fmt.Errorf("backup: %w", err)
		}
		groupOf(alias).GroupID = rec.GroupID().String()
	}

	if err := collectConfig(p.Config, files); err != nil {
		return nil, Manifest{}, fmt.Errorf("backup: %w", err)
	}

	for _, g := range groups {
		m.Groups = append(m.Groups, *g)
	}
	sort.Slice(m.Groups, func(i, j int) bool { return m.Groups[i].Alias < m.Groups[j].Alias })
	if m.Groups == nil {
		m.Groups = []Group{}
	}
	return files, m, nil
}

// vaultFile reports whether name is one of the Vault's age files: the
// Node's shared file, or a Group's shares or Secrets file, with its alias.
func vaultFile(name string) (kind, alias string, ok bool) {
	if name == "shared" {
		return "shared", "", true
	}
	for _, kind := range []string{"shares", "secrets"} {
		if alias, ok := strings.CutPrefix(name, kind+"-"); ok && group.ValidateAlias(alias) == nil {
			return kind, alias, true
		}
	}
	return "", "", false
}

// dataTaken reports whether a Backup takes the data tree's file at rel
// (slash-separated): the identity, a Vault file, or a Group's Record.
func dataTaken(rel string) bool {
	parts := strings.Split(rel, "/")
	switch {
	case len(parts) == 1:
		return rel == "identity"
	case len(parts) == 2 && parts[0] == "vault":
		_, _, ok := vaultFile(parts[1])
		return ok
	case len(parts) == 3 && parts[0] == "group":
		return group.ValidateAlias(parts[1]) == nil && parts[2] == "roster.json"
	}
	return false
}

// configRoot is what a Backup takes from the root of the config tree. With
// --dir the data tree is the same directory, so anything else there is data.
var configRoot = map[string]bool{"display-name": true, "current-group": true, "agents": true, "group": true}

// groupData is what a Group's data directory holds. With --dir it is also
// the Group's Workspace, and none of it is the Workspace's: the Record is
// taken from the data tree, the rest is not taken at all.
var groupData = map[string]bool{
	"roster.json": true, "raft.db": true, "raft-snap": true, "tsnet": true,
	"outbox.json": true, "conversations": true,
	"local-accept.json": true, "pending-shares.json": true,
}

// configTaken reports whether a Backup takes the config tree's entry at rel
// (slash-separated): not a Recap or Transcripts, and not data.
func configTaken(rel string, isDir bool) bool {
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	switch {
	case name == "RECAP.md" || (isDir && name == "transcripts"):
		return false
	case len(parts) == 1:
		return configRoot[name]
	case len(parts) == 3 && parts[0] == "group":
		return !groupData[name] && !strings.HasPrefix(name, "cosl.sqlite") && !strings.HasPrefix(name, ".")
	}
	return true
}

// collectConfig adds the config tree's files that configTaken takes,
// following a symlink to a regular file.
func collectConfig(root string, files map[string][]byte) error {
	return filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && full == root {
				return filepath.SkipDir
			}
			return err
		}
		rel, err := filepath.Rel(root, full)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch taken := configTaken(rel, d.IsDir()); {
		case d.IsDir() && !taken:
			return filepath.SkipDir
		case d.IsDir() || !taken:
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if info, err := os.Stat(full); err != nil || !info.Mode().IsRegular() {
				return nil
			}
		} else if !d.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		files["config/"+rel] = raw
		return nil
	})
}

// seal writes the manifest and files as a tar archive, sorted by name after
// the manifest, inside one age scrypt layer under passphrase.
func seal(m Manifest, files map[string][]byte, passphrase string) ([]byte, error) {
	manifest, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	recipient.SetWorkFactor(vault.WorkFactor)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	tw := tar.NewWriter(w)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	add := func(name string, body []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	if err := add(manifestName, manifest); err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	for _, name := range names {
		if err := add(name, files[name]); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	return buf.Bytes(), nil
}

// Open decrypts the Backup at src with passphrase. A wrong passphrase is
// vault.ErrBadPassphrase; a format this build does not know is
// ErrUnknownFormat. Every file is one Write would have taken, named once.
func Open(src, passphrase string) (*Archive, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ident, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	r, err := age.Decrypt(f, ident)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, vault.ErrBadPassphrase
		}
		return nil, fmt.Errorf("backup: %w", err)
	}
	tr := tar.NewReader(r)
	a := &Archive{Files: map[string][]byte{}}
	for first := true; ; first = false {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			if first {
				return nil, fmt.Errorf("backup: empty archive: %w", ErrUnknownFormat)
			}
			return a, nil
		} else if err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		if first {
			if h.Name != manifestName {
				return nil, fmt.Errorf("backup: no manifest: %w", ErrUnknownFormat)
			}
			if err := json.Unmarshal(body, &a.Manifest); err != nil {
				return nil, fmt.Errorf("backup: manifest: %w", err)
			}
			if a.Manifest.Version != FormatVersion {
				return nil, fmt.Errorf("backup: format %d: %w", a.Manifest.Version, ErrUnknownFormat)
			}
			continue
		}
		if _, dup := a.Files[h.Name]; dup || h.Typeflag != tar.TypeReg || !archived(h.Name) {
			return nil, fmt.Errorf("backup: unexpected entry %q", h.Name)
		}
		a.Files[h.Name] = body
	}
}

// archived reports whether name is a file Write would have put in a Backup:
// data/<rel> that dataTaken takes, or config/<rel> that configTaken takes
// along with every directory above it.
func archived(name string) bool {
	tree, rel, _ := strings.Cut(name, "/")
	if !filepath.IsLocal(rel) || path.Clean(rel) != rel {
		return false
	}
	switch tree {
	case "data":
		return dataTaken(rel)
	case "config":
		parts := strings.Split(rel, "/")
		for i := 1; i < len(parts); i++ {
			if !configTaken(strings.Join(parts[:i], "/"), true) {
				return false
			}
		}
		return configTaken(rel, false)
	}
	return false
}

// Restore writes an opened Backup's files into this machine's paths p: each
// data/ file under p.Data and each config/ file under p.Config, files 0600
// and directories 0700. It never overwrites a file. The identity and the
// Vault's shared file go last, so a Node exists at p only once everything
// else is in place; on an error the files and directories it made are
// removed, so a retry finds the target as it was.
func Restore(a *Archive, p nodedir.Paths) (err error) {
	names := make([]string, 0, len(a.Files))
	for name := range a.Files {
		names = append(names, name)
	}
	writtenLast := map[string]int{"data/vault/shared": 1, "data/identity": 2}
	sort.Slice(names, func(i, j int) bool {
		if writtenLast[names[i]] != writtenLast[names[j]] {
			return writtenLast[names[i]] < writtenLast[names[j]]
		}
		return names[i] < names[j]
	})
	var made []string // files and directories, in the order made
	defer func() {
		if err != nil {
			for i := len(made) - 1; i >= 0; i-- {
				os.Remove(made[i])
			}
		}
	}()
	for _, name := range names {
		tree, rel, _ := strings.Cut(name, "/")
		root := p.Data
		if tree == "config" {
			root = p.Config
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		dirs, err := mkdirs(filepath.Dir(dst))
		made = append(made, dirs...)
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		made = append(made, dst)
		if _, err = f.Write(a.Files[name]); err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	return nil
}

// mkdirs makes dir and any missing parents 0700, returning the ones it made,
// outermost first.
func mkdirs(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	var made []string
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o700); err != nil {
			return made, err
		}
		made = append(made, missing[i])
	}
	return made, nil
}
