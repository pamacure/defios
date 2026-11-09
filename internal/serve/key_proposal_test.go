package serve_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
)

// loggedPair is two Nodes whose serve logs are kept.
func loggedPair(t *testing.T, prep func(*node)) (a, b *node, logs map[*node]*logLines) {
	t.Helper()
	logs = map[*node]*logLines{}
	a, b = pairWith(t, func(n *node) {
		if prep != nil {
			prep(n)
		}
		book, inner := &logLines{}, n.tune
		logs[n] = book
		n.tune = func(o *serve.Options) {
			if inner != nil {
				inner(o)
			}
			o.Logf = book.logf
		}
	})
	return a, b, logs
}

func aliasOn(t *testing.T, n *node, name string) string {
	t.Helper()
	w, err := config.ReadWorkspace(n.paths.ConversationConfigFile(group.ImplicitAlias, name))
	if err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	return w.Alias
}

func waitAlias(t *testing.T, n *node, name, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := aliasOn(t, n, name)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: alias of %s = %q, want %q", n.name, name, got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// One proposal on session.start names the Key on every Node party to it, as
// it was typed, in the Workspace the Node has just created.
func TestAProposalBecomesTheKeysAliasOnEveryNode(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

	name := mintProposing(t, a, b, gid, bytes.Repeat([]byte{0x44}, 33), "Cold")
	waitAlias(t, a, name, "Cold")
	waitAlias(t, b, name, "Cold")
}

// A proposal that cannot be an alias leaves the Key with none, and the
// warning does not repeat what another Node sent. The Key still has its
// Workspace.
func TestAnInvalidProposalLeavesNoAliasAndWarnsWithoutPrintingIt(t *testing.T) {
	a, b, logs := loggedPair(t, nil)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

	const bad = "\x1b[31mevil"
	name := mintProposing(t, a, b, gid, bytes.Repeat([]byte{0x45}, 33), bad)
	for _, n := range []*node{a, b} {
		waitWorkspace(t, n, name)
		waitLog(t, logs[n], "is invalid")
		if leaked := logs[n].matching("evil"); len(leaked) != 0 {
			t.Fatalf("%s logged the invalid proposal: %q", n.name, leaked)
		}
		if got := aliasOn(t, n, name); got != "" {
			t.Fatalf("%s: alias = %q", n.name, got)
		}
	}
}

// A proposal taken on one Node leaves the Key with no alias there and a
// warning, and takes effect on the Node where it is free. Compared as Key
// aliases are, whatever the case.
func TestAProposalTakenOnOneNodeLeavesNoAliasThereOnly(t *testing.T) {
	a, b, logs := loggedPair(t, nil)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

	first := mintProposing(t, a, b, gid, bytes.Repeat([]byte{0x46}, 33), "")
	waitWorkspace(t, a, first)
	waitWorkspace(t, b, first)
	writeConfig(t, a.paths.ConversationConfigFile(group.ImplicitAlias, first), "alias = \"COLD\"\n")

	second := mintProposing(t, a, b, gid, bytes.Repeat([]byte{0x47}, 33), "cold")
	waitAlias(t, b, second, "cold")
	waitLog(t, logs[a], `"cold"`)
	if got := aliasOn(t, a, second); got != "" {
		t.Fatalf("alice: alias = %q", got)
	}
}

// config.toml alone decides once the Workspace exists: a restart reads the
// proposal off the COSL again, and must not put a cleared or renamed alias back.
func TestRestartDoesNotApplyAProposalAgain(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	name := mintProposing(t, a, b, gid, bytes.Repeat([]byte{0x48}, 33), "Cold")
	waitAlias(t, a, name, "Cold")
	waitAlias(t, b, name, "Cold")

	writeConfig(t, a.paths.ConversationConfigFile(group.ImplicitAlias, name), "")
	writeConfig(t, b.paths.ConversationConfigFile(group.ImplicitAlias, name), "alias = \"Renamed\"\n")
	a.restart(t)
	b.restart(t)
	// A Key minted after the restart proves each watcher has run its scan.
	next := mintProposing(t, a, b, gid, bytes.Repeat([]byte{0x49}, 33), "Next")
	waitAlias(t, a, next, "Next")
	waitAlias(t, b, next, "Next")
	if got := aliasOn(t, a, name); got != "" {
		t.Errorf("cleared alias came back: %q", got)
	}
	if got := aliasOn(t, b, name); got != "Renamed" {
		t.Errorf("renamed alias = %q", got)
	}
}

func sessionStarts(t *testing.T, n *node) []*coslv1.SessionStart {
	t.Helper()
	store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeSessionStart}})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]*coslv1.SessionStart, len(rows))
	for i, row := range rows {
		env, err := row.Decode()
		if err != nil {
			t.Fatal(err)
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = msg.(*coslv1.SessionStart)
	}
	return out
}

// session start refuses at once an alias that could not be used here, and
// appends nothing; a good one goes on session.start in stored form.
func TestSessionStartProposesAnAliasOrRefusesItAppendingNothing(t *testing.T) {
	a, b := pairWith(t, recordsStances)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	ctx := context.Background()
	name := mintKey(t, a, b, gid)
	waitWorkspace(t, a, name)
	file := a.paths.ConversationConfigFile(group.ImplicitAlias, name)
	writeConfig(t, file, "alias = \"Cold\"\n")

	for _, bad := range []string{"cOLD", " spaced", "a\x01b", strings.Repeat("x", 64)} {
		if _, err := a.client().SessionStart(ctx, "ecdsa", 2, bad); !errors.Is(err, serve.ErrBadAlias) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	// A config.toml that did not read might hold the alias, so it refuses too.
	writeConfig(t, file, "alias = \n")
	if _, err := a.client().SessionStart(ctx, "ecdsa", 2, "Warm"); err == nil || !strings.Contains(err.Error(), file) {
		t.Errorf("broken config.toml: err = %v", err)
	}
	if got := len(sessionStarts(t, a)); got != 1 {
		t.Fatalf("session.start rows = %d, want only the mint's", got)
	}
	writeConfig(t, file, "alias = \"Cold\"\n")

	sum, err := a.client().SessionStart(ctx, "ecdsa", 2, "Cafe\u0301")
	if err != nil {
		t.Fatal(err)
	}
	if sum.ProposedAlias != "Caf\u00e9" {
		t.Errorf("summary = %+v", sum)
	}
	// The call returns at commit; this replica may apply the row just after.
	deadline := time.Now().Add(20 * time.Second)
	starts := sessionStarts(t, a)
	for len(starts) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		starts = sessionStarts(t, a)
	}
	if len(starts) != 2 || string(starts[1].ProposedAlias) != "Caf\u00e9" {
		t.Fatalf("session.start rows = %+v", starts)
	}
}
