package cosl_test

import (
	"bytes"
	"strings"
	"testing"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

// Author signs a Group-level envelope the FSM accepts; msg_id is random.
func TestAuthorProducesEnvelopeTheFSMAccepts(t *testing.T) {
	f := newFixture(t)
	a := cosl.Author{Node: f.nodes[0], GroupID: f.roster.GroupID()}
	m1, err := a.Sign(cosl.TypeAppFreeform, nil, freeform("hi"))
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := a.Sign(cosl.TypeAppFreeform, nil, freeform("hi"))
	if m1.MsgID == m2.MsgID {
		t.Fatal("two signings share a msg_id")
	}
	if res := f.apply(1, 1, m1.Bytes); !res.Inserted {
		t.Fatalf("not inserted: %s", res.Reason)
	}
	rows := f.rows()
	if len(rows) != 1 || rows[0].MsgID != m1.MsgID || rows[0].ConversationID != nil || rows[0].Sender != f.nodes[0].NodeID {
		t.Fatalf("rows = %+v", rows)
	}
	has, err := f.store.Has(m1.MsgID)
	if err != nil || !has {
		t.Fatalf("Has(committed) = %v, %v", has, err)
	}
	has, err = f.store.Has(m2.MsgID)
	if err != nil || has {
		t.Fatalf("Has(uncommitted) = %v, %v", has, err)
	}

	// A Key-scoped envelope carries the conversation id.
	conv := bytes.Repeat([]byte{7}, 32)
	m3, _ := a.Sign(cosl.TypeAppFreeform, conv, freeform("scoped"))
	f.apply(2, 1, m3.Bytes)
	if rows := f.rows(); !bytes.Equal(rows[1].ConversationID, conv) {
		t.Fatalf("conversation id = %x", rows[1].ConversationID)
	}
}

// membership.join is self-asserted: the keys it carries are the sender's
// pinned keys. It adds no one (§5.4).
func TestMembershipJoinMustBeSelfAsserted(t *testing.T) {
	f := newFixture(t)
	alice, bob := f.nodes[0], f.nodes[1]
	a := cosl.Author{Node: alice, GroupID: f.roster.GroupID()}

	good, _ := a.Sign(cosl.TypeMembershipJoin, nil, &coslv1.MembershipJoin{
		DisplayName: "alice", TsnetHostname: "defios-alice",
		EncryptionPublicKey: alice.EncryptionPublicKey, AuthPublicKey: alice.AuthPublicKey,
	})
	if res := f.apply(1, 1, good.Bytes); !res.Inserted {
		t.Fatalf("own join rejected: %s", res.Reason)
	}

	// alice signs a join carrying bob's keys.
	bad, _ := a.Sign(cosl.TypeMembershipJoin, nil, &coslv1.MembershipJoin{
		DisplayName: "bob", TsnetHostname: "defios-bob",
		EncryptionPublicKey: bob.EncryptionPublicKey, AuthPublicKey: bob.AuthPublicKey,
	})
	if res := f.apply(2, 1, bad.Bytes); res.Inserted || !strings.Contains(res.Reason, "self-asserted") {
		t.Fatalf("join for another Node: %+v", res)
	}

	// An unparseable join payload is rejected too.
	env := f.envelope(0, cosl.TypeMembershipJoin, freeform("x"))
	env.Payload = []byte{0xff, 0xff}
	if res := f.apply(3, 1, f.signed(0, env)); res.Inserted {
		t.Fatal("garbage join inserted")
	}
	if n := len(f.rows()); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

// Author.Sign rejects a conversation id that is not 32 bytes.
func TestAuthorRejectsBadConversationID(t *testing.T) {
	f := newFixture(t)
	a := cosl.Author{Node: f.nodes[0], GroupID: f.roster.GroupID()}
	if _, err := a.Sign(cosl.TypeAppFreeform, []byte{1, 2}, freeform("x")); err == nil {
		t.Fatal("short conversation id accepted")
	}
}
