//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"defios.com/internal/serve"
)

// TestKeyScopedConversation (§8, §9.4): both Nodes send Group-level messages
// and messages in each minted Key's Conversation. Each filter returns only
// its rows, and the two Nodes print the same log. key inspect's Conversation
// ID is what cosl decode shows on a Key-scoped row.
func TestKeyScopedConversation(t *testing.T) {
	hs := startHeadscale(t)
	alice, bob := initKeygenNodes(t, hs)
	startKeygenGroup(t, alice, bob)
	_, ecdsaPub := mintCurve(t, alice, bob, "ecdsa")
	_, eddsaPub := mintCurve(t, alice, bob, "eddsa")

	var ecdsaKey, eddsaKey keyView
	alice.mustJSONVerb(t, &ecdsaKey, "key", "inspect", "ecdsa:"+ecdsaPub)
	bob.mustJSONVerb(t, &eddsaKey, "key", "inspect", "eddsa:"+eddsaPub)
	if ecdsaKey.ConversationID == "" || eddsaKey.ConversationID == "" || ecdsaKey.ConversationID == eddsaKey.ConversationID {
		t.Fatalf("conversation ids: %+v %+v", ecdsaKey, eddsaKey)
	}

	var scoped serve.SendView
	alice.mustJSONVerb(t, &scoped, "conversation", "send", "--key", "ecdsa:"+ecdsaPub, "ecdsa from alice")
	for _, n := range []*node{alice, bob} {
		decoded := waitDecode(t, n, scoped.MsgID)
		if decoded.ConversationID == nil || *decoded.ConversationID != ecdsaKey.ConversationID {
			t.Fatalf("%s decode conversation %v, key inspect %s", n.name, decoded.ConversationID, ecdsaKey.ConversationID)
		}
	}

	bob.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "--key", ecdsaKey.ConversationID, "ecdsa from bob")
	alice.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "--key", eddsaPub[:8], "eddsa from alice")
	bob.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "--key", "eddsa:"+eddsaPub, "eddsa from bob")

	var group serve.SendView
	alice.mustJSONVerb(t, &group, "conversation", "send", "group from alice")
	groupDecoded := waitDecode(t, alice, group.MsgID)
	if groupDecoded.ConversationID != nil {
		t.Fatalf("group-level conversation_id = %s", *groupDecoded.ConversationID)
	}
	bob.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "group from bob")

	want := map[string][]string{
		"":                      {"group from alice", "group from bob"},
		"ecdsa:" + ecdsaPub:     {"ecdsa from alice", "ecdsa from bob"},
		eddsaKey.ConversationID: {"eddsa from alice", "eddsa from bob"},
	}
	for query, texts := range want {
		args := []string{"conversation", "log"}
		if query != "" {
			args = append(args, "--key", query)
		}
		aliceOut := waitConversation(t, alice, args, texts)
		bobOut := waitConversation(t, bob, args, texts)
		if aliceOut != bobOut {
			t.Fatalf("--key %q logs differ:\n%s\n%s", query, aliceOut, bobOut)
		}
	}
}

type decodedRow struct {
	ConversationID *string `json:"conversation_id"`
}

// waitDecode polls cosl decode until the row is on this replica. A follower
// can apply just after the leader's send returns.
func waitDecode(t *testing.T, n *node, msgID string) decodedRow {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last result
	for time.Now().Before(deadline) {
		last = n.run(context.Background(), "cosl", "decode", msgID, "--json")
		if last.code == 0 {
			var d decodedRow
			if err := json.Unmarshal([]byte(last.stdout), &d); err != nil {
				t.Fatalf("%s decode: %v\n%s", n.name, err, last.stdout)
			}
			return d
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s decode %s: exit %d: %s", n.name, msgID, last.code, last.stderr)
	return decodedRow{}
}

// waitConversation polls conversation log --json until its texts are exactly
// want, in order, and returns that stdout.
func waitConversation(t *testing.T, n *node, args, want []string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last result
	for time.Now().Before(deadline) {
		last = n.run(context.Background(), append(args, "--json")...)
		if last.code == 0 {
			var rows []struct {
				Text string `json:"text"`
			}
			if err := unmarshalLines(last.stdout, &rows); err == nil && len(rows) == len(want) {
				ok := true
				for i := range want {
					if rows[i].Text != want[i] {
						ok = false
					}
				}
				if ok {
					return last.stdout
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s %v: want texts %q, last exit %d\n%s\n%s", n.name, args, want, last.code, last.stdout, last.stderr)
	return ""
}
