package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/cosl/coslbench"
	"defios.com/internal/group"
	"defios.com/internal/nodedir"
	"defios.com/internal/recap"
	coslv1 "defios.com/proto/cosl/v1"
)

const day = 24 * time.Hour

// policyOn is a prune policy over an empty replica, starting with age as
// its policy (0: none), logging to lines, and reading the time from clock.
func policyOn(t *testing.T, age time.Duration, lines *[]string, clock *time.Time) (*server, *cosl.Store, *prunePolicy) {
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
	s.opts.Config.Prune = config.Age(age)
	p := s.newPrunePolicy(store)
	p.now = func() time.Time { return *clock }
	return s, store, p
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

// restoreTalk rebuilds the replica from one that kept all its talk, plus
// further more messages after it, so that each call with a larger further
// is a snapshot the replica restores from. It reports when the oldest talk
// was written: a cached replica's talk may be days old.
func restoreTalk(t *testing.T, store *cosl.Store, further int) time.Time {
	t.Helper()
	r := coslbench.Build(t, coslbench.Shapes[0])
	if further > 0 {
		snap, err := cosl.Open(r.Path)
		if err != nil {
			t.Fatal(err)
		}
		meta, err := snap.Meta()
		if err != nil {
			t.Fatal(err)
		}
		fsm := cosl.NewFSM(snap, r.Roster)
		author := cosl.Author{Node: r.Nodes[0], GroupID: r.Roster.GroupID()}
		for i := range further {
			s, err := author.Sign(cosl.TypeAppFreeform, nil, &coslv1.AppFreeform{Text: "further"})
			if err != nil {
				t.Fatal(err)
			}
			if res, err := fsm.Apply(meta.LastAppliedSeq+uint64(i)+1, 1, s.Bytes); err != nil || !res.Inserted {
				t.Fatalf("apply: %+v %v", res, err)
			}
		}
		snap.Close()
	}
	if err := store.Restore(r.Path); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}})
	if err != nil || len(rows) == 0 {
		t.Fatalf("talk: %d rows, %v", len(rows), err)
	}
	oldest := rows[0].CreatedAt
	for _, r := range rows {
		oldest = min(oldest, r.CreatedAt)
	}
	return time.Unix(0, oldest)
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

// With an age set, the policy prunes the talk older than it as serve
// starts and again as soon as a snapshot restore brings that talk back,
// logging what each run dropped.
func TestPrunePolicyPrunesOnStartAndAfterARestore(t *testing.T) {
	var lines []string
	clock := time.Now()
	_, store, p := policyOn(t, day, &lines, &clock)
	clock = restoreTalk(t, store, 0).Add(2 * day)
	p.wake()
	n := prunedCount(t, store)
	if n == 0 {
		t.Fatal("on start, nothing older than a day pruned")
	}
	if got := matching(lines, "older than 1d"); len(got) == 0 {
		t.Fatalf("logged %q, want what was pruned", lines)
	}

	logged := len(lines)
	p.wake()
	if len(lines) != logged {
		t.Fatalf("a wake with nothing new logged %q", lines[logged:])
	}
	restoreTalk(t, store, 1)
	if prunedCount(t, store) != 0 {
		t.Fatal("the restore did not bring the talk back")
	}
	p.wake()
	if got := prunedCount(t, store); got < n {
		t.Fatalf("after a restore, %d rows pruned, want the %d old ones again", got, n)
	}
}

// Between restores the policy runs once a day, so talk that comes of age
// is pruned within a day of it.
func TestPrunePolicyRunsOnceADay(t *testing.T) {
	var lines []string
	clock := time.Now()
	_, store, p := policyOn(t, day, &lines, &clock)
	written := restoreTalk(t, store, 0)
	clock = written.Add(23 * time.Hour)
	p.wake()
	clock = written.Add(25 * time.Hour)
	p.wake() // the talk is of age, but the policy ran two hours ago
	if n := prunedCount(t, store); n != 0 {
		t.Fatalf("%d rows pruned less than a day after the last run", n)
	}
	clock = written.Add(23*time.Hour + day)
	p.wake()
	if n := prunedCount(t, store); n == 0 {
		t.Fatal("a day after the last run, nothing pruned")
	}
}

