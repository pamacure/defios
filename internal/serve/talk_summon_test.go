package serve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"defios.com/internal/group"
	"defios.com/internal/serve"
	transcripts "defios.com/internal/transcript"
)

// callSummon ends a begun stream with a call of talk_summon for handle.
func callSummon(w http.ResponseWriter, handle string) {
	input, _ := json.Marshal(map[string]string{"handle": handle})
	streamParts(w)
	endCall(w, "talk_summon", string(input))
}

// scriptAs makes provider answer each agent its system prompt names with
// its replies in turn, one per model call, across all its runs; an agent
// out of replies says "reply <n>".
func scriptAs(provider *fakeProvider, replies map[string][]func(http.ResponseWriter)) {
	var mu sync.Mutex
	next := map[string]int{}
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		sys := provider.seen()[n-1].System
		for h, rs := range replies {
			if !briefed(sys, h) {
				continue
			}
			mu.Lock()
			i := next[h]
			next[h]++
			mu.Unlock()
			if i < len(rs) {
				rs[i](w)
				return
			}
		}
		streamSay(w, fmt.Sprint("reply ", n))
	})
}

// say is a model call that says text.
func say(text string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { streamSay(w, text) }
}

// summonOf is a model call of talk_summon for handle.
func summonOf(handle string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { callSummon(w, handle) }
}

// silent is a model call that ends the run without talk_write.
func silent(w http.ResponseWriter) { streamText(w, "nothing to add") }

// An agent granted talk_summon summons another with it: the call is
// accepted at once, the run goes on to say its message, and the summoned
// agent answers after it at the next depth, framed as what summoned it
// (ADR 0041).
func TestAnAgentSummonsWithTalkSummon(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "quant")
	maxDepth2(t, a)
	grantTools(t, a, "auditor", `["talk_write", "talk_summon"]`)
	scriptAs(provider, map[string][]func(http.ResponseWriter){
		"auditor": {summonOf("quant"), say("quant will check")},
		"quant":   {say("checked")},
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := untilEnd(t, s, v.MsgID)
	want := `chain.start start:@auditor/1 tool:talk_summon {"handle":"quant"} summoned:@quant ` +
		"tool:\n" + `talk_write {"text":"quant will check"} commit:quant will check ` +
		`start:@quant/2 tool:talk_write {"text":"checked"} commit:checked end`
	if got := summary(events); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	for _, ev := range events {
		if ev.Type == serve.EventAgentSummoned && (ev.Handle != "quant" || ev.By != "auditor") {
			t.Fatalf("agent.summoned = %+v", ev)
		}
	}
	calls := provider.seen()
	if len(calls) != 3 {
		t.Fatalf("calls = %d", len(calls))
	}
	if !strings.Contains(calls[1].Turns, "@quant will answer after your run ends.") {
		t.Fatalf("talk_summon's result:\n%s", calls[1].Turns)
	}
	if want := fmt.Sprintf("(@auditor summoned you on account of:)\n[%d] alice/@auditor: quant will check", v.Seq+1); calls[2].Prompt != want {
		t.Fatalf("quant's prompt = %q\nwant %q", calls[2].Prompt, want)
	}
}

// An agent's @handle in what it says is only text: it summons nobody, and
// nothing is skipped (ADR 0041).
func TestAnAgentsTextMentionsNobody(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "scribe")
	maxDepth2(t, a)
	replyAs(provider, map[string]string{"auditor": "ask @scribe"})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); got != `chain.start start:@auditor/1 tool:talk_write {"text":"ask @scribe"} commit:ask @scribe end` {
		t.Fatalf("events = %s", got)
	}
	if n := len(provider.seen()); n != 1 {
		t.Fatalf("provider called %d times, want 1", n)
	}
}

