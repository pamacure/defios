package node

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"defios.com/internal/cosl"
	"defios.com/internal/nodedir"
	"defios.com/internal/serve"
)

// Usage is the Node's disk, read straight from its trees so it needs no
// Group served, and the serving process's own use when one serves it.
// Sizes are apparent (byte length), in bytes.
type Usage struct {
	Binary PathUsage // the executable hosting this Node
	Data   DataUsage
	Config ConfigUsage
	Groups []GroupUsage
	Total  int64 // binary + data + config
	// Serve is the process serving this Node, this one or another; nil
	// when none does.
	Serve *ProcessUsage
}

// ProcessUsage is a serving process's memory and CPU.
type ProcessUsage = serve.Usage

// PathUsage is one file's size.
type PathUsage struct {
	Path  string
	Bytes int64
}

// DataUsage is the data tree: the Node-wide identity and Vault shared file
// plus the data side of every Group measured.
type DataUsage struct {
	Path        string
	Identity    int64
	VaultShared int64
	Bytes       int64
}

// ConfigUsage is the config tree: its Node-wide root files plus the
// Workspace of every Group measured.
type ConfigUsage struct {
	Path  string
	Root  int64
	Bytes int64
}

// GroupUsage is one Group's share of both trees.
type GroupUsage struct {
	Alias     string
	COSL      COSLUsage
	Raft      int64
	Tsnet     int64
	Vault     int64 // its shares and Secrets files
	Workspace int64
	Other     int64 // the rest of its data directory: roster, pending files
	Bytes     int64
}

// COSLUsage is a Group's COSL replica: its size and row counts.
type COSLUsage struct {
	Bytes int64
	cosl.Counts
}

// DataBytes is what of the Group lives in the data tree.
func (u GroupUsage) DataBytes() int64 { return u.COSL.Bytes + u.Raft + u.Tsnet + u.Vault + u.Other }

// Usage measures the Node and group, or every Group when group is "".
// It holds the Vault's lock shared while it reads, since mid-change or
// after an interrupted one the Vault's sizes are not the Vault's.
func (h *Host) Usage(ctx context.Context, group string) (Usage, error) {
	unlock, err := useVault(h.p)
	if errors.Is(err, os.ErrNotExist) {
		unlock = func() {}
	} else if err != nil {
		return Usage{}, err
	}
	defer unlock()
	aliases := []string{group}
	if group == "" {
		if aliases, err = h.aliases(); err != nil {
			return Usage{}, err
		}
	} else if err := h.knownGroup(group); err != nil {
		return Usage{}, err
	}

	p := h.p
	u := Usage{
		Data:   DataUsage{Path: p.Data, Identity: sizeOf(p.IdentityFile()), VaultShared: sizeOf(p.VaultSharedFile())},
		Config: ConfigUsage{Path: p.Config, Root: sizeOf(p.CurrentGroupFile()) + sizeOf(p.DisplayNameFile())},
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		u.Binary = PathUsage{Path: exe, Bytes: sizeOf(exe)}
	}
	u.Data.Bytes = u.Data.Identity + u.Data.VaultShared
	u.Config.Bytes = u.Config.Root
	for _, alias := range aliases {
		gu, err := groupDisk(p, alias)
		if err != nil {
			return Usage{}, err
		}
		u.Groups = append(u.Groups, gu)
		u.Data.Bytes += gu.DataBytes()
		u.Config.Bytes += gu.Workspace
	}
	u.Total = u.Binary.Bytes + u.Data.Bytes + u.Config.Bytes

	switch su, err := (serve.Client{Socket: p.Socket}).Usage(ctx); {
	case err == nil:
		u.Serve = &su
	case errors.Is(err, serve.ErrNotRunning):
	default:
		return Usage{}, err
	}
	return u, nil
}

// groupDisk measures one Group across both trees. When config and data are
// one tree (--dir), group/<alias>/ holds the Workspace too: config.toml,
// agent markdown and one directory per Conversation Workspace sit beside
// the data, and are counted as Workspace, never as other.
func groupDisk(p nodedir.Paths, alias string) (GroupUsage, error) {
	u := GroupUsage{
		Alias: alias,
		Vault: sizeOf(p.VaultSharesFile(alias)) + sizeOf(p.VaultSecretsFile(alias)),
	}
	shared := filepath.Clean(p.Config) == filepath.Clean(p.Data)
	if !shared {
		u.Workspace = sizeOf(p.GroupConfigDir(alias))
	}
	entries, err := os.ReadDir(p.GroupDir(alias))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return GroupUsage{}, err
	}
	for _, e := range entries {
		name, n := e.Name(), sizeOf(filepath.Join(p.GroupDir(alias), e.Name()))
		switch {
		case strings.HasPrefix(name, "cosl.sqlite"):
			u.COSL.Bytes += n
		case name == "raft.db" || name == "raft-snap":
			u.Raft += n
		case name == "tsnet":
			u.Tsnet += n
		case shared && (name == "config.toml" || strings.HasSuffix(name, ".md") || e.IsDir()):
			u.Workspace += n
		default:
			u.Other += n
		}
	}
	if path := p.CoslFile(alias); sizeOf(path) > 0 {
		store, err := cosl.Open(path)
		if err != nil {
			return GroupUsage{}, err
		}
		defer store.Close()
		if u.COSL.Counts, err = store.Counts(); err != nil {
			return GroupUsage{}, err
		}
	}
	u.Bytes = u.DataBytes() + u.Workspace
	return u, nil
}

// sizeOf is the apparent size of a file, or of every regular file under a
// directory; 0 when path does not exist.
func sizeOf(path string) int64 {
	var n int64
	filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}
