//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/serve"
)

// TestFocusSwitchReconvergence (ADR 0006, #29): a single Node-level socket
// gives one-live-serve-at-a-time, an unfocused Group is offline to its peers,
// and refocusing reconverges it through Raft without a manual resync.
//
// alice holds two Groups: the implicit Group A (formed with bob) and a second
// Group "backup" (B). alice serves A, appends, then switches focus to B. While
// alice serves B, Group A is offline to bob — bob cannot commit. When alice
// refocuses to A, the Group reconverges to the committed prefix and new
// appends flow again. Along the way, `group use` while a serve is running must
// not hot-switch the live Group; it takes effect on the next serve.
//
// bob's Current Group stays A throughout, so convergence is observed on bob,
// while alice reads its non-current Group A explicitly with `--group default`.
func TestFocusSwitchReconvergence(t *testing.T) {
	hs := startHeadscale(t)
	alice, bob, _ := formValidPair(t, hs) // both up on Group A (implicit alias)

	// Append on A: a Group-level message commits with both Nodes up. Two
	// membership.join rows plus this message is the committed prefix (3 rows).
	alice.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "on A before switch")
	mustConverge(t, alice, bob, 3)

	// Scaffold Group B on alice. `group new` auto-focuses it, so alice's Current
	// Group pointer now names backup while alice's serve still serves A. bob
	// never leaves A, so its reads stay scoped to A.
	if r := alice.run(context.Background(), "group", "new", "backup", "--headscale", hs.url); r.code != 0 {
		t.Fatalf("alice group new backup: exit %d: %s", r.code, r.stderr)
	}

	// Criterion: `group use` while a serve is running does not hot-switch the
	// live Group. The pointer is backup, yet a send still lands on A (the Group
	// the running serve serves) and commits with bob, who is on A.
	if r := alice.run(context.Background(), "group", "use", "backup"); r.code != 0 {
		t.Fatalf("alice group use backup: exit %d: %s", r.code, r.stderr)
	}
	alice.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "still on A after group use")
	// bob (Current Group A) sees the new row: the live serve ignored the moved
	// pointer. alice reads the same A log via --group default, not its pointer.
	// Both are polled: a send returns on commit, and the follower's FSM apply
	// (the sqlite row `cosl log` reads) can lag that by a beat.
	bob.coslLog(t, func(rows []coslLine) bool { return len(rows) == 4 })
	alice.waitCoslGroup(t, group.ImplicitAlias, 4)

	// Switch focus to B: stop A's serve and start one for backup. The pointer
	// (backup) takes effect on this next serve, and the single socket means
	// exactly one serve is live, so A is now offline to bob.
	alice.stopServe(t)
	alice.startServe(t) // pointer is backup, so this serves B

	// Second criterion (one live serve): starting a second serve for any Group
	// while one is live fails with a clear message and the first keeps running.
	r := alice.run(context.Background(), "serve")
	if r.code != 2 || !strings.Contains(r.stderr, "already running") {
		t.Fatalf("second serve: exit %d, stderr %q; want 2 and 'already running'", r.code, r.stderr)
	}

	// Unfocused reconvergence, part 1: Group A is offline to bob. With alice's
	// A-serve down there is no quorum, so bob cannot commit and the committed
	// prefix is unchanged on his replica.
	before := bob.coslNow(t)
	if send := bob.run(context.Background(), "conversation", "send", "anyone on A?"); send.code != 2 {
		t.Fatalf("bob send while A unfocused: exit %d (want 2); stderr %q", send.code, send.stderr)
	}
	if now := bob.coslNow(t); len(now) != len(before) {
		t.Fatalf("bob committed while A unfocused: %d rows, had %d", len(now), len(before))
	}

	// Refocus to A: stop B's serve, point back at the implicit alias, serve A.
	alice.stopServe(t)
	if r := alice.run(context.Background(), "group", "use", group.ImplicitAlias); r.code != 0 {
		t.Fatalf("alice group use default: exit %d: %s", r.code, r.stderr)
	}
	alice.startServe(t)

	// Unfocused reconvergence, part 2: with no manual resync, A reconverges to
	// the committed prefix via Raft catch-up, and new appends commit again.
	bob.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "back on A")
	rows := mustConverge(t, alice, bob, 5)

	// The pre-switch committed prefix survived the focus switch intact.
	texts := conversationTexts(t, alice)
	for _, want := range []string{"on A before switch", "still on A after group use", "back on A"} {
		if !slices.Contains(texts, want) {
			t.Fatalf("message %q missing after reconvergence; converged rows:\n%+v", want, rows)
		}
	}
}

// waitCoslGroup polls `cosl log --group alias` until that Group's COSL has at
// least want rows, so a Node can wait on a Group that is not its current one
// (its FSM apply can lag a committed send). No serve needed.
func (n *node) waitCoslGroup(t *testing.T, alias string, want int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		r := n.run(context.Background(), "cosl", "log", "--group", alias, "--json")
		if r.code == 0 {
			var rows []coslLine
			if err := json.Unmarshal([]byte(r.stdout), &rows); err == nil {
				if last = len(rows); last >= want {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s cosl log --group %s has %d rows; want >= %d (serve did not hot-switch off A)", n.name, alias, last, want)
}

// conversationTexts returns the Group-level conversation message texts in order.
func conversationTexts(t *testing.T, n *node) []string {
	t.Helper()
	rows, _ := n.conversationLog(t, func([]messageLine) bool { return true })
	texts := make([]string, 0, len(rows))
	for _, r := range rows {
		texts = append(texts, r.Text)
	}
	return texts
}
