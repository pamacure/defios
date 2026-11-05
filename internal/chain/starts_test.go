package chain_test

import (
	"slices"
	"testing"

	"defios.com/internal/chain"
	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	"defios.com/internal/talk"
)

func TestSummonsAreThisNodesPersonsMentions(t *testing.T) {
	me, other := identity.ID{1}, identity.ID{2}
	msg := func(id byte, sender identity.ID, handle, text string) talk.Message {
		return talk.Message{Row: cosl.Row{Sender: sender, MsgID: [32]byte{id}}, Handle: handle, Text: text}
	}
	pruned := msg(5, me, "", "")
	pruned.Pruned = true
	unreadable := msg(6, me, "", "")
	unreadable.Unreadable = true
	for _, tc := range []struct {
		name string
		m    talk.Message
		want []string
	}{
		{"a person's mentions, in order", msg(1, me, "", "@a then @b"), []string{"a", "b"}},
		{"no mentions", msg(2, me, "", "nobody here"), nil},
		{"an agent's summons its own chain's", msg(3, me, "a", "over to @b"), nil},
		{"another Node's mention is its own", msg(4, other, "", "@a from there"), nil},
		{"pruned", pruned, nil},
		{"unreadable", unreadable, nil},
	} {
		if got := chain.Summons(tc.m, me); !slices.Equal(got, tc.want) {
			t.Errorf("%s: Summons = %q, want %q", tc.name, got, tc.want)
		}
	}
}
