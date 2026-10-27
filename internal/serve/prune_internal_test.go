package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/cosl/coslbench"
	"defios.com/internal/group"
	"defios.com/internal/nodedir"
)

// policyOn is a prune policy over an empty replica, starting as on says
// and logging to lines.
func policyOn(t *testing.T, on bool, lines *[]string) (*server, *cosl.Store, *prunePolicy) {
	t.Helper()
	store, err := cosl.Open(filepath.Join(t.TempDir(), "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := &server{opts: Options{
		Paths: nodedir.Resolve(nodedir.Lookup{Dir: filepath.Join(t.TempDir(), "node")}),
		Alias: group.ImplicitAlias,
		Logf:  func(format string, args ...any) { *lines = append(*lines, fmt.Sprintf(format, args...)) },
	}}
	s.opts.Config.Prune = on
	return s, store, s.newPrunePolicy(store)
}

// setPrune writes the Group's config.toml with body, stamped at a time of
// its own so each write reads as a change.
func setPrune(t *testing.T, s *server, body string, at time.Time) {
	t.Helper()
	path := s.opts.Paths.GroupConfigFile(s.opts.Alias)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// restoreTalk rebuilds the replica from one that kept all its talk.
func restoreTalk(t *testing.T, store *cosl.Store) {
	t.Helper()
	if err := store.Restore(coslbench.Build(t, coslbench.Shapes[0]).Path); err != nil {
		t.Fatal(err)
	}
}

func prunedCount(t *testing.T, store *cosl.Store) int {
	t.Helper()
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.Pruned() {
			n++
		}
	}
	return n
}

func matching(lines []string, substr string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}

// Setting `prune` in config.toml while serve runs takes effect at the next
// fallback tick: on, the policy prunes what is covered; off again, it
// destroys nothing, even after a restore brings the talk back.
func TestPrunePolicyFollowsConfigWithoutARestart(t *testing.T) {
	var lines []string
	s, store, p := policyOn(t, false, &lines)
	restoreTalk(t, store)
	p.wake()
	if n := prunedCount(t, store); n != 0 {
		t.Fatalf("with prune off, %d rows pruned", n)
	}

	t0 := time.Now().Add(-time.Hour)
	setPrune(t, s, "prune = true\n", t0)
	p.wake() // a wake alone does not reread config.toml
	if n := prunedCount(t, store); n != 0 {
		t.Fatalf("before the fallback tick, %d rows pruned", n)
	}
	p.fallback()
	if n := prunedCount(t, store); n == 0 {
		t.Fatal("after turning prune on and a fallback tick, nothing pruned")
	}
	if got := matching(lines, "pruned "); len(got) != 1 {
		t.Fatalf("logged %q, want one line saying what was pruned", lines)
	}

	// Turned off on a fresh Node, the policy leaves the talk a restore
	// brings alone.
	s, store, p = policyOn(t, true, &lines)
	p.wake()
	setPrune(t, s, "prune = false\n", t0)
	p.fallback()
	restoreTalk(t, store)
	p.wake()
	p.fallback()
	if n := prunedCount(t, store); n != 0 {
		t.Fatalf("after turning prune off and a restore, %d rows pruned", n)
	}
}

// A config.toml that does not parse is logged once, and the policy keeps
// the setting it last read.
func TestPrunePolicyKeepsItsLastSettingOverAMalformedConfig(t *testing.T) {
	var lines []string
	s, store, p := policyOn(t, true, &lines)
	t0 := time.Now().Add(-time.Hour)
	setPrune(t, s, "prune = \n", t0)
	for range 3 {
		p.fallback()
	}
	if got := matching(lines, "config.toml"); len(got) != 1 {
		t.Fatalf("after three ticks over a malformed config.toml logged %q, want one line", lines)
	}

	restoreTalk(t, store)
	p.wake()
	if n := prunedCount(t, store); n == 0 {
		t.Fatal("the policy dropped its last setting over a malformed config.toml")
	}

	setPrune(t, s, "prune = true\n", t0.Add(time.Minute))
	p.fallback()
	if got := matching(lines, "ok again"); len(got) != 1 {
		t.Fatalf("after fixing config.toml logged %q, want it cleared once", lines)
	}
}

// A prune that fails the same way on every wake is logged once. With the
// policy off, a wake does not touch the replica at all.
func TestPrunePolicyLogsAPersistentErrorOnce(t *testing.T) {
	var lines []string
	_, store, p := policyOn(t, true, &lines)
	store.Close()
	for range 3 {
		p.wake()
	}
	if len(lines) != 1 {
		t.Fatalf("after three failing wakes logged %q, want one line", lines)
	}

	lines = nil
	_, store, p = policyOn(t, false, &lines)
	store.Close()
	p.wake()
	p.fallback()
	if len(lines) != 0 {
		t.Fatalf("with prune off, a wake over a closed replica logged %q", lines)
	}
}
