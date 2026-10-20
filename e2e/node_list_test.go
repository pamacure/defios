//go:build e2e

package e2e

import (
	"encoding/json"
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

	var aliceID, bobID struct {
		NodeID string `json:"node_id"`
	}
	json.Unmarshal([]byte(alice.run(t.Context(), "node", "inspect", "--json").stdout), &aliceID)
	json.Unmarshal([]byte(bob.run(t.Context(), "node", "inspect", "--json").stdout), &bobID)

	seesBob := func(nodes []serve.NodeSummary) bool {
		return len(nodes) == 1 && nodes[0].NodeID == bobID.NodeID
	}
	seesAlice := func(nodes []serve.NodeSummary) bool {
		return len(nodes) == 1 && nodes[0].NodeID == aliceID.NodeID
	}

	got := alice.nodeList(t, seesBob)
	if got[0].DisplayName != "bob" || got[0].Hostname != "defios-bob" {
		t.Fatalf("alice sees %+v", got[0])
	}
	got = bob.nodeList(t, seesAlice)
	if got[0].DisplayName != "alice" || got[0].Hostname != "defios-alice" {
		t.Fatalf("bob sees %+v", got[0])
	}

	// node inspect reports serve-up while serve runs.
	var insp struct {
		ServeUp bool `json:"serve_up"`
	}
	json.Unmarshal([]byte(alice.run(t.Context(), "node", "inspect", "--json").stdout), &insp)
	if !insp.ServeUp {
		t.Fatal("alice node inspect: serve_up = false while serve runs")
	}
}
