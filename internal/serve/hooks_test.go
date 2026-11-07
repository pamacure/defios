package serve_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/serve"
	"defios.com/internal/signreq"
)

// readLines is the file's lines, none if it is missing.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(strings.TrimSpace(string(b)))
}

// waitLines polls until the file holds want, failing with what it holds.
func waitLines(t *testing.T, path string, want []string) {
	t.Helper()
	var got []string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if got = readLines(t, path); slices.Equal(got, want) {
			return
		}
	}
	t.Fatalf("%s holds %v, want %v", filepath.Base(path), got, want)
}

// record is an exec Job appending its event's Type and the message's text
// to path, one word each; it takes a moment, so firings that overlapped
// would interleave.
func record(path string) config.Job {
	return config.Job{Exec: []string{"sh", "-c", `echo "start:$DEFIOS_EVENT" >> "$0"; sleep 0.05; grep -o '"text":"[^"]*"' | cut -d '"' -f 4 >> "$0"`, path}}
}

// A Hook fires on talk this serve applies live, its Jobs in order until
// one fails, one firing at a time, and never again on the backlog a
// restarted serve starts from. A Hook added to config.toml while serve runs
// fires from the next event on.
func TestAHookFiresOnLiveTalkOnly(t *testing.T) {
	out := filepath.Join(t.TempDir(), "said")
	never := filepath.Join(t.TempDir(), "never")
	var bobLog logLines
	a, b := pairWith(t, func(n *node) {
		if n.name != "bob" {
			return
		}
		n.tune = func(o *serve.Options) {
			o.Getenv = os.Getenv
			o.Logf = bobLog.logf
			o.Config.Jobs = map[string]config.Job{
				"record": record(out),
				"fail":   {Exec: []string{"false"}},
				"after":  {Exec: []string{"touch", never}},
			}
			o.Config.Hooks = map[string]config.Hook{
				"said":   {Events: []string{"app.freeform"}, From: config.Names{"others"}, Jobs: []string{"record"}},
				"broken": {Events: []string{"app.freeform"}, Jobs: []string{"fail", "after"}},
			}
		}
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	ctx := context.Background()
	for _, text := range []string{"one", "two", "three"} {
		if _, err := a.client().ConversationSend(ctx, text, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitLines(t, out, []string{"start:app.freeform", "one", "start:app.freeform", "two", "start:app.freeform", "three"})
	// Bob's own talk is not "others".
	if _, err := b.client().ConversationSend(ctx, "mine", nil); err != nil {
		t.Fatal(err)
	}
	b.groupLevelRows(t, gid, 4)

	// What bob catches up on as it restarts is backlog: send once it is live.
	live := "serve: hooks: live after seq"
	before := len(bobLog.matching(live))
	b.restart(t)
	for deadline := time.Now().Add(20 * time.Second); len(bobLog.matching(live)) == before; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("bob's Hooks never went live after the restart")
		}
	}
	if _, err := a.client().ConversationSend(ctx, "four", nil); err != nil {
		t.Fatal(err)
	}
	waitLines(t, out, []string{"start:app.freeform", "one", "start:app.freeform", "two", "start:app.freeform", "three", "start:app.freeform", "four"})

	if _, err := os.Stat(never); err == nil {
		t.Error("a Hook ran a Job after one that failed")
	}
	if len(bobLog.matching(`hook broken: job fail failed`)) == 0 {
		t.Errorf("bob's log %v, want the failed Job", bobLog.matching("hook"))
	}

	// A Hook written into config.toml is taken up without a restart.
	late := filepath.Join(t.TempDir(), "late")
	cfg := config.Config{DisplayName: "bob", Hostname: "defios-bob",
		Jobs:  map[string]config.Job{"record": record(late)},
		Hooks: map[string]config.Hook{"late": {Jobs: []string{"record"}, Text: "^five$"}},
	}
	if err := os.MkdirAll(b.paths.GroupConfigDir(group.ImplicitAlias), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteFile(b.paths.GroupConfigFile(group.ImplicitAlias), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := a.client().ConversationSend(ctx, "five", nil); err != nil {
		t.Fatal(err)
	}
	waitLines(t, late, []string{"start:app.freeform", "five"})
	if got := readLines(t, out); len(got) != 8 {
		t.Errorf("the removed Hook still fired: %v", got)
	}
}

// A sign.* event has its request's depth, and a request.<status> event
// too, so a Hook answering a Job's proposal sees how deep it already is;
// a message a Job writes at a depth, though no agent's, keeps it.
func TestSignEventsHaveTheirRequestsDepth(t *testing.T) {
	out, ready := filepath.Join(t.TempDir(), "depths"), filepath.Join(t.TempDir(), "ready")
	a, b := pairWith(t, func(n *node) {
		recordsStances(n)
		stances := n.tune
		n.tune = func(o *serve.Options) { stances(o); o.Getenv = os.Getenv }
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	mintKey(t, a, b, gid)
	pk := bytes.Repeat([]byte{0x33}, 33)
	conv := key.ConversationID("ecdsa", pk)
	eth := "[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\n"
	for _, n := range []*node{a, b} {
		waitMinted(t, n, gid, conv[:])
	}
	setNetworks(t, b, eth)
	// Alice's config.toml, which serve rereads, holds her Hook.
	setNetworks(t, a, eth+fmt.Sprintf(`
[agent]
max_depth = 3

[jobs.depth]
exec = ["sh", "-c", "echo \"$DEFIOS_EVENT:$DEFIOS_DEPTH\" >> \"$0\"", %q]

[jobs.ready]
exec = ["touch", %q]

[hooks.signs]
events = ["sign.request", "sign.refuse", "request.open", "request.refused"]
jobs = ["depth"]

[hooks.job_talk]
text = "^from a Job$"
jobs = ["depth"]

[hooks.ready]
text = "^ready[?]$"
jobs = ["ready"]
`, out, ready))
	ctx := context.Background()
	// Bob's request lands once alice's Hook is taken up: talk first, until
	// it fires.
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("alice's Hook never fired")
		}
		if _, err := b.client().ConversationSend(ctx, "ready?", nil); err != nil {
			t.Fatal(err)
		}
	}
	v, err := b.client().SignPropose(ctx, "ecdsa:"+hexOf(pk), signreq.Spec{Reason: "pay", Transactions: payEth, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	waitLines(t, out, []string{"sign.request:2", "request.open:2"})
	if d := waitSign(t, a, v.MsgID, func(signreq.Detail) bool { return true }); d.Depth != 1 {
		t.Errorf("inspect depth %d, want 1", d.Depth)
	}
	if _, err := a.client().SignRefuse(ctx, v.MsgID, "no"); err != nil {
		t.Fatal(err)
	}
	waitLines(t, out, []string{"sign.request:2", "request.open:2", "sign.refuse:2", "request.refused:2"})

	if _, err := b.client().ConversationSendAs(ctx, "from a Job", "", 2, nil); err != nil {
		t.Fatal(err)
	}
	waitLines(t, out, []string{"sign.request:2", "request.open:2", "sign.refuse:2", "request.refused:2", "app.freeform:3"})
}
