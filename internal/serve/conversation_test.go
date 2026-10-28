package serve_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/key"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
	raftv1 "defios.com/proto/raft/v1"
)

// groupLevelRows polls the Node's replica until n Group-level app.freeform
// rows are there.
func (n *node) groupLevelRows(t *testing.T, gid group.ID, want int) []cosl.Row {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var rows []cosl.Row
	for time.Now().Before(deadline) {
		store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
		if err == nil {
			rows, err = store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}, GroupLevel: true})
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

// An id that no minted Key derives is refused and leaves no row (§8).
// Group-level send on the same Group still commits with conversation_id unset.
func TestConversationSendRejectsUnmintedKey(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

	conv := bytes.Repeat([]byte{0x11}, 32)
	ctx := context.Background()
	if _, err := a.client().ConversationSend(ctx, "about the key", conv); !errors.Is(err, serve.ErrNoConversation) {
		t.Fatalf("unminted send: %v", err)
	}
	if rows := conversationRows(t, a, gid, conv); len(rows) != 0 {
		t.Fatalf("unminted conversation stored: %+v", rows)
	}
	if _, err := b.client().ConversationSend(ctx, "group", nil); err != nil {
		t.Fatal(err)
	}
	group := a.groupLevelRows(t, gid, 1)
	if len(group) != 1 || group[0].ConversationID != nil {
		t.Fatalf("group-level = %+v", group)
	}
}

