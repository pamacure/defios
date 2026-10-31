package serve_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/serve"
)

// logLines collects a serve's log, safe for its goroutines.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) matching(substr string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, substr) {
			out = append(out, s)
		}
	}
	return out
}

// prunedSeqs is the seqs whose payload this Node has destroyed, and its
// chain head.
func prunedSeqs(t *testing.T, n *node) ([]uint64, [32]byte) {
	t.Helper()
	store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	var out []uint64
	for _, r := range rows {
		if r.Pruned() {
			out = append(out, r.Seq)
		}
	}
	meta, err := store.Meta()
	if err != nil {
		t.Fatal(err)
	}
	return out, meta.ChainHead
}

// waitPruned polls until the Node has pruned exactly want.
func waitPruned(t *testing.T, n *node, want []uint64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var got []uint64
	for time.Now().Before(deadline) {
		if got, _ = prunedSeqs(t, n); slices.Equal(got, want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: pruned seqs %v, want %v", n.name, got, want)
}

// With a prune age set, a Node prunes the talk older than it as its serve
// starts, logging what it dropped, and not as each message commits; with
// none, a Node destroys nothing. The two hold different amounts of talk and
// still agree on the chain and replicate to each other. The policy's runs
// after a restore and once a day are its own tests'.
func TestPrunePolicyPrunesOldTalkOnStart(t *testing.T) {
	var bobLog logLines
	a, b := pairWith(t, func(n *node) {
		if n.name == "bob" {
			n.tune = func(o *serve.Options) {
				o.Config.Prune = config.Age(time.Second)
				o.Logf = bobLog.logf
			}
		}
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	ctx := context.Background()

	var sent []serve.SendView
	for _, text := range []string{"one", "two"} {
		v, err := a.client().ConversationSend(ctx, text, nil)
		if err != nil {
			t.Fatal(err)
		}
		sent = append(sent, v)
	}
	b.groupLevelRows(t, gid, 2)
	time.Sleep(1500 * time.Millisecond) // the talk comes of age
	if got, _ := prunedSeqs(t, b); len(got) != 0 {
		t.Fatalf("bob pruned %v as talk committed; the policy runs on start, after a restore and daily", got)
	}

	b.restart(t)
	old := []uint64{sent[0].Seq, sent[1].Seq}
	waitPruned(t, b, old)
	// The line follows the prune, so it may land a moment after.
	line := "pruned 2 messages of Group-level talk older than 1s"
	for deadline := time.Now().Add(5 * time.Second); len(bobLog.matching(line)) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if got := bobLog.matching(line); len(got) != 1 {
		t.Errorf("bob's log has %v, want one line saying what was pruned", bobLog.matching("pruned"))
	}

	// Alice, an archival Node, still holds everything, and both still replicate.
	v, err := b.client().ConversationSend(ctx, "after", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.groupLevelRows(t, gid, 3)
	b.groupLevelRows(t, gid, 3)
	ap, ah := prunedSeqs(t, a)
	bp, bh := prunedSeqs(t, b)
	if len(ap) != 0 {
		t.Fatalf("alice, with no prune policy, pruned %v", ap)
	}
	if ah != bh || !slices.Equal(bp, old) {
		t.Fatalf("after %d: heads %x / %x, bob pruned %v", v.Seq, ah, bh, bp)
	}
}
