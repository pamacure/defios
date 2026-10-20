//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"defios.com/internal/serve"
)

// TestFormation (§12, §11.5): alice proposes, bob accepts, both `group
// inspect` outputs agree on Roster and Group ID with status valid; Raft
// starts on valid, both membership.join rows land on both replicas, and
// `cosl log` is identical on both Nodes. A second proposal into the
// occupied slot is refused.
func TestFormation(t *testing.T) {
	hs := startHeadscale(t)
	alice := newNode(t, hs, "alice")
	bob := newNode(t, hs, "bob")
	carol := newNode(t, hs, "carol")
	alice.startServe(t)
	bob.startServe(t)
	carol.startServe(t)

	aliceID, bobID := alice.inspect(t).NodeID, bob.inspect(t).NodeID
	// Wait until discovery sees bob; formation posts ride the same path.
	alice.nodeList(t, func(nodes []serve.NodeSummary) bool {
		for _, n := range nodes {
			if n.NodeID == bobID {
				return true
			}
		}
		return false
	})

	var created serve.GroupView
	alice.mustJSONVerb(t, &created, "group", "create", aliceID, bobID)
	if created.Status != "pending" || created.GroupID == "" {
		t.Fatalf("create = %+v", created)
	}

	// Bob sees it pending without touching serve.
	var listed []struct {
		GroupID string `json:"group_id"`
		Status  string `json:"status"`
	}
	bob.mustJSONVerb(t, &listed, "group", "list")
	if len(listed) != 1 || listed[0].GroupID != created.GroupID || listed[0].Status != "pending" {
		t.Fatalf("bob group list = %+v", listed)
	}

	// Carol cannot propose into bob's occupied slot: protocol reject, exit 3.
	carol.nodeList(t, func(nodes []serve.NodeSummary) bool {
		for _, n := range nodes {
			if n.NodeID == bobID {
				return true
			}
		}
		return false
	})
	r := carol.run(context.Background(), "group", "create", carol.inspect(t).NodeID, bobID)
	if r.code != 3 {
		t.Fatalf("carol create into occupied bob: exit %d, stderr %s", r.code, r.stderr)
	}

	var accepted serve.GroupView
	bob.mustJSONVerb(t, &accepted, "group", "accept", created.GroupID)
	if accepted.Status != "valid" || len(accepted.Undelivered) != 0 {
		t.Fatalf("accept = %+v", accepted)
	}

	ia, ib := alice.groupInspect(t), bob.groupInspect(t)
	if ia.Status != "valid" || ib.Status != "valid" {
		t.Fatalf("status alice=%s bob=%s", ia.Status, ib.Status)
	}
	if ia.GroupID != created.GroupID || ib.GroupID != created.GroupID {
		t.Fatalf("group ids: %s %s %s", created.GroupID, ia.GroupID, ib.GroupID)
	}
	if len(ia.Roster) != 2 || len(ib.Roster) != 2 {
		t.Fatalf("roster sizes %d %d", len(ia.Roster), len(ib.Roster))
	}
	for i := range ia.Roster {
		a, b := ia.Roster[i], ib.Roster[i]
		if a.NodeID != b.NodeID || a.AuthPublicKey != b.AuthPublicKey || a.EncryptionPublicKey != b.EncryptionPublicKey ||
			a.DisplayName != b.DisplayName || a.Hostname != b.Hostname {
			t.Fatalf("roster row %d differs:\n%+v\n%+v", i, a, b)
		}
		if !a.Reachable {
			t.Fatalf("alice sees %s unreachable while both serve", a.DisplayName)
		}
	}
	for _, want := range []string{"alice", "bob", "defios-alice", "defios-bob"} {
		if !strings.Contains(mustMarshal(t, ia), want) {
			t.Fatalf("alice inspect missing %q: %+v", want, ia)
		}
	}
	if gid := alice.run(context.Background(), "node", "inspect"); !strings.Contains(gid.stdout, created.GroupID) {
		t.Fatalf("node inspect does not show the Group:\n%s", gid.stdout)
	}

	// Raft on valid: both membership.join rows reach both replicas (§6.2
	// step 6) and the logs are identical (§11.5).
	bothJoined := func(rows []coslLine) bool {
		if len(rows) != 2 {
			return false
		}
		for _, r := range rows {
			if r.TypeName != "membership.join" {
				return false
			}
		}
		return true
	}
	la, rawA := alice.coslLog(t, bothJoined)
	_, rawB := bob.coslLog(t, bothJoined)
	if rawA != rawB {
		t.Fatalf("cosl log differs:\nalice %s\nbob   %s", rawA, rawB)
	}
	senders := map[string]string{la[0].Sender: la[0].SenderDisplayName, la[1].Sender: la[1].SenderDisplayName}
	if senders[aliceID] != "alice" || senders[bobID] != "bob" {
		t.Fatalf("join senders = %v", senders)
	}
	for _, n := range []*node{alice, bob} {
		for _, row := range n.groupInspect(t).Roster {
			if !row.Joined {
				t.Fatalf("%s group inspect: %s joined=no after both joins committed", n.name, row.DisplayName)
			}
		}
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