// A send with a minted Key's Conversation ID lands on both replicas with
// that id set. A Group-level send stays unset and out of the filter.
func TestConversationSendSetsConversationID(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

	pk := bytes.Repeat([]byte{0x11}, 33)
	var sid [32]byte
	copy(sid[:], pk)
	parts := [][]byte{a.id.NodeID[:], b.id.NodeID[:]}
	sign := func(n *node, msg proto.Message, typ uint32) cosl.Signed {
		t.Helper()
		s, err := (cosl.Author{Node: n.id, GroupID: gid}).Sign(typ, nil, msg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	commitEnvelope(t, a, b, sign(a, &coslv1.SessionStart{
		SessionId: sid[:], Curve: "ecdsa", T: 2, N: 2, Participants: parts,
	}, cosl.TypeSessionStart))
	complete := &coslv1.SessionComplete{SessionId: sid[:], Curve: "ecdsa", PublicKey: pk}
	commitEnvelope(t, a, b, sign(a, complete, cosl.TypeSessionComplete))
	commitEnvelope(t, a, b, sign(b, complete, cosl.TypeSessionComplete))

	id := key.ConversationID("ecdsa", pk)
	waitMinted(t, a, gid, id[:])
	if _, err := a.client().ConversationSend(context.Background(), "about the key", id[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := b.client().ConversationSend(context.Background(), "group", nil); err != nil {
		t.Fatal(err)
	}
	ra := waitConversation(t, a, gid, id[:], 1)
	rb := waitConversation(t, b, gid, id[:], 1)
	if ra[0].MsgID != rb[0].MsgID || !bytes.Equal(ra[0].ConversationID, id[:]) {
		t.Fatalf("scoped replicas:\n%+v\n%+v", ra, rb)
	}
	group := a.groupLevelRows(t, gid, 1)
	if len(group) != 1 || group[0].ConversationID != nil {
		t.Fatalf("group-level = %+v", group)
	}
}

// commitEnvelope appends a signed envelope through the leader's forward gate.
func commitEnvelope(t *testing.T, a, b *node, signed cosl.Signed) {
	t.Helper()
	body, err := proto.Marshal(&raftv1.ForwardRequest{Envelope: signed.Bytes})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		for _, n := range []*node{a, b} {
			req, err := http.NewRequest(http.MethodPost, "http://"+n.tailnet.hostname+":4633/v1/forward", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := n.tailnet.HTTPClient().Do(req)
			if err != nil {
				last = err
				continue
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusServiceUnavailable {
				last = fmt.Errorf("%s: %s", n.name, bytes.TrimSpace(raw))
				continue
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s forward: %s %s", n.name, resp.Status, bytes.TrimSpace(raw))
			}
			var out raftv1.ForwardResponse
			if err := proto.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			if out.Error != "" {
				t.Fatalf("forward rejected: %s", out.Error)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("envelope never committed: %v", last)
}

func conversationRows(t *testing.T, n *node, gid group.ID, conv []byte) []cosl.Row {
	t.Helper()
	store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
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

func waitConversation(t *testing.T, n *node, gid group.ID, conv []byte, want int) []cosl.Row {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var rows []cosl.Row
	for time.Now().Before(deadline) {
		rows = conversationRows(t, n, gid, conv)
		if len(rows) >= want {
			return rows
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: %d conversation rows, want %d", n.name, len(rows), want)
	return nil
}

func waitMinted(t *testing.T, n *node, gid group.ID, conv []byte) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
		if err == nil {
			rows, err := store.Log(cosl.Query{})
			store.Close()
			if err == nil {
				keys, err := key.List(rows)
				if err != nil {
					t.Fatal(err)
				}
				for _, k := range keys {
					id := key.ConversationID(k.Curve, k.PublicKey)
					if bytes.Equal(id[:], conv) {
						return
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: Conversation %x was not minted", n.name, conv)
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

// An agent's message commits with its Handle and depth when this Node's
// config includes the Handle where it is sent and the depth is within
// max_depth. Anything else is refused on the socket, and nothing is
// appended.
func TestConversationSendAsAgent(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	writeConfig(t, a.paths.GroupConfigFile(group.ImplicitAlias), `
[agents.auditor]
model = "claude"

[agents.scribe]
model = "claude"

[agent]
profiles = ["auditor"]
max_depth = 2
`)
	ctx := context.Background()
	if _, err := a.client().ConversationSendAs(ctx, "looks fine", "auditor", 2, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		handle string
		depth  uint32
	}{
		{"auditor", 0},
		{"auditor", 3},
		{"scribe", 1}, // defined, not included
		{"nobody", 1}, // not defined
	} {
		if _, err := a.client().ConversationSendAs(ctx, "nope", c.handle, c.depth, nil); !errors.Is(err, serve.ErrBadSender) {
			t.Errorf("@%s at depth %d = %v, want ErrBadSender", c.handle, c.depth, err)
		}
	}
	// A Conversation that sets profiles replaces the Group's list.
	name := mintKey(t, a, b, gid)
	waitWorkspace(t, a, name)
	id := key.ConversationID("ecdsa", bytes.Repeat([]byte{0x33}, 33))
	writeConfig(t, a.paths.ConversationConfigFile(group.ImplicitAlias, name), "[agent]\nprofiles = []\n")
	if _, err := a.client().ConversationSendAs(ctx, "nope", "auditor", 1, id[:]); !errors.Is(err, serve.ErrBadSender) {
		t.Errorf("@auditor where the Conversation includes none = %v, want ErrBadSender", err)
	}
	// b's config defines no agents, so b cannot post as one.
	if _, err := b.client().ConversationSendAs(ctx, "nope", "auditor", 1, nil); !errors.Is(err, serve.ErrBadSender) {
		t.Errorf("@auditor on a Node without it = %v, want ErrBadSender", err)
	}

	if _, err := a.client().ConversationSend(ctx, "a person", nil); err != nil {
		t.Fatal(err)
	}
	rows := b.groupLevelRows(t, gid, 2)
	if len(rows) != 2 {
		t.Fatalf("%d group-level rows, want the agent's and the person's", len(rows))
	}
	if rows := conversationRows(t, a, gid, id[:]); len(rows) != 0 {
		t.Fatalf("refused agent message stored: %+v", rows)
	}
	for i, want := range []struct {
		handle string
		depth  uint32
	}{{"auditor", 2}, {"", 0}} {
		env, _ := rows[i].Decode()
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if ff := msg.(*coslv1.AppFreeform); ff.Handle != want.handle || ff.Depth != want.depth {
			t.Errorf("row %d: handle %q depth %d, want %q %d", i, ff.Handle, ff.Depth, want.handle, want.depth)
		}
	}
}
