//go:build e2e

package e2e

import (
	"testing"

	"defios.com/internal/serve"
)

// TestNodeListSeesPeerIdentity: two Nodes on one headscale tailnet each
// discover the other's signed Identity through `node list` (§6.1, §11.5
// first clause).
func TestNodeListSeesPeerIdentity(t *testing.T) {
	hs := startHeadscale(t)
	alice := newNode(t, hs, "alice")
	bob := newNode(t, hs, "bob")

	alice.startServe(t)
	bob.startServe(t)

	aliceID, bobID := alice.inspect(t).NodeID, bob.inspect(t).NodeID

	got := alice.nodeList(t, func(nodes []serve.NodeSummary) bool {
		return len(nodes) == 1 && nodes[0].NodeID == bobID
	})
	if got[0].DisplayName != "bob" || got[0].Hostname != "defios-bob" {
		t.Fatalf("alice sees %+v", got[0])
	}
	got = bob.nodeList(t, func(nodes []serve.NodeSummary) bool {
		return len(nodes) == 1 && nodes[0].NodeID == aliceID
	})
	if got[0].DisplayName != "alice" || got[0].Hostname != "defios-alice" {
		t.Fatalf("bob sees %+v", got[0])
	}

	// node inspect reports serve-up while serve runs.
	if !alice.inspect(t).ServeUp {
		t.Fatal("alice node inspect: serve_up = false while serve runs")
	}
}