// Setting `prune` in config.toml while serve runs takes effect at the next
// fallback tick: set, the policy prunes what is old enough; removed, it
// destroys nothing, even after a restore brings the talk back.
func TestPrunePolicyFollowsConfigWithoutARestart(t *testing.T) {
	var lines []string
	clock := time.Now()
	s, store, p := policyOn(t, 0, &lines, &clock)
	clock = restoreTalk(t, store, 0).Add(2 * day)
	p.wake()
	if n := prunedCount(t, store); n != 0 {
		t.Fatalf("with no policy, %d rows pruned", n)
	}

	t0 := time.Now().Add(-time.Hour)
	setPrune(t, s, "prune = \"1d\"\n", t0)
	p.wake() // a wake alone does not reread config.toml
	if n := prunedCount(t, store); n != 0 {
		t.Fatalf("before the fallback tick, %d rows pruned", n)
	}
	p.fallback()
	if n := prunedCount(t, store); n == 0 {
		t.Fatal("after setting prune and a fallback tick, nothing pruned")
	}
	if got := matching(lines, "pruned "); len(got) != 2 {
		t.Fatalf("logged %q, want a line for each Conversation it pruned", lines)
	}

	// Removed on a fresh Node, the policy leaves the talk a restore brings
	// alone.
	s, store, p = policyOn(t, day, &lines, &clock)
	p.wake()
	setPrune(t, s, "", t0)
	p.fallback()
	restoreTalk(t, store, 0)
	p.wake()
	p.fallback()
	if n := prunedCount(t, store); n != 0 {
		t.Fatalf("after removing prune and a restore, %d rows pruned", n)
	}
}

// A config.toml that does not parse, such as a boolean prune, is logged
// once, and the policy keeps the setting it last read.
func TestPrunePolicyKeepsItsLastSettingOverAMalformedConfig(t *testing.T) {
	var lines []string
	clock := time.Now()
	s, store, p := policyOn(t, day, &lines, &clock)
	t0 := time.Now().Add(-time.Hour)
	setPrune(t, s, "prune = true\n", t0)
	for range 3 {
		p.fallback()
	}
	if got := matching(lines, "config.toml"); len(got) != 1 {
		t.Fatalf("after three ticks over a malformed config.toml logged %q, want one line", lines)
	}

	clock = restoreTalk(t, store, 0).Add(2 * day)
	p.wake()
	if n := prunedCount(t, store); n == 0 {
		t.Fatal("the policy dropped its last setting over a malformed config.toml")
	}

	setPrune(t, s, "prune = \"1d\"\n", t0.Add(time.Minute))
	p.fallback()
	if got := matching(lines, "ok again"); len(got) != 1 {
		t.Fatalf("after fixing config.toml logged %q, want it cleared once", lines)
	}
}

// A prune that fails the same way on every wake is logged once. With no
// policy, a wake does not touch the replica at all.
func TestPrunePolicyLogsAPersistentErrorOnce(t *testing.T) {
	var lines []string
	clock := time.Now()
	_, store, p := policyOn(t, day, &lines, &clock)
	store.Close()
	for range 3 {
		p.wake()
	}
	if len(lines) != 1 {
		t.Fatalf("after three failing wakes logged %q, want one line", lines)
	}

	lines = nil
	_, store, p = policyOn(t, 0, &lines, &clock)
	store.Close()
	p.wake()
	p.fallback()
	if len(lines) != 0 {
		t.Fatalf("with no policy, a wake over a closed replica logged %q", lines)
	}
}

// The policy warns when it destroys talk past this Node's RECAP.md, or any
// talk of a Conversation with no RECAP.md: this Node's agents then lose it
// with no account in its place. Talk the Recap accounts for prunes quietly.
func TestPrunePolicyWarnsWhenItPrunesPastTheRecap(t *testing.T) {
	var lines []string
	clock := time.Now()
	s, store, p := policyOn(t, day, &lines, &clock)
	clock = restoreTalk(t, store, 0).Add(2 * day)
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}})
	if err != nil {
		t.Fatal(err)
	}
	var lastGroup uint64
	for _, r := range rows {
		if r.ConversationID == nil {
			lastGroup = r.Seq
		}
	}
	recapFile := func(conv []byte) string {
		return s.opts.Paths.RecapFile(s.opts.Alias, nodedir.ConversationName(conv))
	}
	if err := recap.Write(recapFile(nil), recap.Recap{BaseSeq: lastGroup, Text: "all of it"}); err != nil {
		t.Fatal(err)
	}
	p.wake()
	warnings := matching(lines, "warning")
	if len(warnings) != 1 || strings.Contains(warnings[0], "Group-level") || !strings.Contains(warnings[0], "no RECAP.md") {
		t.Fatalf("logged %q, want one warning, for the Key's Conversation, which has no RECAP.md", lines)
	}

	lines = nil
	if err := recap.Write(recapFile(nil), recap.Recap{BaseSeq: rows[0].Seq, Text: "the first message"}); err != nil {
		t.Fatal(err)
	}
	restoreTalk(t, store, 1)
	p.wake()
	warnings = matching(lines, "warning")
	if len(warnings) != 2 || !slices.ContainsFunc(warnings, func(l string) bool {
		return strings.Contains(l, "Group-level talk") && strings.Contains(l, fmt.Sprintf("past seq %d", rows[0].Seq))
	}) {
		t.Fatalf("logged %q, want a warning for Group-level talk pruned past its RECAP.md", lines)
	}
}
