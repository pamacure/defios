package serve_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// talk_read is granted with talk_write unless a profile says otherwise
// (ADR 0036). An agent calling it reads the latest talk, each line under
// its seq; last bounds the page, which then says how to page back, and
// before_seq pages back; an input of any other shape is a tool error.
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
	summon := fmt.Sprintf(`[%d] alice: @auditor read the talk`, v.Seq)
	second := fmt.Sprintf(`[%d] alice: second\\nline`, seqs[1])
	if want := fmt.Sprintf(`"[%d] alice: first\n%s\n%s"`, seqs[0], second, summon); !strings.Contains(lastTurn(calls[1]), want) {
		t.Errorf("talk_read answered %s, want %s", lastTurn(calls[1]), want)
	}
	if want := fmt.Sprintf(`"%s\n(Earlier talk: call talk_read with before_seq %d.)"`, summon, seqs[1]+1); !strings.Contains(lastTurn(calls[2]), want) {
		t.Errorf("last 1 answered %s, want %s", lastTurn(calls[2]), want)
	}
	if want := fmt.Sprintf(`"%s\n(Earlier talk: call talk_read with before_seq %d.)"`, second, seqs[0]+1); !strings.Contains(lastTurn(calls[3]), want) {
		t.Errorf("before_seq answered %s, want %s", lastTurn(calls[3]), want)
	}
	for _, c := range calls[4:] {
		if got := lastTurn(c); !strings.Contains(got, `"is_error":true`) || !strings.Contains(got, "its input must be") {
			t.Errorf("a malformed talk_read answered %s", got)
		}
	}
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