// talk_summon refuses, with a tool error naming whom the agent may summon,
// its own Handle, a person, a Handle not included, and one already
// summoned this run; what it accepted still runs, once (ADR 0041).
func TestTalkSummonRefusesWhatCannotBeSummoned(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "quant", "scribe")
	maxDepth2(t, a)
	grantTools(t, a, "auditor", `["talk_write", "talk_summon"]`)
	scriptAs(provider, map[string][]func(http.ResponseWriter){
		"auditor": {summonOf("auditor"), summonOf("user"), summonOf("ghost"), summonOf("quant"), summonOf("quant"), silent},
		"quant":   {say("checked")},
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); !strings.HasSuffix(got, `silent:@auditor start:@quant/2 tool:talk_write {"text":"checked"} commit:checked end`) ||
		strings.Count(got, "summoned:") != 1 {
		t.Fatalf("events = %s", got)
	}
	turns := provider.seen()[5].Turns
	for _, want := range []string{
		"@auditor is you. You may summon: @quant, @scribe.",
		"@user is a person, not an agent. You may summon: @quant, @scribe.",
		"@ghost is not an agent included here. You may summon: @quant, @scribe.",
		"@quant will answer after your run ends.",
		"@quant is already summoned in this run. You may summon: @scribe.",
	} {
		if !strings.Contains(turns, want) {
			t.Errorf("results lack %q:\n%s", want, turns)
		}
	}
}

// At the max depth talk_summon is still offered, and refuses (ADR 0041).
func TestTalkSummonRefusesAtTheMaxDepth(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "quant")
	grantTools(t, a, "auditor", `["talk_write", "talk_summon"]`)
	scriptAs(provider, map[string][]func(http.ResponseWriter){
		"auditor": {summonOf("quant"), say("done")},
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); strings.Contains(got, "quant/") || strings.Contains(got, "summoned:") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if !strings.Contains(calls[0].Tools, "talk_summon") {
		t.Fatalf("tools = %s", calls[0].Tools)
	}
	if !strings.Contains(calls[1].Turns, "you are at depth 1, the max depth: you may summon nobody") {
		t.Fatalf("result:\n%s", calls[1].Turns)
	}
}

// A silent agent's summons still run, and the summoned agent is shown the
// nearest message up the Chain, here the person's, framed as what its
// summoner summoned it on account of. The summon is in the summoner's
// Transcript (ADR 0041).
func TestASilentRoutersSummonsRun(t *testing.T) {
	a, _, _, provider := agentPair(t, "router", "quant")
	maxDepth2(t, a)
	grantTools(t, a, "router", `["talk_summon"]`)
	scriptAs(provider, map[string][]func(http.ResponseWriter){
		"router": {summonOf("quant"), silent},
		"quant":  {say("on it")},
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@router price this", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); got != `chain.start start:@router/1 tool:talk_summon {"handle":"quant"} summoned:@quant text:nothing to add silent:@router `+
		`start:@quant/2 tool:talk_write {"text":"on it"} commit:on it end` {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if want := fmt.Sprintf("(@router summoned you on account of:)\n[%d] alice: @router price this", v.Seq); calls[2].Prompt != want {
		t.Fatalf("quant's prompt = %q\nwant %q", calls[2].Prompt, want)
	}
	ts, err := transcripts.ReadConversation(a.paths, group.ImplicitAlias, nil)
	if err != nil {
		t.Fatal(err)
	}
	var summoned []string
	for _, tr := range ts {
		for _, r := range transcripts.Runs(tr.Lines) {
			if r.Handle == "router" {
				summoned = r.Summoned
			}
		}
	}
	if len(summoned) != 1 || summoned[0] != "quant" {
		t.Fatalf("router's run summoned %q", summoned)
	}
}

// The stable facts explain talk_summon to an agent granted it, and say
// nothing about @ in what an agent writes (ADR 0041).
func TestTheFactsExplainTalkSummon(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "quant")
	grantTools(t, a, "auditor", `["talk_write", "talk_summon"]`)
	grantTools(t, a, "quant", `["talk_write"]`)
	summon(t, a, "@auditor @quant")
	calls := provider.seen()
	if !strings.Contains(calls[0].System, "call talk_summon") || strings.Contains(calls[1].System, "call talk_summon") {
		t.Fatalf("systems:\n%s\n---\n%s", calls[0].System, calls[1].System)
	}
	for _, c := range calls {
		if strings.Contains(c.System, `\@`) || strings.Contains(c.System, "without the `@`") {
			t.Fatalf("system advises on @:\n%s", c.System)
		}
	}
}
