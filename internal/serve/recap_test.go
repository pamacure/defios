package serve_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/recap"
	"defios.com/internal/serve"
)

// A Recap written on one Node folds identically on both; a Recap the fold
// would skip is refused with the recap sentinel over the socket, and leaves
// no row.
func TestConversationRecapFoldsOnBothNodes(t *testing.T) {
	a, b := pair(t)
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
	// The writer pre-checks against its own replica, so b must have applied
	// the talk it recaps.
	b.groupLevelRows(t, gid, len(sent))
	if _, err := b.client().ConversationRecap(ctx, "one and two", sent[1].Seq, nil); err != nil {
		t.Fatal(err)
	}
	want := func(v recap.View) bool {
		return v.Live != nil && v.Live.Text == "one and two" && v.Live.Covers == 2 && len(v.Talk) == 1 && v.Talk[0].Seq == sent[2].Seq
	}
	va, vb := waitRecap(t, a, want), waitRecap(t, b, want)
	if va.Live.Row.MsgID != vb.Live.Row.MsgID || va.Live.Row.Sender != b.id.NodeID {
		t.Fatalf("views differ: %+v / %+v", va.Live, vb.Live)
	}

	if _, err := a.client().ConversationRecap(ctx, "stale", sent[0].Seq, nil); !errors.Is(err, recap.ErrStale) {
		t.Fatalf("stale recap: %v", err)
	}
	if _, err := a.client().ConversationRecap(ctx, "the live recap", va.Live.Row.Seq, nil); !errors.Is(err, recap.ErrBadBase) {
		t.Fatalf("base not talk: %v", err)
	}
	if _, err := a.client().ConversationRecap(ctx, "", sent[2].Seq, nil); !errors.Is(err, cosl.ErrBadText) {
		t.Fatalf("empty text: %v", err)
	}
	if _, err := a.client().ConversationRecap(ctx, "unminted", sent[2].Seq, bytes.Repeat([]byte{0x11}, 32)); !errors.Is(err, serve.ErrNoConversation) {
		t.Fatalf("unminted conversation: %v", err)
	}
	if got := recapRows(t, a); got != 1 {
		t.Fatalf("%d app.recap rows, want 1", got)
	}

	if _, err := a.client().ConversationRecap(ctx, "all three", sent[2].Seq, nil); err != nil {
		t.Fatal(err)
	}
	waitRecap(t, b, func(v recap.View) bool { return v.Live != nil && v.Live.Covers == 3 && len(v.Talk) == 0 })
}

// waitRecap polls the Node's Group-level fold until ok.
func waitRecap(t *testing.T, n *node, ok func(recap.View) bool) recap.View {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last recap.View
	for time.Now().Before(deadline) {
		store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
		if err == nil {
			rows, err := store.Log(cosl.Query{GroupLevel: true})
			store.Close()
			if v, ferr := recap.Fold(rows); err == nil && ferr == nil {
				if ok(v) {
					return v
				}
				last = v
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: fold never matched; last live %+v, %d talk", n.name, last.Live, len(last.Talk))
	return recap.View{}
}

func recapRows(t *testing.T, n *node) int {
	t.Helper()
	store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	typ := cosl.TypeAppRecap
	rows, err := store.Log(cosl.Query{MsgType: &typ})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}
