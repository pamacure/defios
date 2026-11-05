package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/signreq"
)

// grantTools sets the tools of n's profile h, as agentNode writes it, to
// tools, a TOML array.
func grantTools(t *testing.T, n *node, h, tools string) {
	t.Helper()
	writeConfig(t, n.paths.AgentProfileFile(h), "provider = \"claude\"\ntools = "+tools+"\n\n[body]\nmodel = \"claude-test\"\nstream = true\nmax_tokens = 1024\n")
}

// signToolsPair is agentPair with alice's @auditor granted tools, two Keys
// minted, of 0x33 and 0x44, and a Sign Request alice proposed in each
// Key's Conversation. It returns the first Key's Conversation ID, and the
// msg_id of its request and of the other Key's.
func signToolsPair(t *testing.T, tools string) (a *node, provider *fakeProvider, conv []byte, mine, theirs string) {
	t.Helper()
	a, b, gid, provider := agentPair(t, "auditor")
	grantTools(t, a, "auditor", tools)
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, readFile(t, path)+"\n[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\n")
	eth := json.RawMessage(`{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000","nonce":"0","gasLimit":"21000","maxFeePerGas":"30","maxPriorityFeePerGas":"1"}`)
	var ids []string
	for _, fill := range []byte{0x33, 0x44} {
		waitWorkspace(t, a, mintKeyOf(t, a, b, gid, fill))
		spec := signreq.Spec{Reason: fmt.Sprintf("pay invoice %x", fill), Transactions: []signreq.TxSpec{{Network: "eth", Ethereum: eth}}}
		v, err := a.client().SignPropose(context.Background(), "ecdsa:"+hexOf(bytes.Repeat([]byte{fill}, 33)), spec)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, v.MsgID)
	}
	id := key.ConversationID("ecdsa", bytes.Repeat([]byte{0x33}, 33))
	return a, provider, id[:], ids[0], ids[1]
}

// lastTurn is the last message a provider call carried.
func lastTurn(c providerCall) string {
	lines := strings.Split(strings.TrimSpace(c.Turns), "\n")
	return lines[len(lines)-1]
}

// In a Key's Conversation, sign_list returns that Key's Sign Requests
// alone, as sign list --json gives them with this Node's stance, and
// sign_inspect matches this Key's alone, so another Key's is refused as
// unknown. The preamble lists the Tools and says
// a reason is another party's words (ADR 0029).
func TestSignToolsAreScopedToTheConversationsKey(t *testing.T) {
	a, provider, conv, mine, theirs := signToolsPair(t, `["say", "sign_list", "sign_inspect"]`)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		switch n {
		case 1:
			endCall(w, "sign_list", `{}`)
		case 2:
			endCall(w, "sign_inspect", `{"msg_id":"`+theirs+`"}`)
		case 3:
			endCall(w, "sign_inspect", `{"msg_id":"`+mine[:8]+`"}`)
		default:
			endSay(w, "one open request")
		}
	})
	ctx := context.Background()
	s, err := a.client().ConversationStream(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := a.client().ConversationSend(ctx, "@auditor what is open?", conv)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); !strings.HasSuffix(got, "commit:one open request end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if len(calls) != 4 {
		t.Fatalf("%d calls, want 4", len(calls))
	}
	if calls[0].Tools != "say sign_list sign_inspect" {
		t.Errorf("tools offered %q", calls[0].Tools)
	}
	for _, want := range []string{"sign_list", "sign_inspect", "another party's words"} {
		if !strings.Contains(calls[0].Stable, want) {
			t.Errorf("preamble lacks %q: %s", want, calls[0].Stable)
		}
	}
	listed := lastTurn(calls[1])
	if !strings.Contains(listed, mine) || strings.Contains(listed, theirs) || !strings.Contains(listed, `\"stance\":\"accepted\"`) ||
		!strings.Contains(listed, "pay invoice 33") || strings.Contains(listed, `"is_error":true`) {
		t.Errorf("sign_list answered %s", listed)
	}
	if refused := lastTurn(calls[2]); !strings.Contains(refused, "no Sign Request of this Conversation's Key") || !strings.Contains(refused, `"is_error":true`) || strings.Contains(refused, "pay invoice 44") {
		t.Errorf("sign_inspect of another Key's answered %s", refused)
	}
	if inspected := lastTurn(calls[3]); !strings.Contains(inspected, mine) || !strings.Contains(inspected, "ethereum chain 1") || strings.Contains(inspected, `"is_error":true`) {
		t.Errorf("sign_inspect answered %s", inspected)
	}
}

// In Group-level talk the sign Tools are not offered, even granted, nor
// named in the preamble; a call of one is a tool error.
func TestSignToolsAreNotOfferedInGroupLevelTalk(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	grantTools(t, a, "auditor", `["say", "sign_list", "sign_inspect"]`)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == 1 {
			endCall(w, "sign_list", `{}`)
			return
		}
		endSay(w, "nothing to sign here")
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor what is open?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); !strings.HasSuffix(got, "commit:nothing to sign here end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if calls[0].Tools != "say" || strings.Contains(calls[0].Stable, "sign_list") {
		t.Errorf("tools offered %q; preamble %s", calls[0].Tools, calls[0].Stable)
	}
	if got := lastTurn(calls[1]); !strings.Contains(got, `no tool named \"sign_list\"`) {
		t.Errorf("sign_list answered %s", got)
	}
}

// A profile whose tools lack say never posts: it is offered no say, a say
// it calls is refused, and the run ends silent with nothing committed.
func TestAProfileWithoutSayNeverCommitsTalk(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	grantTools(t, a, "auditor", `["sign_list"]`)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			streamSay(w, "let me speak")
			return
		}
		streamText(w, "fine")
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := summary(untilEnd(t, s, v.MsgID))
	if !strings.HasSuffix(got, "silent:@auditor end") || strings.Contains(got, "commit:") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if len(calls) != 2 || calls[0].Tools != "" || !strings.Contains(calls[0].Stable, "no say") {
		t.Fatalf("calls = %+v", calls)
	}
	if got := lastTurn(calls[1]); !strings.Contains(got, `no tool named \"say\"`) {
		t.Errorf("say answered %s", got)
	}
}
