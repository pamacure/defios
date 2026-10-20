//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnb-chain/tss-lib/v4/ecdsa/keygen"

	"defios.com/internal/serve"
	"defios.com/internal/vault"
)

// TestKeygenECDSA_2of2 (§11.1, ECDSA half): a real tss-lib keygen on a
// 2-Node Group mints one secp256k1 Key. Each replica has one session.start
// and two agreeing session.completes, and both Nodes report the same
// ecdsa:<hex> with share=yes.
func TestKeygenECDSA_2of2(t *testing.T) {
	hs := startHeadscale(t)
	alice := newNodeOpts(t, hs, "alice", false)
	bob := newNodeOpts(t, hs, "bob", false)
	for _, n := range []*node{alice, bob} {
		if !strings.Contains(n.logs.String(), "generating ECDSA preparams") {
			t.Fatalf("%s init did not report preparam generation", n.name)
		}
		contents, err := vault.Open(filepath.Join(n.dir, "vault"), passphrase)
		if err != nil {
			t.Fatal(err)
		}
		var pre keygen.LocalPreParams
		if err := json.Unmarshal(contents.EcdsaPreparams, &pre); err != nil || !pre.ValidateWithProof() {
			t.Fatalf("%s preparams: %v", n.name, err)
		}
	}
	for _, n := range []*node{alice, bob} {
		n.env["DEFIOS_SESSION_TIMEOUT"] = "5m"
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
	alice.coslLog(t, func(rows []coslLine) bool { return countType(rows, "membership.join") >= 2 })

	var started sessionView
	alice.mustJSONVerb(t, &started, "session", "start", "--curve", "ecdsa", "--t", "2")
	bob.coslLog(t, func(rows []coslLine) bool { return hasStart(rows, started.SessionID) })
	bob.mustJSONVerb(t, &sessionView{}, "session", "accept", started.SessionID)

	rows := waitCompletes(t, alice, started.SessionID, 2)
	bobRows := waitCompletes(t, bob, started.SessionID, 2)
	for _, set := range [][]coslLine{rows, bobRows} {
		if n := countSession(set, "session.start", started.SessionID); n != 1 {
			t.Fatalf("session.start rows = %d", n)
		}
		if n := countSession(set, "session.complete", started.SessionID); n != 2 {
			t.Fatalf("session.complete rows = %d", n)
		}
	}
	aliceHex := completeHexes(rows, started.SessionID)
	bobHex := completeHexes(bobRows, started.SessionID)
	if len(aliceHex) != 2 || aliceHex[0] == "" || aliceHex[0] != aliceHex[1] || aliceHex[0] != bobHex[0] || aliceHex[0] != bobHex[1] {
		t.Fatalf("completes disagree: alice %q bob %q", aliceHex, bobHex)
	}

	var aliceKey, bobKey keyView
	alice.mustJSONVerb(t, &aliceKey, "key", "inspect", "ecdsa:"+aliceHex[0])
	bob.mustJSONVerb(t, &bobKey, "key", "inspect", aliceKey.Key)
	if !aliceKey.Share || !bobKey.Share || aliceKey.Key != bobKey.Key || !strings.HasPrefix(aliceKey.Key, "ecdsa:") {
		t.Fatalf("inspect alice=%+v bob=%+v", aliceKey, bobKey)
	}
	if aliceKey.T != 2 || aliceKey.N != 2 || aliceKey.SessionID != started.SessionID || aliceKey.ConversationID == "" {
		t.Fatalf("inspect = %+v", aliceKey)
	}
	aliceList := alice.run(context.Background(), "key", "list")
	bobList := bob.run(context.Background(), "key", "list")
	if aliceList.code != 0 || bobList.code != 0 {
		t.Fatalf("list alice %d %s bob %d %s", aliceList.code, aliceList.stderr, bobList.code, bobList.stderr)
	}
	if strings.TrimSpace(aliceList.stdout) != strings.TrimSpace(bobList.stdout) {
		t.Fatalf("lists differ:\n%s\n%s", aliceList.stdout, bobList.stdout)
	}
	if !strings.Contains(aliceList.stdout, aliceKey.Key+"  t=2 n=2  share=yes") {
		t.Fatalf("list = %q", aliceList.stdout)
	}
}

type keyView struct {
	Key            string `json:"key"`
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	T              int    `json:"t"`
	N              int    `json:"n"`
	Share          bool   `json:"share"`
}

func countType(rows []coslLine, name string) int {
	n := 0
	for _, r := range rows {
		if r.TypeName == name {
			n++
		}
	}
	return n
}

func countSession(rows []coslLine, typ, id string) int {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	n := 0
	for _, r := range rows {
		if r.TypeName == typ && strings.Contains(r.Summary, "session="+short) {
			n++
		}
	}
	return n
}

func completeHexes(rows []coslLine, id string) []string {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	var out []string
	for _, r := range rows {
		if r.TypeName != "session.complete" || !strings.Contains(r.Summary, "session="+short) {
			continue
		}
		_, hexPart, ok := strings.Cut(r.Summary, "ecdsa:")
		if ok {
			out = append(out, hexPart)
		}
	}
	return out
}

func waitCompletes(t *testing.T, n *node, id string, want int) []coslLine {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var last []coslLine
	for time.Now().Before(deadline) {
		last = n.coslNow(t)
		if countSession(last, "session.complete", id) >= want {
			return last
		}
		if countSession(last, "session.abort", id) > 0 {
			t.Fatalf("%s session aborted before %d completes:\n%+v", n.name, want, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s timed out waiting for %d completes, last:\n%+v", n.name, want, last)
	return nil
}
