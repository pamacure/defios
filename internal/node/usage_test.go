package node_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"defios.com/internal/node"
)

// Usage measures every Group, or one, from its files, with no Group
// served; the totals add up from their parts.
func TestUsageMeasuresTheNodesGroups(t *testing.T) {
	p := nodeDir(t)
	h, err := node.Open(p, node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := os.WriteFile(p.GroupDir("alpha")+"/raft.db", make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}

	u, err := h.Usage(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Groups) != 2 || u.Groups[0].Alias != "alpha" || u.Groups[1].Alias != "beta" {
		t.Fatalf("groups: %#v", u.Groups)
	}
	if u.Groups[0].Raft != 1000 || u.Groups[1].Raft != 0 {
		t.Fatalf("raft: alpha %d, beta %d; want 1000, 0", u.Groups[0].Raft, u.Groups[1].Raft)
	}
	if u.Groups[0].Workspace == 0 {
		t.Fatal("alpha's config.toml is not counted as Workspace")
	}
	if u.Data.Identity == 0 || u.Serve != nil {
		t.Fatalf("identity %d, serve %#v; want an identity and no serve", u.Data.Identity, u.Serve)
	}
	var groups int64
	for _, g := range u.Groups {
		groups += g.Bytes
	}
	if u.Data.Bytes+u.Config.Bytes != u.Data.Identity+u.Data.VaultShared+u.Config.Root+groups {
		t.Fatalf("data %d + config %d do not add up from their parts", u.Data.Bytes, u.Config.Bytes)
	}
	if u.Total != u.Binary.Bytes+u.Data.Bytes+u.Config.Bytes {
		t.Fatalf("total %d is not binary + data + config", u.Total)
	}

	one, err := h.Usage(ctx, "beta")
	if err != nil || len(one.Groups) != 1 || one.Groups[0].Alias != "beta" {
		t.Fatalf("beta alone: %#v, %v", one.Groups, err)
	}
	if _, err := h.Usage(ctx, "ghost"); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("ghost: %v; want ErrUnknownGroup", err)
	}
}
