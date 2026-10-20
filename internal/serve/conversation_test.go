package serve_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
)

// groupLevelRows polls the Node's replica until n Group-level app.freeform
// rows are there.
func (n *node) groupLevelRows(t *testing.T, gid group.ID, want int) []cosl.Row {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var rows []cosl.Row
	for time.Now().Before(deadline) {
		store, err := cosl.Open(n.paths.CoslFile(gid.String()))
		if err == nil {
			typ := cosl.TypeAppFreeform
			rows, err = store.Log(cosl.Query{MsgType: &typ, GroupLevel: true})
			store.Close()
			if err == nil && len(rows) >= want {
				return rows
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: %d group-level rows, want %d", n.name, len(rows), want)
	return nil
}

// conversation send from either Node lands on both replicas as a
// Group-level app.freeform signed by the sender (§8).
func TestConversationSendFromEitherNodeReachesBoth(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

	ctx := context.Background()
	va, err := a.client().ConversationSend(ctx, "hello from alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := b.client().ConversationSend(ctx, "hello from bob", nil)
	if err != nil {
		t.Fatal(err)
	}
	if va.Seq == 0 || vb.Seq <= va.Seq || len(va.MsgID) != 64 {
		t.Fatalf("views = %+v, %+v", va, vb)
	}

	ra, rb := a.groupLevelRows(t, gid, 2), b.groupLevelRows(t, gid, 2)
	for i := range ra {
		if ra[i].Seq != rb[i].Seq || ra[i].MsgID != rb[i].MsgID || ra[i].ConversationID != nil {
			t.Fatalf("replicas differ at %d:\n%+v\n%+v", i, ra[i], rb[i])
		}
	}
	texts := map[identity.ID]string{}
	for _, r := range ra {
		env, _ := r.Decode()
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			t.Fatal(err)
		}
		ff := msg.(*coslv1.AppFreeform)
		if ff.ContentType != "text/plain" {
			t.Fatalf("content_type = %q", ff.ContentType)
		}
		texts[r.Sender] = ff.Text
	}
	if texts[a.id.NodeID] != "hello from alice" || texts[b.id.NodeID] != "hello from bob" {
		t.Fatalf("texts = %v", texts)
	}
}

// A send with a Conversation ID lands on both replicas with that id set.
// A Group-level send on the same Group stays unset and out of the filter.
func TestConversationSendSetsConversationID(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

	conv := bytes.Repeat([]byte{0x11}, 32)
	ctx := context.Background()
	if _, err := a.client().ConversationSend(ctx, "about the key", conv); err != nil {
		t.Fatal(err)
	}
	if _, err := b.client().ConversationSend(ctx, "group", nil); err != nil {
		t.Fatal(err)
	}

	scoped := func(n *node) []cosl.Row {
		t.Helper()
		store, err := cosl.Open(n.paths.CoslFile(gid.String()))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		rows, err := store.Log(cosl.Query{ConversationID: conv})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	ra, rb := scoped(a), scoped(b)
	if len(ra) != 1 || len(rb) != 1 || ra[0].MsgID != rb[0].MsgID || !bytes.Equal(ra[0].ConversationID, conv) {
		t.Fatalf("scoped replicas:\n%+v\n%+v", ra, rb)
	}
	group := a.groupLevelRows(t, gid, 1)
	if len(group) != 1 || group[0].ConversationID != nil {
		t.Fatalf("group-level = %+v", group)
	}
}

// Without a valid Group there is no replica to append to; bad text is the
// caller's error.
func TestConversationSendErrors(t *testing.T) {
	a := newNode(t, "alice")
	a.start(t)
	ctx := context.Background()
	if _, err := a.client().ConversationSend(ctx, "anyone?", nil); !errors.Is(err, serve.ErrNoReplica) {
		t.Fatalf("send without a Group = %v, want ErrNoReplica", err)
	}
	if _, err := a.client().ConversationSend(ctx, "", nil); !errors.Is(err, cosl.ErrBadText) {
		t.Fatalf("send empty = %v, want ErrBadText", err)
	}
}
