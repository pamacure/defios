package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/serve"
	"defios.com/internal/session"
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
		v, err := a.client().SignPropose(context.Background(), "ecdsa:"+hexOf(bytes.Repeat([]byte{fill}, 33)), spec, "")
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
// unknown, as are sign_accept and sign_refuse. The system block lists the
// Tools and says a reason is another party's words (ADR 0029).
func TestSignToolsAreScopedToTheConversationsKey(t *testing.T) {
	a, provider, conv, mine, theirs := signToolsPair(t, `["talk_write", "sign_list", "sign_inspect", "sign_accept", "sign_refuse"]`)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		switch n {
		case 1:
			endCall(w, "sign_list", `{}`)
		case 2:
			endCall(w, "sign_inspect", `{"msg_id":"`+theirs+`"}`)
		case 3:
			endCall(w, "sign_inspect", `{"msg_id":"`+mine[:8]+`"}`)
		case 4:
			endCall(w, "sign_accept", `{"msg_id":"`+theirs+`"}`)
		case 5:
			endCall(w, "sign_refuse", `{"msg_id":"`+theirs+`","reason":"no"}`)
		default:
			endTalkWrite(w, "one open request")
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
	if len(calls) != 6 {
		t.Fatalf("%d calls, want 6", len(calls))
	}
	if calls[0].Tools != "talk_write sign_list sign_inspect sign_accept sign_refuse" {
		t.Errorf("tools offered %q", calls[0].Tools)
	}
	for _, want := range []string{"sign_list", "sign_inspect", "another party's words"} {
		if !strings.Contains(calls[0].System, want) {
			t.Errorf("system block lacks %q: %s", want, calls[0].System)
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
	for i, tool := range []string{"sign_accept", "sign_refuse"} {
		if got := lastTurn(calls[4+i]); !strings.Contains(got, "no-sign-request: no Sign Request of this Conversation's Key") || !strings.Contains(got, `"is_error":true`) {
			t.Errorf("%s of another Key's answered %s", tool, got)
		}
	}
}

// In Group-level talk the sign Tools are not offered, even granted, nor
// named in the system block; a call of one is a tool error.
func TestSignToolsAreNotOfferedInGroupLevelTalk(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	grantTools(t, a, "auditor", `["talk_write", "sign_list", "sign_inspect", "sign_propose", "sign_accept", "sign_refuse", "chain_fill"]`)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		switch n {
		case 1:
			endCall(w, "sign_list", `{}`)
		case 2:
			endCall(w, "sign_propose", `{"reason":"pay","transactions":`+payEthTool+`}`)
		default:
			endTalkWrite(w, "nothing to sign here")
		}
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
	if calls[0].Tools != "talk_write" || strings.Contains(calls[0].System, "sign_") || strings.Contains(calls[0].System, "chain_fill") {
		t.Errorf("tools offered %q; system block %s", calls[0].Tools, calls[0].System)
	}
	if got := lastTurn(calls[1]); !strings.Contains(got, `no tool named \"sign_list\"`) {
		t.Errorf("sign_list answered %s", got)
	}
	if got := lastTurn(calls[2]); !strings.Contains(got, `no tool named \"sign_propose\"`) {
		t.Errorf("sign_propose answered %s", got)
	}
}

// A profile whose tools lack talk_write never posts: it is offered no talk_write, a talk_write
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
	if len(calls) != 2 || calls[0].Tools != "" || !strings.Contains(calls[0].System, "no talk_write") {
		t.Fatalf("calls = %+v", calls)
	}
	if got := lastTurn(calls[1]); !strings.Contains(got, `no tool named \"talk_write\"`) {
		t.Errorf("talk_write answered %s", got)
	}
}

// signingAgents is a valid Group of alice and bob, each running an agent on
// the one fake Messages API, alice's @auditor and bob's @reviewer, both
// granted every Tool, with a 2-of-2 Key of 0x33 minted and a Network for
// Ethereum chain 1 on each. Stances are kept but no signing Session
// starts. It returns the Key's Conversation ID.
func signingAgents(t *testing.T) (a, b *node, provider *fakeProvider, conv []byte) {
	t.Helper()
	a, b = pairWith(t, func(n *node) {
		n.tune = func(o *serve.Options) {
			o.AgentBackoff = time.Millisecond
			o.NewEngine = func() session.Engine { return session.Unavailable{} }
		}
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	provider = newFakeProvider(t)
	mintKey(t, a, b, gid)
	id := key.ConversationID("ecdsa", bytes.Repeat([]byte{0x33}, 33))
	for n, h := range map[*node]string{a: "auditor", b: "reviewer"} {
		agentNode(t, n, provider.URL, h)
		grantTools(t, n, h, `["talk_write", "sign_list", "sign_inspect", "sign_propose", "sign_accept", "sign_refuse"]`)
		path := n.paths.GroupConfigFile(group.ImplicitAlias)
		writeConfig(t, path, readFile(t, path)+"\n[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\n")
		waitMinted(t, n, gid, id[:])
	}
	return a, b, provider, id[:]
}

// summonIn sends text to conv on n and waits for the chain it starts to end,
// returning its events as summary gives them.
func summonIn(t *testing.T, n *node, conv []byte, text string) (chain, events string) {
	t.Helper()
	ctx := context.Background()
	s, err := n.client().ConversationStream(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := n.client().ConversationSend(ctx, text, conv)
	if err != nil {
		t.Fatal(err)
	}
	return v.MsgID, summary(untilEnd(t, s, v.MsgID))
}

// actedWords is the Transcript's acted lines as "<tool> <msg_id>" words.
func actedWords(lines []map[string]any) []string {
	var out []string
	for _, l := range lines {
		if l["kind"] == serve.KindActed {
			out = append(out, fmt.Sprint(l["tool"], " ", l["msg_id"]))
		}
	}
	return out
}

const payEthTool = `[{"network":"eth","ethereum":{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000","nonce":"0","gasLimit":"21000","maxFeePerGas":"30","maxPriorityFeePerGas":"1"}}]`

// sign_list with a status returns only the Key's Sign Requests in that
// status, leaving out those in another, and an empty list if none is in
// it; with none it returns them all, and an
// unknown status or argument is a tool error naming the valid ones
// (ADR 0036).
func TestSignListFiltersByStatus(t *testing.T) {
	a, b, provider, conv := signingAgents(t)
	ctx := context.Background()
	ref := "ecdsa:" + hexOf(bytes.Repeat([]byte{0x33}, 33))
	open, err := a.client().SignPropose(ctx, ref, signreq.Spec{Reason: "pay one", Transactions: payEth}, "")
	if err != nil {
		t.Fatal(err)
	}
	refused, err := b.client().SignPropose(ctx, ref, signreq.Spec{Reason: "pay two", Transactions: payEth}, "")
	if err != nil {
		t.Fatal(err)
	}
	waitSign(t, a, refused.MsgID, func(signreq.Detail) bool { return true })
	if _, err := a.client().SignRefuse(ctx, refused.MsgID, "no"); err != nil {
		t.Fatal(err)
	}
	waitSign(t, a, refused.MsgID, func(d signreq.Detail) bool { return d.Status == signreq.Refused })
	statuses := strings.Join(signreq.Statuses, ", ")
	cases := []struct {
		input string
		fails bool
		want  []string // each in the answer
		lacks string   // not in it
	}{
		{input: `{"status":"open"}`, want: []string{open.MsgID}, lacks: refused.MsgID},
		{input: `{"status":"refused"}`, want: []string{refused.MsgID}, lacks: open.MsgID},
		{input: `{"status":"expired"}`, want: []string{`"[]"`}},
		{input: `{}`, want: []string{open.MsgID, refused.MsgID}},
		{input: `{"status":"pending"}`, fails: true, want: []string{"pending", statuses}},
		{input: `{"status":""}`, fails: true, want: []string{statuses}},
		{input: `{"state":"open"}`, fails: true, want: []string{statuses}},
	}
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n <= len(cases) {
			endCall(w, "sign_list", cases[n-1].input)
			return
		}
		endTalkWrite(w, "listed")
	})
	if _, got := summonIn(t, a, conv, "@auditor what is open?"); !strings.HasSuffix(got, "commit:listed end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seenWith("sk-alice")
	if len(calls) != len(cases)+1 {
		t.Fatalf("%d calls, want %d", len(calls), len(cases)+1)
	}
	for i, c := range cases {
		got := lastTurn(calls[i+1])
		if strings.Contains(got, `"is_error":true`) != c.fails {
			t.Errorf("sign_list %s answered %s", c.input, got)
		}
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("sign_list %s lacks %q: %s", c.input, want, got)
			}
		}
		if c.lacks != "" && strings.Contains(got, c.lacks) {
			t.Errorf("sign_list %s has %q: %s", c.input, c.lacks, got)
		}
	}
}

// An agent proposes a Sign Request of its Conversation's Key under its
// Handle, and its proposal is its Node's accept. Another Node's agent
// refuses it with a reason; that Node's stance is then final, so its
// agent's accept is the tool error already-refused, and the run goes on to
// talk_write. Each successful sign Tool is an acted line in the Transcript.
func TestAgentsProposeAndRefuseASignRequest(t *testing.T) {
	a, b, provider, conv := signingAgents(t)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == 1 {
			endCall(w, "sign_propose", `{"reason":"pay invoice 7","transactions":`+payEthTool+`}`)
			return
		}
		endTalkWrite(w, "proposed")
	})
	chain, got := summonIn(t, a, conv, "@auditor pay invoice 7")
	if !strings.HasSuffix(got, "commit:proposed end") {
		t.Fatalf("alice's events = %s", got)
	}
	calls := provider.seenWith("sk-alice")
	if calls[0].Tools != "talk_write sign_list sign_inspect sign_propose sign_accept sign_refuse" {
		t.Errorf("tools offered %q", calls[0].Tools)
	}
	if !strings.Contains(calls[0].System, "acts as this Node") {
		t.Errorf("system block lacks that a sign Tool acts as this Node: %s", calls[0].System)
	}
	proposed := lastTurn(calls[1])
	if strings.Contains(proposed, `"is_error":true`) || !strings.Contains(proposed, `\"seq\":`) {
		t.Fatalf("sign_propose answered %s", proposed)
	}
	list, err := a.client().SignList(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	msgID := list[0].MsgID
	if !strings.Contains(proposed, msgID) {
		t.Errorf("sign_propose answered %s, want msg_id %s", proposed, msgID)
	}
	if d := waitSign(t, b, msgID, func(signreq.Detail) bool { return true }); d.ProposerHandle != "auditor" || d.Reason != "pay invoice 7" {
		t.Fatalf("bob sees %+v", d.Summary)
	}
	if got := actedWords(convTranscript(t, a, conv, chain)); len(got) != 1 || got[0] != "sign_propose "+msgID {
		t.Errorf("alice's acted lines = %q", got)
	}

	provider.set(func(n int, w http.ResponseWriter, r *http.Request) {
		streamParts(w)
		switch len(provider.seenWith("sk-bob")) {
		case 1:
			endCall(w, "sign_refuse", `{"msg_id":"`+msgID[:8]+`","reason":"unknown payee"}`)
		case 2:
			endCall(w, "sign_accept", `{"msg_id":"`+msgID+`"}`)
		default:
			endTalkWrite(w, "refused")
		}
	})
	chain, got = summonIn(t, b, conv, "@reviewer check it")
	if !strings.HasSuffix(got, "commit:refused end") {
		t.Fatalf("bob's events = %s", got)
	}
	calls = provider.seenWith("sk-bob")
	if len(calls) != 3 {
		t.Fatalf("%d calls, want 3", len(calls))
	}
	if refused := lastTurn(calls[1]); strings.Contains(refused, `"is_error":true`) {
		t.Errorf("sign_refuse answered %s", refused)
	}
	if again := lastTurn(calls[2]); !strings.Contains(again, "already-refused") || !strings.Contains(again, `"is_error":true`) {
		t.Errorf("sign_accept after refusing answered %s", again)
	}
	d := waitSign(t, a, msgID, func(d signreq.Detail) bool { return len(d.Refusals) == 1 })
	if r := d.Refusals[0]; r.Handle != "reviewer" || r.Reason != "unknown payee" || d.Status != signreq.Refused {
		t.Errorf("alice sees %+v", d.Summary)
	}
	if got := actedWords(convTranscript(t, b, conv, chain)); len(got) != 1 || got[0] != "sign_refuse "+msgID {
		t.Errorf("bob's acted lines = %q", got)
	}
}

// With no leader, an agent's proposal waits in its Node's Outbox: the
// model is told it is queued, with its msg_id, and the run goes on to post with talk_write.
func TestAQueuedProposalIsQueuedToTheModel(t *testing.T) {
	a, b, provider, conv := signingAgents(t)
	b.stop()
	a.waitLeaderless(t)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == 1 {
			endCall(w, "sign_propose", `{"reason":"pay invoice 8","transactions":`+payEthTool+`}`)
			return
		}
		endTalkWrite(w, "proposed, queued")
	})
	chain, got := summonIn(t, a, conv, "@auditor pay invoice 8")
	if !strings.HasSuffix(got, "commit:proposed, queued end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	proposed := lastTurn(calls[1])
	if !strings.Contains(proposed, `\"queued\":true`) || strings.Contains(proposed, `\"seq\":`) || strings.Contains(proposed, `"is_error":true`) {
		t.Fatalf("sign_propose answered %s", proposed)
	}
	if got := actedWords(convTranscript(t, a, conv, chain)); len(got) != 1 || !strings.HasPrefix(got[0], "sign_propose ") || !strings.Contains(proposed, strings.TrimPrefix(got[0], "sign_propose ")) {
		t.Errorf("acted lines = %q; sign_propose answered %s", got, proposed)
	}
}

