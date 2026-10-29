package serve_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/serve"
)

// answerAs makes provider answer as each agent its system prompt names:
// with a 400 while failing[handle] is set, or else by saying
// replies[handle].
func answerAs(provider *fakeProvider, replies map[string]string, failing map[string]*atomic.Bool) {
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		sys := provider.seen()[n-1].System
		for h, text := range replies {
			if !strings.HasPrefix(sys, "You are "+h+".") {
				continue
			}
			if f := failing[h]; f != nil && f.Load() {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"no such model"}`)
				return
			}
			streamSay(w, text)
			return
		}
		w.WriteHeader(400)
	})
}

// maxDepth2 lets a's agents summon one level down.
func maxDepth2(t *testing.T, a *node) {
	t.Helper()
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, strings.Replace(readFile(t, path), "[agent]\n", "[agent]\nmax_depth = 2\n", 1))
}

// retry retries the Group-level chain seq on n (0: the latest unfinished)
// and returns its chain's events.
func retry(t *testing.T, n *node, s *serve.ChainStream, seq uint64) []serve.ChainEvent {
	t.Helper()
	v, err := n.client().ConversationRetry(context.Background(), nil, seq)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if seq != 0 && v.Chain != seq {
		t.Fatalf("retried chain %d, want %d", v.Chain, seq)
	}
	return untilEnd(t, s, v.Chain)
}

// @auditor @scribe, where auditor summons quant and scribe fails: a retry
// reuses auditor's and quant's messages, runs scribe again as attempt 2 of
// step 3, and appends nothing itself, so auditor's message is posted once.
func TestRetryRerunsOnlyTheFailedStep(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor", "scribe", "quant")
	maxDepth2(t, a)
	var fail atomic.Bool
	fail.Store(true)
	answerAs(provider, map[string]string{"auditor": "ask @quant", "quant": "noted", "scribe": "done"}, map[string]*atomic.Bool{"scribe": &fail})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.Seq)); got != "chain.start start:@auditor/1 tool:say {\"text\":\"ask @quant\"} commit:ask @quant start:@quant/2 tool:say {\"text\":\"noted\"} commit:noted start:@scribe/1 error:@scribe end" {
		t.Fatalf("first run = %s", got)
	}
	fail.Store(false)
	if got := summary(retry(t, a, s, 0)); got != "chain.start skipped:@auditor skipped:@quant start:@scribe/1 tool:say {\"text\":\"done\"} commit:done end" {
		t.Fatalf("retry = %s", got)
	}
	if n := len(provider.seen()); n != 4 {
		t.Fatalf("provider called %d times, want 4", n)
	}
	var texts []string
	for _, ff := range freeforms(t, b.groupLevelRows(t, gid, 4)) {
		texts = append(texts, ff.Text)
	}
	if got := strings.Join(texts, "|"); got != "@auditor @scribe|ask @quant|noted|done" {
		t.Fatalf("talk = %s", got)
	}
	words := lineWords(transcript(t, a, v.Seq))
	if !strings.HasSuffix(words, "3/scribe/1/1:outcome=error 3/scribe/1/2:request 3/scribe/1/2:block 3/scribe/1/2:block 3/scribe/1/2:usage 3/scribe/1/2:outcome=said") {
		t.Fatalf("transcript = %s", words)
	}
	if _, err := a.client().ConversationRetry(context.Background(), nil, 0); !errors.Is(err, serve.ErrNothingToRetry) {
		t.Fatalf("retry after it = %v, want ErrNothingToRetry", err)
	}
	time.Sleep(200 * time.Millisecond)
	if rows := a.groupLevelRows(t, gid, 4); len(rows) != 4 {
		t.Fatalf("%d rows after nothing to retry, want 4", len(rows))
	}
}

// A Handle at two steps of one chain, as auditor and scribe both summon
// quant: only the step that failed runs again.
func TestRetryRerunsOnlyTheFailedStepOfARepeatedHandle(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "scribe", "quant")
	maxDepth2(t, a)
	var calls atomic.Int32
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		sys := provider.seen()[n-1].System
		switch {
		case strings.HasPrefix(sys, "You are quant."):
			if calls.Add(1) == 2 {
				w.WriteHeader(400)
				return
			}
			streamSay(w, "noted")
		default:
			streamSay(w, "ask @quant")
		}
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.Seq)); got != "chain.start start:@auditor/1 tool:say {\"text\":\"ask @quant\"} commit:ask @quant start:@quant/2 tool:say {\"text\":\"noted\"} commit:noted start:@scribe/1 tool:say {\"text\":\"ask @quant\"} commit:ask @quant start:@quant/2 error:@quant end" {
		t.Fatalf("first run = %s", got)
	}
	if got := summary(retry(t, a, s, v.Seq)); got != "chain.start skipped:@auditor skipped:@quant skipped:@scribe start:@quant/2 tool:say {\"text\":\"noted\"} commit:noted end" {
		t.Fatalf("retry = %s", got)
	}
	if n := len(provider.seen()); n != 5 {
		t.Fatalf("provider called %d times, want 5", n)
	}
	if words := lineWords(transcript(t, a, v.Seq)); !strings.HasSuffix(words, "4/quant/2/1:outcome=error 4/quant/2/2:request 4/quant/2/2:block 4/quant/2/2:block 4/quant/2/2:usage 4/quant/2/2:outcome=said") {
		t.Fatalf("transcript = %s", words)
	}
}

// A run cut off by a serve stop has no outcome; after a restart a retry
// finds it from the Transcript alone and runs it as attempt 2.
func TestRetryAfterARestartRunsTheCutOffStep(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor")
	started, _ := holdProvider(provider)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	a.restart(t)
	if got := lineWords(transcript(t, a, v.Seq)); got != "1/auditor/1/1:request" {
		t.Fatalf("transcript after the stop = %s", got)
	}
	provider.set(func(_ int, w http.ResponseWriter, _ *http.Request) { streamSay(w, "back") })
	s := watch(t, a)
	if got := summary(retry(t, a, s, 0)); got != "chain.start start:@auditor/1 tool:say {\"text\":\"back\"} commit:back end" {
		t.Fatalf("retry = %s", got)
	}
	if ffs := freeforms(t, b.groupLevelRows(t, gid, 2)); len(ffs) != 2 || ffs[1].Text != "back" {
		t.Fatalf("talk = %+v", ffs)
	}
	if got := lineWords(transcript(t, a, v.Seq)); !strings.HasPrefix(got, "1/auditor/1/1:request 1/auditor/1/2:request") || !strings.HasSuffix(got, "1/auditor/1/2:outcome=said") {
		t.Fatalf("transcript = %s", got)
	}
}

// A chain still queued, or running, cannot be retried. Once cancelled while
// queued it has no Transcript, and --chain runs it from the start. A chain
// whose steps all said has nothing to retry, and a seq that started no
// chain is refused.
func TestRetryQueuedRunningAndUntranscribedChains(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	started, release := holdProvider(provider)
	s := watch(t, a)
	ctx := context.Background()
	first, err := a.client().ConversationSend(ctx, "@auditor one", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := a.client().ConversationSend(ctx, "@auditor two", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.applied(t, second.Seq)
	time.Sleep(200 * time.Millisecond) // the follower queues it
	for _, seq := range []uint64{first.Seq, second.Seq} {
		if _, err := a.client().ConversationRetry(ctx, nil, seq); !errors.Is(err, serve.ErrChainBusy) {
			t.Fatalf("retry of %d = %v, want ErrChainBusy", seq, err)
		}
	}
	if c, err := a.client().ConversationCancel(ctx, nil, second.Seq); err != nil || !c.Cancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	close(release)
	firstEvents := untilEnd(t, s, first.Seq)
	untilEnd(t, s, second.Seq)
	if got := summary(retry(t, a, s, second.Seq)); got != "chain.start start:@auditor/1 text:start tool:say {\"text\":\"start done\"} commit:start done end" {
		t.Fatalf("retry = %s", got)
	}
	if got := lineWords(transcript(t, a, second.Seq)); !strings.HasPrefix(got, "1/auditor/1/1:request") {
		t.Fatalf("transcript = %s", got)
	}
	if _, err := a.client().ConversationRetry(ctx, nil, first.Seq); !errors.Is(err, serve.ErrNothingToRetry) {
		t.Fatalf("retry of a finished chain = %v, want ErrNothingToRetry", err)
	}
	if _, err := a.client().ConversationRetry(ctx, nil, 0); !errors.Is(err, serve.ErrNothingToRetry) {
		t.Fatalf("retry with nothing unfinished = %v, want ErrNothingToRetry", err)
	}
	reply := firstEvents[len(firstEvents)-2].Seq // agent.commit
	if _, err := a.client().ConversationRetry(ctx, nil, reply); !errors.Is(err, serve.ErrNotAChain) {
		t.Fatalf("retry of an agent's message = %v, want ErrNotAChain", err)
	}
}

// dropProfile takes handle out of a's [agent] profiles.
func dropProfile(t *testing.T, a *node, handle string) {
	t.Helper()
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	body := readFile(t, path)
	edited := strings.Replace(strings.Replace(body, `"`+handle+`", `, "", 1), `, "`+handle+`"`, "", 1)
	if edited == body {
		t.Fatalf("@%s is not in %s", handle, path)
	}
	writeConfig(t, path, edited)
}

// @auditor @quant @critic, where auditor summons scribe and critic fails:
// with scribe since dropped from the profiles, a retry still numbers quant
// step 3, so quant, which said, is not run again and posts once.
func TestRetryKeepsStepsWhenAHandleThatRanIsNowSkipped(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor", "scribe", "quant", "critic")
	maxDepth2(t, a)
	var fail atomic.Bool
	fail.Store(true)
	answerAs(provider, map[string]string{"auditor": "ask @scribe", "scribe": "ok", "quant": "noted", "critic": "fine"}, map[string]*atomic.Bool{"critic": &fail})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @quant @critic", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.Seq)); !strings.HasSuffix(got, "commit:noted start:@critic/1 error:@critic end") {
		t.Fatalf("first run = %s", got)
	}
	dropProfile(t, a, "scribe")
	fail.Store(false)
	if got := summary(retry(t, a, s, 0)); got != "chain.start skipped:@auditor skipped:@scribe skipped:@quant start:@critic/1 tool:say {\"text\":\"fine\"} commit:fine end" {
		t.Fatalf("retry = %s", got)
	}
	var texts []string
	for _, ff := range freeforms(t, b.groupLevelRows(t, gid, 5)) {
		texts = append(texts, ff.Text)
	}
	if got := strings.Join(texts, "|"); got != "@auditor @quant @critic|ask @scribe|ok|noted|fine" {
		t.Fatalf("talk = %s", got)
	}
	if words := lineWords(transcript(t, a, v.Seq)); !strings.HasSuffix(words, "4/critic/1/1:outcome=error 4/critic/1/2:request 4/critic/1/2:block 4/critic/1/2:block 4/critic/1/2:usage 4/critic/1/2:outcome=said") {
		t.Fatalf("transcript = %s", words)
	}
}

// cutAfterStep rewrites the Transcript of a's Group-level chain seq to its
// lines up to step: as if a stop came between that step and the next.
func cutAfterStep(t *testing.T, a *node, seq uint64, step int) {
	t.Helper()
	path := a.paths.TranscriptFile(group.ImplicitAlias, "group", seq)
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(readFile(t, path)), "\n") {
		var l serve.TranscriptLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		if l.Step <= step {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A chain cut off after a step said, before the next wrote anything, has
// every recorded step finished; a retry, with or without --chain, still
// finds the step it never reached and runs it.
func TestRetryRunsAStepAStopCameBefore(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "scribe")
	answerAs(provider, map[string]string{"auditor": "checked", "scribe": "written"}, nil)
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.Seq)
	for _, seq := range []uint64{0, v.Seq} {
		cutAfterStep(t, a, v.Seq, 1)
		if got := summary(retry(t, a, s, seq)); got != "chain.start skipped:@auditor start:@scribe/1 tool:say {\"text\":\"written\"} commit:written end" {
			t.Fatalf("retry %d = %s", seq, got)
		}
	}
	if _, err := a.client().ConversationRetry(context.Background(), nil, 0); !errors.Is(err, serve.ErrNothingToRetry) {
		t.Fatalf("retry after it = %v, want ErrNothingToRetry", err)
	}
}
