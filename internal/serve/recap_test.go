package serve_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/nodedir"
	"defios.com/internal/recap"
	"defios.com/internal/serve"
)

// writeRecap writes n's RECAP.md for Group-level talk.
func writeRecap(t *testing.T, n *node, baseSeq uint64, text string) {
	t.Helper()
	path := n.paths.RecapFile(group.ImplicitAlias, nodedir.ConversationName(nil))
	if err := recap.Write(path, recap.Recap{BaseSeq: baseSeq, Text: text}); err != nil {
		t.Fatal(err)
	}
}

// sendAll sends texts as n's person, Group-level, and returns their seqs.
func sendAll(t *testing.T, n *node, texts ...string) []uint64 {
	t.Helper()
	var seqs []uint64
	for _, text := range texts {
		v, err := n.client().ConversationSend(context.Background(), text, nil)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, v.Seq)
	}
	return seqs
}

// summon has n's person mention text and waits for the chain to end.
func summon(t *testing.T, n *node, text string) serve.SendView {
	t.Helper()
	s := watch(t, n)
	v, err := n.client().ConversationSend(context.Background(), text, nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.Seq)
	return v
}

// recapOpening is how the first user turn opens under a Recap of baseSeq.
func recapOpening(baseSeq uint64, text string) string {
	return fmt.Sprintf("(This Node's Recap: its own account of the talk up to seq %d, in place of that talk.)\n%s\n(End of the Recap. The talk after it follows.)", baseSeq, text)
}

// Each Node's agents read its own RECAP.md, at the start of the first user
// turn, then only the talk after its base_seq; another Node's Recap never
// reaches them.
func TestEachNodesAgentsReadOnlyItsOwnRecap(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor")
	agentNode(t, b, provider.URL, "auditor")
	seqs := sendAll(t, a, "one", "two", "three")
	b.groupLevelRows(t, gid, 3)
	writeRecap(t, a, seqs[0], "alice's account of one")
	writeRecap(t, b, seqs[1], "bob's account of one and two")

	summon(t, a, "@auditor what now?")
	b.groupLevelRows(t, gid, 5)
	summon(t, b, "@auditor and you?")

	alice, bob := provider.seenWith("sk-"+a.name), provider.seenWith("sk-"+b.name)
	if len(alice) != 1 || len(bob) != 1 {
		t.Fatalf("calls: alice %d, bob %d", len(alice), len(bob))
	}
	want := "[user] " + recapOpening(seqs[0], "alice's account of one") + "\nalice: two\nalice: three\nalice: @auditor what now?\n"
	if alice[0].Turns != want {
		t.Errorf("alice's agent read\n%s\nwant\n%s", alice[0].Turns, want)
	}
	want = "[user] " + recapOpening(seqs[1], "bob's account of one and two") + "\nalice: three\nalice: @auditor what now?\nalice/@auditor: reply 1\nbob: @auditor and you?\n"
	if bob[0].Turns != want {
		t.Errorf("bob's agent read\n%s\nwant\n%s", bob[0].Turns, want)
	}
	for _, c := range append(alice, bob...) {
		if strings.Contains(c.System, "missing") {
			t.Errorf("nothing is pruned, yet the preamble says talk is missing: %q", c.System)
		}
	}
}

// A Recap that opens the talk takes the place of the fixed turn that would
// open talk starting with the agent's own message.
func TestARecapOpensTalkThatStartsWithTheAgentsOwnMessage(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	v := summon(t, a, "@auditor first")
	writeRecap(t, a, v.Seq, "alice asked first")
	summon(t, a, "@auditor again")
	calls := provider.seen()
	want := "[user] " + recapOpening(v.Seq, "alice asked first") + "\n[assistant] reply 1\n[user] alice: @auditor again\n"
	if len(calls) != 2 || calls[1].Turns != want {
		t.Fatalf("turns =\n%s\nwant\n%s", calls[len(calls)-1].Turns, want)
	}
}

// pruneOn destroys the payloads of n's Group-level talk at or below seq, as
// `conversation prune` does.
func pruneOn(t *testing.T, n *node, through uint64) {
	t.Helper()
	store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Prune(nil, through); err != nil {
		t.Fatal(err)
	}
}

// When this replica has pruned talk its Recap does not account for, or any
// talk with no Recap, the preamble says earlier talk is missing; pruned
// talk the Recap accounts for is not missing.
func TestThePreambleSaysWhenPrunedTalkIsMissing(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	seqs := sendAll(t, a, "one", "two", "three")
	missing := func() bool {
		t.Helper()
		calls := provider.seen()
		return strings.Contains(calls[len(calls)-1].System, "Earlier talk is missing")
	}

	pruneOn(t, a, seqs[1])
	writeRecap(t, a, seqs[1], "one and two")
	summon(t, a, "@auditor covered?")
	if missing() {
		t.Errorf("the Recap accounts for all pruned talk, yet the preamble says talk is missing")
	}

	writeRecap(t, a, seqs[0], "one")
	summon(t, a, "@auditor past the Recap?")
	if !missing() {
		t.Errorf("seq %d is pruned past the Recap, yet the preamble does not say talk is missing", seqs[1])
	}
	if calls := provider.seen(); strings.Contains(calls[len(calls)-1].Turns, "alice: two") {
		t.Errorf("pruned talk was read: %s", calls[len(calls)-1].Turns)
	}

	if err := os.Remove(a.paths.RecapFile(group.ImplicitAlias, "group")); err != nil {
		t.Fatal(err)
	}
	summon(t, a, "@auditor no Recap?")
	if !missing() {
		t.Errorf("talk is pruned and there is no Recap, yet the preamble does not say talk is missing")
	}
}

// A RECAP.md that does not parse fails the summon, naming the file, rather
// than being silently ignored.
func TestABadRECAPmdFailsTheSummon(t *testing.T) {
	a, _, _, _ := agentPair(t, "auditor")
	path := a.paths.RecapFile(group.ImplicitAlias, "group")
	writeRecap(t, a, 1, "fine")
	if err := os.WriteFile(path, []byte("no front matter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor?", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := untilEnd(t, s, v.Seq)
	for _, ev := range events {
		if ev.Type == serve.EventAgentError && strings.Contains(ev.Error, path) {
			return
		}
	}
	t.Fatalf("events = %s, want an agent error naming %s", summary(events), path)
}
