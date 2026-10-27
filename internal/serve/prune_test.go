package serve_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

// With prune on, a Node prunes when a Recap commits and after its replica
// is rebuilt from a Node that kept the talk, logging what it dropped; with
// it off, a Node destroys nothing. The two hold different amounts of talk
// and still agree on the chain and replicate to each other.
func TestPrunePolicyKeepsALightNodeLight(t *testing.T) {
	var bobLog logLines
	a, b := pairWith(t, func(n *node) {
		if n.name == "bob" {
			n.tune = func(o *serve.Options) {
				o.Config.Prune = true
				o.Logf = bobLog.logf
			}
		}
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	ctx := context.Background()

	var sent []serve.SendView
	for _, text := range []string{"one", "two", "three"} {
		v, err := a.client().ConversationSend(ctx, text, nil)
		if err != nil {
			t.Fatal(err)
		}
		sent = append(sent, v)
	}
	b.groupLevelRows(t, gid, 3)
	if _, err := a.client().ConversationRecap(ctx, "one and two", sent[1].Seq, nil); err != nil {
		t.Fatal(err)
	}
	waitPruned(t, b, []uint64{sent[0].Seq, sent[1].Seq})
	// The line follows the prune, so it may land a moment after.
	line := "pruned 2 messages of Group-level talk up to seq " + fmt.Sprint(sent[1].Seq)
	for deadline := time.Now().Add(5 * time.Second); len(bobLog.matching(line)) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if got := bobLog.matching(line); len(got) != 1 {
		t.Errorf("bob's log has %v, want one line saying what was pruned", bobLog.matching("pruned"))
	}

	// A wider Recap extends what bob holds no payload for.
	if _, err := b.client().ConversationRecap(ctx, "all three", sent[2].Seq, nil); err != nil {
		t.Fatal(err)
	}
	all := []uint64{sent[0].Seq, sent[1].Seq, sent[2].Seq}
	waitPruned(t, b, all)

	// Alice, an archival Node, still holds everything, and both still replicate.
	v, err := b.client().ConversationSend(ctx, "after", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.groupLevelRows(t, gid, 4)
	time.Sleep(time.Second) // long enough for a pruning serve to have pruned
	ap, ah := prunedSeqs(t, a)
	bp, bh := prunedSeqs(t, b)
	if len(ap) != 0 {
		t.Fatalf("alice, with prune off, pruned %v", ap)
	}
	if ah != bh || !slices.Equal(bp, all) {
		t.Fatalf("after %d: heads %x / %x, bob pruned %v", v.Seq, ah, bh, bp)
	}

	// Rebuilt from alice, bob gets the talk back and prunes it again as his
	// serve starts. This log is far shorter than a snapshot's threshold, so
	// he is rebuilt by replay; recap's Keeper tests cover an InstallSnapshot
	// restore.
	b.stop()
	for _, name := range []string{"cosl.sqlite", "cosl.sqlite-wal", "cosl.sqlite-shm", "raft.db", "raft-snap"} {
		if err := os.RemoveAll(filepath.Join(b.paths.GroupDir(group.ImplicitAlias), name)); err != nil {
			t.Fatal(err)
		}
	}
	b.restart(t)
	b.groupLevelRows(t, gid, 4)
	waitPruned(t, b, all)
}
