package serve_test

import (
	"context"
	"fmt"
	"os"
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
	untilEnd(t, s, v.MsgID)
	return v
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

// An agent is given no Recap (ADR 0036): this Node's RECAP.md is not read
// for it, so one that does not parse fails no summon. Talk this Node has
// pruned since the agent's last run shows in its turn as one line giving
// its seqs, the agent's own pruned reply among them.
func TestATurnShowsPrunedTalkAsAGapAndReadsNoRecap(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	first := summon(t, a, "@auditor first")
	seqs := sendAll(t, a, "one", "two", "three")
	pruneOn(t, a, seqs[1])
	writeRecap(t, a, 1, "fine")
	if err := os.WriteFile(a.paths.RecapFile(group.ImplicitAlias, "group"), []byte("no front matter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	again := summon(t, a, "@auditor again")
	calls := provider.seen()
	want := fmt.Sprintf("(Talk since your last run:)\n[%d–%d pruned on this Node]\n[%d] alice: three\n", first.Seq+1, seqs[1], seqs[2]) + summonedBy(again.Seq, "alice: @auditor again")
	if len(calls) != 2 || calls[1].Prompt != want {
		t.Fatalf("turn =\n%s\nwant\n%s", calls[len(calls)-1].Prompt, want)
	}
}
