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
	alice, bob := initKeygenNodes(t, hs)
	for _, n := range []*node{alice, bob} {
		if !strings.Contains(n.logs.String(), "generating ECDSA preparams") {
			t.Fatalf("%s init did not report preparam generation", n.name)
		}
		contents, err := vault.Open(filepath.Join(n.dir, "vault", "shared"), passphrase)
		if err != nil {
			t.Fatal(err)
		}
		var pre keygen.LocalPreParams
		if err := json.Unmarshal(contents.EcdsaPreparams, &pre); err != nil || !pre.ValidateWithProof() {
			t.Fatalf("%s preparams: %v", n.name, err)
		}
	}
	startKeygenGroup(t, alice, bob)
	sessionID, pub := mintCurve(t, alice, bob, "ecdsa")
	assertMintedKey(t, alice, bob, "ecdsa", sessionID, pub)
}

// TestKeygenEd25519_2of2 (§11.1, Ed25519 half): after the ECDSA Key exists,
// the same Group mints an Ed25519 Key in a second Session. key list on
// both Nodes prints both Keys, and key inspect reports share=yes.
func TestKeygenEd25519_2of2(t *testing.T) {
	hs := startHeadscale(t)
	alice, bob := initKeygenNodes(t, hs)
	startKeygenGroup(t, alice, bob)
	_, ecdsaPub := mintCurve(t, alice, bob, "ecdsa")
	sessionID, eddsaPub := mintCurve(t, alice, bob, "eddsa")
	assertMintedKey(t, alice, bob, "eddsa", sessionID, eddsaPub)

	aliceList := alice.run(context.Background(), "key", "list")
	bobList := bob.run(context.Background(), "key", "list")
	if aliceList.code != 0 || bobList.code != 0 {
		t.Fatalf("list alice %d %s bob %d %s", aliceList.code, aliceList.stderr, bobList.code, bobList.stderr)
	}
	if strings.TrimSpace(aliceList.stdout) != strings.TrimSpace(bobList.stdout) {
		t.Fatalf("lists differ:\n%s\n%s", aliceList.stdout, bobList.stdout)
	}
	for _, line := range []string{
		"ecdsa:" + ecdsaPub + "  t=2 n=2  share=yes",
		"eddsa:" + eddsaPub + "  t=2 n=2  share=yes",
	} {
		if !strings.Contains(aliceList.stdout, line) {
			t.Fatalf("list = %q, missing %s", aliceList.stdout, line)
		}
	}
}

// TestKeygenProposedAliasNamesTheKeyOnBothNodes: one session start --alias
// names the Key on both Nodes, without bob being asked for one (ADR 0033).
func TestKeygenProposedAliasNamesTheKeyOnBothNodes(t *testing.T) {
	hs := startHeadscale(t)
	alice, bob := initKeygenNodes(t, hs)
	startKeygenGroup(t, alice, bob)
	sessionID, pub := mintCurve(t, alice, bob, "eddsa", "--alias", "Cold")
	for _, n := range []*node{alice, bob} {
		v := waitKeyByAlias(t, n, "Cold")
		if v.Key != "eddsa:"+pub || v.Alias != "Cold" || v.SessionID != sessionID {
			t.Fatalf("%s inspect = %+v", n.name, v)
		}
	}
}

// TestKeygenOverrideNamesTheKeyOnOneNode: alice proposes an alias and bob
// accepts with its own, so one Key goes by a different name on each (ADR 0033).
func TestKeygenOverrideNamesTheKeyOnOneNode(t *testing.T) {
	hs := startHeadscale(t)
	alice, bob := initKeygenNodes(t, hs)
	startKeygenGroup(t, alice, bob)
	sessionID, pub := mintCurveAccepting(t, alice, bob, "eddsa", []string{"--alias", "Cold"}, []string{"--alias", "Vault"})
	for n, alias := range map[*node]string{alice: "Cold", bob: "Vault"} {
		v := waitKeyByAlias(t, n, alias)
		if v.Key != "eddsa:"+pub || v.Alias != alias || v.SessionID != sessionID {
			t.Fatalf("%s inspect = %+v", n.name, v)
		}
	}
}

