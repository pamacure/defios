package serve_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/serve"
)

// hookConfig appends body, Jobs and Hooks, to n's Group config.toml, and
// waits until n's serve has taken it up: until its Hook "ready", on a
// message "ready?", has fired.
func hookConfig(t *testing.T, n, sender *node, body string) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	path := n.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, readFile(t, path)+body+fmt.Sprintf(`
[jobs.ready]
exec = ["touch", %q]

[hooks.ready]
text = "^ready[?]$"
jobs = ["ready"]
`, ready))
	// A later mtime than any write before, however coarse the clock.
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's Hooks never fired", n.name)
		}
		if _, err := sender.client().ConversationSend(context.Background(), "ready?", nil); err != nil {
			t.Fatal(err)
		}
	}
}

// waitFile waits until path exists.
func waitFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", filepath.Base(path))
		}
	}
}

// waitLog waits until logs hold a line containing substr.
func waitLog(t *testing.T, logs *logLines, substr string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); len(logs.matching(substr)) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never said %q", substr)
		}
	}
}

// nextChain reads s until a chain starts and returns that chain's events,
// to its end.
func nextChain(t *testing.T, s *serve.ChainStream) []serve.ChainEvent {
	t.Helper()
	for {
		ev, err := s.Next()
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if ev.Type == serve.EventChainStart {
			return append([]serve.ChainEvent{ev}, untilEnd(t, s, ev.Chain)...)
		}
	}
}

// A Hook's agent Job runs a chain in the event's Conversation, streamed
// and written to a Transcript as any chain is: the agent reads the talk,
// then the Job's prompt and the event as its last user turn, which is
// never committed, and answers at the event's depth + 1. The Hook's next
// Job runs once the chain has ended. An agent Job whose Handle is not in
// its target's profiles fails, and its Hook's next Job does not run.
func TestAHooksAgentJobRunsAChain(t *testing.T) {
	var logs logLines
	a, b, gid, provider := agentPairTuned(t, func(o *serve.Options) {
		o.Getenv = os.Getenv
		o.Logf = logs.logf
	}, "auditor")
	after, never := filepath.Join(t.TempDir(), "after"), filepath.Join(t.TempDir(), "never")
	hookConfig(t, a, b, fmt.Sprintf(`
[jobs.review]
agent = "auditor"
prompt = "Review what bob said."

[jobs.stranger]
agent = "scribe"
prompt = "Never runs."

[jobs.after]
exec = ["touch", %q]

[jobs.never]
exec = ["touch", %q]

[hooks.review]
text = "^hello$"
from = "others"
jobs = ["review", "after"]

[hooks.broken]
text = "^hello$"
jobs = ["stranger", "never"]
`, after, never))
	s := watch(t, a)
	calls := len(provider.seen())
	if _, err := b.client().ConversationSend(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	events := nextChain(t, s)
	if got := summary(events); !strings.HasPrefix(got, "chain.start start:@auditor/1 ") || !strings.HasSuffix(got, "commit:reply "+fmt.Sprint(calls+1)+" end") {
		t.Fatalf("events = %s", got)
	}
	call := provider.seen()[calls]
	last := call.Turns[strings.LastIndex(call.Turns, "[user] "):]
	for _, want := range []string{"bob: hello", "Review what bob said.", `"trigger":"review"`, `"text":"hello"`} {
		if !strings.Contains(last, want) {
			t.Errorf("last user turn lacks %q: %q", want, last)
		}
	}
	tr := filepath.Join(a.paths.GroupDir(group.ImplicitAlias), "conversations", "group", "transcripts", events[0].Chain+".jsonl")
	if !strings.Contains(readFile(t, tr), `"kind":"outcome"`) {
		t.Errorf("%s has no outcome", tr)
	}
	waitFile(t, after)
	for _, ff := range freeforms(t, b.groupLevelRows(t, gid, 1)) {
		if strings.Contains(ff.Text, "Review what bob said.") {
			t.Errorf("the Job's prompt was committed: %+v", ff)
		}
		if ff.Handle == "auditor" && ff.Depth != 1 {
			t.Errorf("the reply is at depth %d, want 1", ff.Depth)
		}
	}
	waitLog(t, &logs, "job stranger failed")
	if got := logs.matching("job stranger failed"); len(got) != 1 || !strings.Contains(got[0], "@scribe is not in [agent] profiles") {
		t.Errorf("log %v, want stranger failing on its Handle", got)
	}
	if _, err := os.Stat(never); err == nil {
		t.Error("a Hook ran a Job after an agent Job that failed")
	}
}

// An agent Job answers in the event's Conversation unless its target
// names another, here Group-level talk, and at the event's depth + 1.
func TestAnAgentJobsTargetAndDepth(t *testing.T) {
	a, b, gid, provider := agentPairTuned(t, func(o *serve.Options) { o.Getenv = os.Getenv }, "auditor")
	replyAs(provider, map[string]string{"auditor": "looked"})
	name := mintKey(t, a, b, gid)
	waitWorkspace(t, a, name)
	maxDepth2(t, a)
	hookConfig(t, a, b, `
[jobs.here]
agent = "auditor"
prompt = "Look here."

[jobs.there]
agent = "auditor"
target = "group"
prompt = "Report to the Group."

[hooks.check]
text = "^check$"
jobs = ["here", "there"]
`)
	id := key.ConversationID("ecdsa", bytes.Repeat([]byte{0x33}, 33))
	ctx := context.Background()
	inKey, err := a.client().ConversationStream(ctx, id[:])
	if err != nil {
		t.Fatal(err)
	}
	defer inKey.Close()
	groupLevel := watch(t, a)
	if _, err := b.client().ConversationSendAs(ctx, "check", "", 1, id[:]); err != nil {
		t.Fatal(err)
	}
	for where, s := range map[string]*serve.ChainStream{"the Key's Conversation": inKey, "Group-level talk": groupLevel} {
		if got := summary(nextChain(t, s)); !strings.HasPrefix(got, "chain.start start:@auditor/2 ") || !strings.HasSuffix(got, "commit:looked end") {
			t.Errorf("%s: events = %s", where, got)
		}
	}
	if ff := freeforms(t, waitConversation(t, b, gid, id[:], 2)[1:2])[0]; ff.Handle != "auditor" || ff.Depth != 2 {
		t.Errorf("reply in the Key's Conversation = %+v", ff)
	}
}
