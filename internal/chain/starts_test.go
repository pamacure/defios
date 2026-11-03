package chain_test

import (
	"encoding/hex"
	"slices"
	"testing"

	"defios.com/internal/chain"
	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	"defios.com/internal/talk"
)

func TestStartsAreThisNodesPersonsMessagesWithMentions(t *testing.T) {
	me, other := identity.ID{1}, identity.ID{2}
	msg := func(id byte, sender identity.ID, handle, text string) talk.Message {
		return talk.Message{Row: cosl.Row{Sender: sender, MsgID: [32]byte{id}}, Handle: handle, Text: text}
	}
	pruned := msg(5, me, "", "")
	pruned.Pruned = true
	unreadable := msg(6, me, "", "")
	unreadable.Unreadable = true
	got := chain.Starts([]talk.Message{
		msg(1, me, "", "@a then @b"),
		msg(2, me, "", "nobody here"),
		msg(3, me, "a", "over to @b"),      // an agent's summons its own chain's
		msg(4, other, "", "@a from there"), // another Node's mention is its own
		pruned,
		unreadable,
		msg(7, me, "", "and @c"),
	}, me)
	id := func(b byte) string { return hex.EncodeToString(append([]byte{b}, make([]byte, 31)...)) }
	want := []chain.Start{
		{MsgID: id(1), Handles: []string{"a", "b"}},
		{MsgID: id(7), Handles: []string{"c"}},
	}
	if !slices.EqualFunc(got, want, func(a, b chain.Start) bool {
		return a.MsgID == b.MsgID && slices.Equal(a.Handles, b.Handles)
	}) {
		t.Fatalf("Starts = %+v, want %+v", got, want)
	}
}