// waitKeyByAlias waits for n to know a Key by alias: its Workspace is made
// shortly after the Key mints.
func waitKeyByAlias(t *testing.T, n *node, alias string) keyView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		r := n.run(context.Background(), "key", "inspect", alias, "--json")
		if r.code == 0 {
			var v keyView
			if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: key inspect %s: exit %d %s", n.name, alias, r.code, r.stderr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func initKeygenNodes(t *testing.T, hs *headscale) (alice, bob *node) {
	t.Helper()
	alice = newNodeOpts(t, hs, "alice", false)
	bob = newNodeOpts(t, hs, "bob", false)
	for _, n := range []*node{alice, bob} {
		n.env["DEFIOS_SESSION_TIMEOUT"] = "5m"
	}
	return alice, bob
}

func startKeygenGroup(t *testing.T, alice, bob *node) {
	t.Helper()
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
}

// mintCurve runs one t=2 Session to two agreeing completes and returns the
// Session ID and the canonical public-key hex.
func mintCurve(t *testing.T, alice, bob *node, curve string, startFlags ...string) (sessionID, pub string) {
	t.Helper()
	return mintCurveAccepting(t, alice, bob, curve, startFlags, nil)
}

// mintCurveAccepting is mintCurve with flags for bob's session accept too.
func mintCurveAccepting(t *testing.T, alice, bob *node, curve string, startFlags, acceptFlags []string) (sessionID, pub string) {
	t.Helper()
	var started sessionView
	alice.mustJSONVerb(t, &started, append([]string{"session", "start", "--curve", curve, "--t", "2"}, startFlags...)...)
	bob.coslLog(t, func(rows []coslLine) bool { return hasStart(rows, started.SessionID) })
	bob.mustJSONVerb(t, &sessionView{}, append([]string{"session", "accept", started.SessionID}, acceptFlags...)...)

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
	aliceHex := completeHexes(rows, started.SessionID, curve)
	bobHex := completeHexes(bobRows, started.SessionID, curve)
	if len(aliceHex) != 2 || aliceHex[0] == "" || aliceHex[0] != aliceHex[1] || aliceHex[0] != bobHex[0] || aliceHex[0] != bobHex[1] {
		t.Fatalf("completes disagree: alice %q bob %q", aliceHex, bobHex)
	}
	want := 66
	if curve == "eddsa" {
		want = 64
	}
	if len(aliceHex[0]) != want {
		t.Fatalf("%s public key hex length = %d, want %d", curve, len(aliceHex[0]), want)
	}
	return started.SessionID, aliceHex[0]
}

func assertMintedKey(t *testing.T, alice, bob *node, curve, sessionID, pub string) {
	t.Helper()
	var aliceKey, bobKey keyView
	alice.mustJSONVerb(t, &aliceKey, "key", "inspect", curve+":"+pub)
	bob.mustJSONVerb(t, &bobKey, "key", "inspect", aliceKey.Key)
	if !aliceKey.Share || !bobKey.Share || aliceKey.Key != bobKey.Key || aliceKey.Key != curve+":"+pub {
		t.Fatalf("inspect alice=%+v bob=%+v", aliceKey, bobKey)
	}
	if aliceKey.T != 2 || aliceKey.N != 2 || aliceKey.SessionID != sessionID || aliceKey.ConversationID == "" {
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
	Alias          string `json:"alias"`
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

func completeHexes(rows []coslLine, id, curve string) []string {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	var out []string
	for _, r := range rows {
		if r.TypeName != "session.complete" || !strings.Contains(r.Summary, "session="+short) {
			continue
		}
		_, hexPart, ok := strings.Cut(r.Summary, curve+":")
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
