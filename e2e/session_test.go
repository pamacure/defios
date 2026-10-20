//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"defios.com/internal/serve"
)

// TestSessionLocalAccept (§12, §11.4): a peer's session.start lands here.
// This Node emits no tss.session and does not start its engine until
// `session accept`. With no accept, the Session aborts by the injected
// WaitingFor.
func TestSessionLocalAccept(t *testing.T) {
	hs := startHeadscale(t)
	alice := newNode(t, hs, "alice")
	bob := newNode(t, hs, "bob")
	for _, n := range []*node{alice, bob} {
		n.env["DEFIOS_SESSION_ENGINE"] = "exchange"
		n.env["DEFIOS_SESSION_TIMEOUT"] = "4s"
	}
	alice.startServe(t)
	bob.startServe(t)
	aliceID := alice.inspect(t).NodeID
	bobID := bob.inspect(t).NodeID
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
	var accepted serve.GroupView
	bob.mustJSONVerb(t, &accepted, "group", "accept", created.GroupID)
	waitRaft(t, alice)
	waitRaft(t, bob)
	// Both membership.join rows mean a 2-Node commit can land.
	alice.coslLog(t, func(rows []coslLine) bool {
		n := 0
		for _, r := range rows {
			if r.TypeName == "membership.join" {
				n++
			}
		}
		return n >= 2
	})

	// No accept: the peer logs the wait, emits nothing, and the Session
	// aborts once the injected wait passes — not immediately.
	var started sessionView
	alice.mustJSONVerb(t, &started, "session", "start", "--curve", "ecdsa")
	bob.coslLog(t, func(rows []coslLine) bool { return hasStart(rows, started.SessionID) })
	time.Sleep(400 * time.Millisecond)
	early := bob.coslNow(t)
	if hasType(early, "session.abort") {
		t.Fatal("session aborted immediately, not by the wait")
	}
	if n := from(early, bobID, "tss.session"); len(n) != 0 {
		t.Fatalf("bob emitted tss.session before accept: %+v", n)
	}
	if !strings.Contains(bob.logs.String(), "session "+started.SessionID+" waiting for accept") {
		t.Fatalf("bob did not log the wait:\n%s", bob.logs.String())
	}
	bob.coslLog(t, func(rows []coslLine) bool { return hasType(rows, "session.abort") })
	if n := from(bob.coslNow(t), bobID, "tss.session"); len(n) != 0 {
		t.Fatalf("bob emitted tss.session without accepting: %+v", n)
	}
	var inspected sessionView
	bob.mustJSONVerb(t, &inspected, "session", "inspect", started.SessionID)
	if inspected.Status != "aborted" || inspected.Curve != "ecdsa" || inspected.T != 2 || inspected.N != 2 {
		t.Fatalf("inspect = %+v", inspected)
	}

	// A later Session, accepted, does start the engine: bob emits tss.session.
	var again sessionView
	alice.mustJSONVerb(t, &again, "session", "start", "--curve", "eddsa")
	bob.coslLog(t, func(rows []coslLine) bool { return hasStart(rows, again.SessionID) })
	if n := from(bob.coslNow(t), bobID, "tss.session"); len(n) != 0 {
		t.Fatalf("bob emitted before the second accept: %+v", n)
	}
	bob.mustJSONVerb(t, &sessionView{}, "session", "accept", again.SessionID)
	bob.coslLog(t, func(rows []coslLine) bool {
		return len(from(rows, bobID, "tss.session")) > 0
	})
	alice.coslLog(t, func(rows []coslLine) bool {
		return len(from(rows, bobID, "tss.session")) > 0
	})
}

type sessionView struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Curve     string `json:"curve"`
	T         uint32 `json:"t"`
	N         uint32 `json:"n"`
}

func waitRaft(t *testing.T, n *node) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	client := serve.Client{Socket: n.sock}
	for time.Now().Before(deadline) {
		st, err := client.Status(context.Background())
		if err == nil && st.RaftUp {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s raft never came up", n.name)
}

func (n *node) coslNow(t *testing.T) []coslLine {
	t.Helper()
	r := n.run(context.Background(), "cosl", "log", "--json")
	if r.code != 0 {
		t.Fatalf("%s cosl log: exit %d: %s", n.name, r.code, r.stderr)
	}
	var rows []coslLine
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil {
		t.Fatalf("%s cosl log: %v\n%s", n.name, err, r.stdout)
	}
	return rows
}

func hasStart(rows []coslLine, id string) bool {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	for _, r := range rows {
		if r.TypeName == "session.start" && strings.Contains(r.Summary, "session="+short) {
			return true
		}
	}
	return false
}

func hasType(rows []coslLine, name string) bool {
	for _, r := range rows {
		if r.TypeName == name {
			return true
		}
	}
	return false
}

func from(rows []coslLine, sender, typ string) []coslLine {
	var out []coslLine
	for _, r := range rows {
		if r.Sender == sender && r.TypeName == typ {
			out = append(out, r)
		}
	}
	return out
}
