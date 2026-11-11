package node_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"defios.com/internal/identity"
	"defios.com/internal/node"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

// mintedKey is the public key every Node's keygen engine mints here: the
// secp256k1 generator, a valid compressed point.
var mintedKey, _ = hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")

// minter completes a keygen at once with mintedKey, so a test has a Key
// whose Sign Requests it can propose. It never signs.
type minter struct{ done chan session.Result }

func (m *minter) Start(session.Params) error {
	m.done <- session.Result{PublicKey: mintedKey, SaveData: []byte("share")}
	return nil
}
func (m *minter) Receive([32]byte, bool, []byte) error { return nil }
func (m *minter) Out() <-chan session.Outgoing         { return nil }
func (m *minter) Done() <-chan session.Result          { return m.done }
func (m *minter) WaitingFor() [][32]byte               { return nil }
func (m *minter) Abort()                               {}

func mintSessions(identity.ID) (node.Sessions, error) {
	return node.Sessions{Keygen: func() session.Engine { return &minter{done: make(chan session.Result, 1)} }}, nil
}

// ethNetwork is a Network for Ethereum mainnet, which a Node needs to
// propose or accept a request naming it.
const ethNetwork = "\n[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\n"

var payEth = []signreq.TxSpec{{Network: "eth", Ethereum: json.RawMessage(`{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000","gasLimit":"21000","maxFeePerGas":"30"}`)}}

// keyed is alice and bob serving Group "alpha", formed, with a 2-of-2
// Key minted; each Group's config.toml has extra appended before serving.
func keyed(t *testing.T, aliceExtra, bobExtra string) (alice, bob *member, key string) {
	t.Helper()
	m := newMesh()
	alice = joinConfigured(t, m, "alice", mintSessions, aliceExtra)
	bob = joinConfigured(t, m, "bob", mintSessions, bobExtra)
	ctx := context.Background()
	f, err := alice.h.Propose(ctx, "alpha", []string{alice.id, bob.id})
	if err != nil {
		t.Fatal(err)
	}
	next(t, bob.events, proposalEvent)
	if _, err := bob.h.Accept(ctx, "alpha", f.GroupID); err != nil {
		t.Fatal(err)
	}
	for _, mb := range []*member{alice, bob} {
		until(t, mb.events, func(s node.Status) bool { return s.Leader != "" && s.MajorityReachable })
	}
	var started node.Session
	retry(t, func() (err error) {
		started, err = alice.h.StartKeygen(ctx, "alpha", node.Keygen{Curve: "ecdsa", T: 2})
		return err
	})
	retry(t, func() error { return bob.h.AcceptSession(ctx, "alpha", started.SessionID, "") })
	for _, mb := range []*member{alice, bob} {
		retry(t, func() error {
			keys, err := mb.h.Keys(ctx, "alpha", nil)
			if err == nil && len(keys) != 1 {
				err = fmt.Errorf("%d Keys", len(keys))
			}
			if err == nil {
				key = keys[0].Key
			}
			return err
		})
	}
	return alice, bob, key
}