// An agent's accept of a person's Sign Request is its Node's, under its
// Handle: the model is told what sign accept --json gives, and the accept
// is an acted line in the Transcript.
func TestAnAgentAcceptsASignRequest(t *testing.T) {
	a, b, provider, conv := signingAgents(t)
	pk := "ecdsa:" + hexOf(bytes.Repeat([]byte{0x33}, 33))
	v, err := a.client().SignPropose(context.Background(), pk, signreq.Spec{Reason: "pay", Transactions: payEth}, "")
	if err != nil {
		t.Fatal(err)
	}
	waitSign(t, b, v.MsgID, func(signreq.Detail) bool { return true })
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == 1 {
			endCall(w, "sign_accept", `{"msg_id":"`+v.MsgID[:8]+`"}`)
			return
		}
		endTalkWrite(w, "accepted")
	})
	chain, got := summonIn(t, b, conv, "@reviewer accept it")
	if !strings.HasSuffix(got, "commit:accepted end") {
		t.Fatalf("events = %s", got)
	}
	if accepted := lastTurn(provider.seen()[1]); !strings.Contains(accepted, `"content":"{}"`) {
		t.Errorf("sign_accept answered %s", accepted)
	}
	d := waitSign(t, a, v.MsgID, func(d signreq.Detail) bool { return len(d.Accepts) == 2 })
	if d.Accepts[1].Node != b.id.NodeID.String() || d.Accepts[1].Handle != "reviewer" {
		t.Errorf("accepts = %+v", d.Accepts)
	}
	if got := actedWords(convTranscript(t, b, conv, chain)); len(got) != 1 || got[0] != "sign_accept "+v.MsgID {
		t.Errorf("acted lines = %q", got)
	}
}
