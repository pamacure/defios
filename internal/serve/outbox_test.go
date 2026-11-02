package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/outbox"
	"defios.com/internal/serve"
	"defios.com/internal/signreq"
)

// waitLeaderless polls until n's serve knows no leader.
func (n *node) waitLeaderless(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := n.client().Status(context.Background()); err == nil && st.RaftUp && st.Leader == "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s still knows a leader", n.name)
}

// writtenRows are n's committed talk and Sign Requests, in log order.
func writtenRows(t *testing.T, n *node) []cosl.Row {
	t.Helper()
	store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform, cosl.TypeSignRequest}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// waitWritten polls until n has want such rows.
func waitWritten(t *testing.T, n *node, want int) []cosl.Row {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var rows []cosl.Row
	for time.Now().Before(deadline) {
		if rows = writtenRows(t, n); len(rows) >= want {
			return rows
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: %d rows, want %d", n.name, len(rows), want)
	return nil
}

// With the other Node of a 2-Node Group down, talk and a Sign Request are
// queued, not committed, and still are across a restart of their author.
// When the other Node returns, they commit on both replicas in the order
// they were written (ADR 0025).
func TestOutboxHoldsWritesUntilAMajorityReturns(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	mintKey(t, a, b, gid)
	pk := bytes.Repeat([]byte{0x33}, 33)
	conv := key.ConversationID("ecdsa", pk)
	waitMinted(t, a, gid, conv[:])
	waitMinted(t, b, gid, conv[:])
	keyQuery := "ecdsa:" + hexOf(pk)
	setNetworks(t, a, "[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\n")

	b.stop()
	a.waitLeaderless(t)
	ctx := context.Background()
	queued := func(v serve.SendView, err error) serve.SendView {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if !v.Queued || v.Seq != 0 || len(v.MsgID) != 64 {
			t.Fatalf("view = %+v, want queued with a msg_id and no seq", v)
		}
		return v
	}
	one := queued(a.client().ConversationSend(ctx, "one", nil))
	eth := json.RawMessage(`{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000","gasLimit":"21000","maxFeePerGas":"30"}`)
	req := queued(a.client().SignPropose(ctx, keyQuery, signreq.Spec{Reason: "pay", Transactions: []signreq.TxSpec{{Network: "eth", Ethereum: eth}}}))

	a.restart(t)
	three := queued(a.client().ConversationSend(ctx, "three", conv[:]))
	if rows := writtenRows(t, a); len(rows) != 0 {
		t.Fatalf("a's replica has %d queued rows", len(rows))
	}

	b.restart(t)
	want := []string{one.MsgID, req.MsgID, three.MsgID}
	for _, n := range []*node{a, b} {
		rows := waitWritten(t, n, len(want))
		for i, r := range rows {
			if hexOf(r.MsgID[:]) != want[i] {
				t.Fatalf("%s: row %d is %s, want %v in write order", n.name, i, hexOf(r.MsgID[:]), want)
			}
		}
	}
	waitEmptyOutbox(t, a)
}

// waitEmptyOutbox polls until n's Outbox file holds nothing.
func waitEmptyOutbox(t *testing.T, n *node) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ob, err := outbox.Open(outbox.Config{Path: n.paths.OutboxFile(group.ImplicitAlias)})
		if err == nil && len(ob.Entries()) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(n.paths.OutboxFile(group.ImplicitAlias)); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("%s: Outbox not empty after the drain", n.name)
}

// With no leader, a person's mention of @auditor, whose reply summons
// @scribe, runs both: each reply is queued, and @scribe reads @auditor's
// from this Node's view of the talk. When the queued messages commit, no
// chain runs again (ADR 0025).
func TestAChainRunsWithNoLeader(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor", "scribe")
	maxDepth2(t, a)
	replyAs(provider, map[string]string{"auditor": "ask @scribe", "scribe": "done"})
	b.stop()
	a.waitLeaderless(t)
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor check", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Queued {
		t.Fatalf("send = %+v, want queued", v)
	}
	events := untilEnd(t, s, v.MsgID)
	want := `chain.start start:@auditor/1 tool:say {"text":"ask @scribe"} commit:ask @scribe start:@scribe/2 tool:say {"text":"done"} commit:done end`
	if got := summary(events); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	for _, ev := range events {
		if ev.Type == serve.EventAgentCommit && (!ev.Queued || ev.Seq != 0) {
			t.Fatalf("agent.commit %+v, want queued with no seq", ev)
		}
	}
	calls := provider.seen()
	if want := "alice: @auditor check\nalice/@auditor: ask @scribe"; len(calls) != 2 || calls[1].Prompt != want {
		t.Fatalf("calls = %+v, want @scribe's prompt %q", calls, want)
	}

	b.restart(t)
	rows := a.groupLevelRows(t, gid, 3)
	if ffs := freeforms(t, rows); ffs[0].Text != "@auditor check" || ffs[1].Text != "ask @scribe" || ffs[2].Text != "done" {
		t.Fatalf("rows = %+v", ffs)
	}
	waitEmptyOutbox(t, a)
	// One chain at a time runs per Conversation, so once this one ends any
	// chain the commits had started has run.
	v, err = a.client().ConversationSend(context.Background(), "@scribe again", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.MsgID)
	if n := len(provider.seen()); n != 3 {
		t.Fatalf("provider called %d times, want 3: a chain ran again when its messages committed", n)
	}
}

// outbox drop through a running serve erases exactly one queued message;
// when the other Node returns the rest commit in write order, and dropping
// one of them then says it committed (ADR 0025).
func TestOutboxDropThroughServe(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	b.stop()
	a.waitLeaderless(t)
	ctx := context.Background()
	var sent []string
	for _, text := range []string{"one", "two", "three"} {
		v, err := a.client().ConversationSend(ctx, text, nil)
		if err != nil || !v.Queued {
			t.Fatalf("send %q = %+v, %v; want queued", text, v, err)
		}
		sent = append(sent, v.MsgID)
	}
	v, err := a.client().OutboxDrop(ctx, sent[1][:10])
	if err != nil || v.MsgID != sent[1] {
		t.Fatalf("drop = %+v, %v; want %s", v, err, sent[1])
	}
	if _, err := a.client().OutboxDrop(ctx, sent[1]); !errors.Is(err, outbox.ErrNotQueued) {
		t.Fatalf("second drop = %v, want ErrNotQueued", err)
	}

	b.restart(t)
	rows := waitWritten(t, a, 2)
	if got := []string{hexOf(rows[0].MsgID[:]), hexOf(rows[1].MsgID[:])}; got[0] != sent[0] || got[1] != sent[2] || len(rows) != 2 {
		t.Fatalf("committed %v, want %s then %s", got, sent[0], sent[2])
	}
	waitEmptyOutbox(t, a)
	if _, err := a.client().OutboxDrop(ctx, sent[0]); !errors.Is(err, outbox.ErrCommitted) || !strings.Contains(err.Error(), "committed at seq") {
		t.Fatalf("drop of a committed message = %v, want ErrCommitted with its seq", err)
	}
}
