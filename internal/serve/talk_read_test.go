package serve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// talk_read is granted with talk_write unless a profile says otherwise
// (ADR 0036). An agent calling it reads the latest talk as JSON Lines,
// each message with its seq and when it was written (ADR 0039); last bounds the page, which then says how to page back, and
// before_seq pages back; an input of any other shape is a tool error. Its
// description names the agent's own lines as the talk shows them.
func TestAnAgentReadsTheTalkWithTalkRead(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	ctx := context.Background()
	var seqs []uint64
	for _, text := range []string{"first", "second\nline"} {
		v, err := a.client().ConversationSend(ctx, text, nil)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, v.Seq)
	}
	var before string
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		switch n {
		case 1:
			endCall(w, "talk_read", `{}`)
		case 2:
			endCall(w, "talk_read", `{"last":1}`)
		case 3:
			endCall(w, "talk_read", `{"last":1,"before_seq":`+before+`}`)
		case 4:
			endCall(w, "talk_read", `{"last":"many"}`)
		case 5:
			endCall(w, "talk_read", `{"before":3}`)
		default:
			endTalkWrite(w, "read it")
		}
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(ctx, "@auditor read the talk", nil)
	if err != nil {
		t.Fatal(err)
	}
	before = fmt.Sprint(v.Seq)
	if got := summary(untilEnd(t, s, v.MsgID)); !strings.HasSuffix(got, "commit:read it end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if len(calls) != 6 || calls[0].Tools != "talk_write talk_read" {
		t.Fatalf("calls = %+v", calls)
	}
	if got, want := calls[0].Descriptions["talk_read"], `Messages with "sender_display_name":"alice" and "handle":"auditor" are your own talk_write messages`; !strings.Contains(got, want) {
		t.Errorf("description %q lacks %q", got, want)
	}
	summon := fmt.Sprintf(`%d alice: @auditor read the talk`, v.Seq)
	second := fmt.Sprintf("%d alice: second\nline", seqs[1])
	if got, want := talkRead(t, calls[1]), fmt.Sprintf("%d alice: first | %s | %s", seqs[0], second, summon); got != want {
		t.Errorf("talk_read answered %s, want %s", got, want)
	}
	if got, want := talkRead(t, calls[2]), fmt.Sprintf("%s | earlier %d", summon, seqs[1]+1); got != want {
		t.Errorf("last 1 answered %s, want %s", got, want)
	}
	if got, want := talkRead(t, calls[3]), fmt.Sprintf("%s | earlier %d", second, seqs[0]+1); got != want {
		t.Errorf("before_seq answered %s, want %s", got, want)
	}
	for _, c := range calls[4:] {
		if got := lastTurn(c); !strings.Contains(got, `"is_error":true`) || !strings.Contains(got, "its input must be") {
			t.Errorf("a malformed talk_read answered %s", got)
		}
	}
}

// talkRead is the talk_read result c's last turn answers, each JSON line
// as "<seq> <display name>: <text>", or "earlier <seq>" for the line that
// says how to page back, joined by " | ". A message line must say when it
// was written.
func talkRead(t *testing.T, c providerCall) string {
	t.Helper()
	var blocks []struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lastTurn(c), "[user] ")), &blocks); err != nil || len(blocks) != 1 {
		t.Fatalf("last turn %s: %v", lastTurn(c), err)
	}
	var out []string
	for _, l := range strings.Split(blocks[0].Content, "\n") {
		var e struct {
			Seq     uint64 `json:"seq"`
			At      string `json:"created_at"`
			Sender  string `json:"sender_display_name"`
			Text    string `json:"text"`
			Earlier uint64 `json:"earlier_before_seq"`
		}
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line %q is not JSON: %v", l, err)
		}
		if e.Earlier > 0 {
			out = append(out, fmt.Sprint("earlier ", e.Earlier))
			continue
		}
		if _, err := time.Parse(time.RFC3339, e.At); err != nil {
			t.Errorf("line %q: created_at: %v", l, err)
		}
		out = append(out, fmt.Sprintf("%d %s: %s", e.Seq, e.Sender, e.Text))
	}
	return strings.Join(out, " | ")
}

// A profile whose tools lack talk_read is not offered it, and a call of it
// is a tool error.
func TestAProfileCanWithholdTalkRead(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	grantTools(t, a, "auditor", `["talk_write"]`)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == 1 {
			endCall(w, "talk_read", `{}`)
			return
		}
		endTalkWrite(w, "could not read")
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor read the talk", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); !strings.HasSuffix(got, "commit:could not read end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if calls[0].Tools != "talk_write" {
		t.Errorf("tools offered %q", calls[0].Tools)
	}
	if got := lastTurn(calls[1]); !strings.Contains(got, `no tool named \"talk_read\"`) {
		t.Errorf("talk_read answered %s", got)
	}
}