// joinConfigured is joinWith, with sessions and with extra appended to the
// Group's config.toml before it is served.
func joinConfigured(t *testing.T, m *mesh, name string, sessions func(identity.ID) (node.Sessions, error), extra string) *member {
	t.Helper()
	p := create(t, name)
	h, err := node.Open(p, node.Options{NewTailnet: m.tailnet, StatusInterval: 20 * time.Millisecond, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Lock() })
	if _, err := h.NewGroup(node.GroupSpec{Alias: "alpha", HeadscaleURL: "https://hs.example"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p.GroupConfigFile("alpha"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mb := &member{h: h, p: p, id: h.NodeID(), events: h.Subscribe(ctx)}
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	until(t, mb.events, func(s node.Status) bool { return s.TailnetUp })
	return mb
}

// retry calls f until it succeeds, for writes a fresh Group's settling
// leader may turn down for a moment.
func retry(t *testing.T, f func() error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// signEvent matches a Sign Request event for msgID with status.
func signEvent(msgID, status string) func(node.Event) *node.SignRequestEvent {
	return func(ev node.Event) *node.SignRequestEvent {
		if s := ev.SignRequest; s != nil && s.Request.MsgID == msgID && s.Request.Status == status {
			return s
		}
		return nil
	}
}

func TestAProposedRequestShowsEachNodesStanceAndItsStatusMoves(t *testing.T) {
	alice, bob, key := keyed(t, ethNetwork, ethNetwork)
	ctx := context.Background()

	var sent node.Sent
	retry(t, func() (err error) {
		sent, err = alice.h.ProposeSignRequest(ctx, "alpha", key, node.SignSpec{Reason: "pay the invoice", Transactions: payEth})
		return err
	})
	if sent.Queued || sent.Seq == 0 {
		t.Fatalf("proposed %+v", sent)
	}
	opened := next(t, bob.events, signEvent(sent.MsgID, signreq.Open))
	if opened.Group != "alpha" || opened.Request.Proposer != alice.id || opened.Request.Stance != signreq.StanceUnaddressed {
		t.Fatalf("bob's event = %+v", opened)
	}

	if _, err := bob.h.RefuseSignRequest(ctx, "alpha", sent.MsgID[:8], " "); !errors.As(err, new(*node.InvalidError)) {
		t.Fatalf("a blank reason: %v; want InvalidError", err)
	}
	if a, err := bob.h.RefuseSignRequest(ctx, "alpha", sent.MsgID[:8], "unknown payee"); err != nil || a.Queued {
		t.Fatalf("refuse = %+v, %v", a, err)
	}
	// 2 of 2: one refusal leaves too few to accept.
	refused := next(t, alice.events, signEvent(sent.MsgID, signreq.Refused))
	if r := refused.Request; len(r.Refusals) != 1 || r.Refusals[0].Node != bob.id || r.Refusals[0].Reason != "unknown payee" || r.Stance != signreq.StanceAccepted {
		t.Fatalf("alice's event = %+v", refused.Request)
	}

	next(t, bob.events, signEvent(sent.MsgID, signreq.Refused))
	list, err := bob.h.SignRequests(ctx, "alpha")
	if err != nil || len(list) != 1 {
		t.Fatalf("bob's list = %+v, %v", list, err)
	}
	if r := list[0]; r.Status != signreq.Refused || r.Stance != signreq.StanceRefused || len(r.Accepts) != 1 || r.Accepts[0].Node != alice.id {
		t.Fatalf("bob lists %+v", r)
	}
	d, err := alice.h.InspectSignRequest(ctx, "alpha", sent.MsgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Details) != 1 || d.Details[0].Chain != "ethereum chain 1" || len(d.Details[0].Digests) != 1 {
		t.Fatalf("decoded %+v", d.Details)
	}
	if _, err := alice.h.InspectSignRequest(ctx, "alpha", "ffff"); !errors.Is(err, signreq.ErrUnknown) {
		t.Fatalf("an unknown request: %v; want signreq.ErrUnknown", err)
	}
	if _, err := bob.h.AcceptSignRequest(ctx, "alpha", sent.MsgID); !errors.Is(err, session.ErrAlreadyRefused) {
		t.Fatalf("accept after refusing: %v; want session.ErrAlreadyRefused", err)
	}
}

// A deadline passing moves a request to expired, which no message marks.
func TestARequestWhoseDeadlinePassesIsSeenExpired(t *testing.T) {
	alice, bob, key := keyed(t, ethNetwork, ethNetwork)
	ctx := context.Background()
	var sent node.Sent
	retry(t, func() (err error) {
		sent, err = alice.h.ProposeSignRequest(ctx, "alpha", key, node.SignSpec{Reason: "quick", DeadlineSeconds: 1, Transactions: payEth})
		return err
	})
	for _, mb := range []*member{alice, bob} {
		next(t, mb.events, signEvent(sent.MsgID, signreq.Expired))
	}
}

// When a Hook of this Node's fires on a request, the request says which
// Jobs ran, the Handle of an agent Job, and how each ended.
func TestAHookFiringOnARequestShowsItsJobsAndHowTheyEnded(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	hooks := ethNetwork + fmt.Sprintf(`
[jobs.note]
exec = ["true"]

[jobs.review]
agent = "reviewer"
prompt = "check the payee"

[jobs.after]
exec = ["true"]

[jobs.ready]
exec = ["touch", %q]

[hooks.review_requests]
events = ["sign.request"]
jobs = ["note", "review", "after"]

[hooks.ready]
text = "^ready[?]$"
jobs = ["ready"]
`, ready)
	alice, bob, key := keyed(t, ethNetwork, hooks)
	ctx := context.Background()
	// Bob's Hooks fire once live, after the startup catch-up: talk until
	// one has.
	retry(t, func() error {
		if _, err := os.Stat(ready); err == nil {
			return nil
		}
		_, err := alice.h.Send(ctx, "alpha", node.GroupTalk, node.Outgoing{Text: "ready?"})
		if err == nil {
			err = errors.New("bob's Hooks are not live yet")
		}
		return err
	})

	var sent node.Sent
	retry(t, func() (err error) {
		sent, err = alice.h.ProposeSignRequest(ctx, "alpha", key, node.SignSpec{Reason: "pay", Transactions: payEth})
		return err
	})
	ended := next(t, bob.events, func(ev node.Event) *node.HookRunEvent {
		if r := ev.HookRun; r != nil && r.Run.Event.SignRequestID == sent.MsgID && r.Run.Jobs[1].State == node.JobFailed {
			return r
		}
		return nil
	})
	run := ended.Run
	if run.Trigger != "review_requests" || run.Event.Type != "sign.request" || len(run.Jobs) != 3 {
		t.Fatalf("run = %+v", run)
	}
	want := []node.JobRun{
		{Job: "note", Kind: "exec", State: node.JobOK},
		{Job: "review", Kind: "agent", Handle: "reviewer", State: node.JobFailed},
		{Job: "after", Kind: "exec", State: node.JobNotRun},
	}
	for i, w := range want {
		g := run.Jobs[i]
		if g.Job != w.Job || g.Kind != w.Kind || g.Handle != w.Handle || g.State != w.State {
			t.Errorf("job %d = %+v; want %+v", i, g, w)
		}
	}
	if run.Jobs[1].Error == "" {
		t.Error("the failed agent Job says nothing of why")
	}

	d, err := bob.h.InspectSignRequest(ctx, "alpha", sent.MsgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Hooks) != 1 || d.Hooks[0].Trigger != "review_requests" || d.Hooks[0].Jobs[1].Handle != "reviewer" {
		t.Fatalf("bob's inspect Hooks = %+v", d.Hooks)
	}
	list, err := bob.h.SignRequests(ctx, "alpha")
	if err != nil || len(list) != 1 || len(list[0].Hooks) != 1 {
		t.Fatalf("bob's list = %+v, %v", list, err)
	}
	next(t, alice.events, signEvent(sent.MsgID, signreq.Open))
	if d, err := alice.h.InspectSignRequest(ctx, "alpha", sent.MsgID); err != nil || len(d.Hooks) != 0 {
		t.Fatalf("alice, with no Hook, shows %+v, %v", d.Hooks, err)
	}
}

// A refusal queued in this Node's Outbox, with no majority to commit it, is
// already its stance: the request's event says so before any row lands.
func TestARefusalQueuedInTheOutboxIsSeenAsThisNodesStance(t *testing.T) {
	alice, bob, key := keyed(t, ethNetwork, ethNetwork)
	ctx := context.Background()
	var sent node.Sent
	retry(t, func() (err error) {
		sent, err = alice.h.ProposeSignRequest(ctx, "alpha", key, node.SignSpec{Reason: "pay", Transactions: payEth})
		return err
	})
	next(t, bob.events, signEvent(sent.MsgID, signreq.Open))
	if err := alice.h.Lock(); err != nil {
		t.Fatal(err)
	}
	until(t, bob.events, func(s node.Status) bool { return !s.MajorityReachable })
	retry(t, func() error {
		a, err := bob.h.RefuseSignRequest(ctx, "alpha", sent.MsgID, "not without alice")
		if err == nil && !a.Queued {
			err = errors.New("the refusal committed")
		}
		return err
	})
	ev := next(t, bob.events, func(ev node.Event) *node.SignRequestEvent {
		if s := ev.SignRequest; s != nil && s.Request.MsgID == sent.MsgID && s.Request.Stance == signreq.StanceRefused {
			return s
		}
		return nil
	})
	if ev.Request.Status != signreq.Open || len(ev.Request.Refusals) != 0 {
		t.Fatalf("bob's event = %+v; want open, with the refusal not on the log yet", ev.Request)
	}
}
