package node_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/node"
	"defios.com/internal/plugin"
)

const auditManifest = `name = "audit"
version = "1.0.0"
description = "Audits contracts."

[mcp.scanner]
command = "node"
args = ["server.js", "--stdio"]
env = ["ETHERSCAN_KEY"]
`

const auditSkill = `---
name: review
description: How to review a contract.
---
Read the contract first.
`

// pluginSource is a local directory holding the audit Plugin at version.
func writePluginSource(t *testing.T, dir, version string) {
	t.Helper()
	for name, body := range map[string]string{
		"plugin.toml":            strings.Replace(auditManifest, `version = "1.0.0"`, `version = "`+version+`"`, 1),
		"skills/review/SKILL.md": auditSkill,
	} {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A Plugin installs from a path once its stage is committed, and lists
// with its Skills and MCP Servers beside the built-ins; it updates from
// its source, and is removed only once no Agent Profile grants an item of
// it.
func TestAPluginIsInstalledUpdatedAndRemoved(t *testing.T) {
	h, err := node.Open(nodeDir(t), node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src := t.TempDir()
	writePluginSource(t, src, "1.0.0")

	s, err := h.StagePluginInstall(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	wantPlugin := node.Plugin{Name: "audit", Version: "1.0.0", Description: "Audits contracts.",
		Skills:  []node.PluginSkill{{Name: "review", Description: "How to review a contract."}},
		Servers: []node.PluginServer{{Name: "scanner", CommandLine: "node server.js --stdio", Env: []string{"ETHERSCAN_KEY"}}}}
	if got := s.Plugin; got.Name != wantPlugin.Name || !reflect.DeepEqual(got.Skills, wantPlugin.Skills) || !reflect.DeepEqual(got.Servers, wantPlugin.Servers) {
		t.Fatalf("staged %#v\nwant %#v", got, wantPlugin)
	}
	if list := installed(t, h); len(list) != 0 {
		t.Fatalf("installed before commit: %#v", list)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s.Entry.Source != src || s.Entry.Pin == "" || s.Entry.Version != "1.0.0" {
		t.Fatalf("pinned %#v", s.Entry)
	}
	list := installed(t, h)
	if len(list) != 1 || list[0].Source != src || list[0].Pin != s.Entry.Pin || !reflect.DeepEqual(list[0].Skills, wantPlugin.Skills) {
		t.Fatalf("installed %#v", list)
	}
	if names, err := h.InstalledPlugins(ctx); err != nil || !reflect.DeepEqual(names, []string{"audit"}) {
		t.Fatalf("installed names %v, %v; want [audit]", names, err)
	}
	if _, err := h.StagePluginInstall(ctx, src); !errors.Is(err, plugin.ErrInstalled) {
		t.Fatalf("installing it again: %v; want plugin.ErrInstalled", err)
	}

	if s, err = h.StagePluginUpdate(ctx, "audit"); err != nil || !s.UpToDate {
		t.Fatalf("update unchanged: %#v, %v; want up to date", s, err)
	}
	s.Close()
	writePluginSource(t, src, "1.1.0")
	if s, err = h.StagePluginUpdate(ctx, "audit"); err != nil || s.UpToDate || s.Was.Version != "1.0.0" || s.Entry.Version != "1.1.0" {
		t.Fatalf("update: %#v, %v", s, err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if list := installed(t, h); list[0].Version != "1.1.0" {
		t.Fatalf("after update %#v", list)
	}
	if _, err := h.StagePluginUpdate(ctx, "ghost"); !errors.Is(err, plugin.ErrNotInstalled) {
		t.Fatalf("update a ghost: %v; want plugin.ErrNotInstalled", err)
	}

	if _, err := h.AddAgentProfile(ctx, "auditor", node.ProfileEdit{Provider: ptr("claude")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Grant(ctx, "auditor", "audit/review", nil); err != nil {
		t.Fatal(err)
	}
	var granted *node.PluginGrantedError
	if err := h.RemovePlugin(ctx, "audit"); !errors.As(err, &granted) ||
		!reflect.DeepEqual(granted.Grants, []node.PluginGrant{{Handle: "auditor", Items: []string{"audit/review"}}}) {
		t.Fatalf("remove while granted: %v; want a PluginGrantedError naming auditor", err)
	}
	if _, _, err := h.Revoke(ctx, "auditor", "audit/review"); err != nil {
		t.Fatal(err)
	}
	if err := h.RemovePlugin(ctx, "audit"); err != nil {
		t.Fatal(err)
	}
	if list := installed(t, h); len(list) != 0 {
		t.Fatalf("after remove %#v", list)
	}
	if err := h.RemovePlugin(ctx, "audit"); !errors.Is(err, plugin.ErrNotInstalled) {
		t.Fatalf("remove again: %v; want plugin.ErrNotInstalled", err)
	}
}

// installed is the Plugins h lists that are not built in.
func installed(t *testing.T, h *node.Host) []node.Plugin {
	t.Helper()
	all, err := h.Plugins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []node.Plugin
	for _, p := range all {
		if !p.BuiltIn {
			out = append(out, p)
		}
	}
	return out
}
