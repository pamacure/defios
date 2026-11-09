package session_test

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/session"
	coslv1 "defios.com/proto/cosl/v1"
)

// proposalRows is a replica's rows after a Session starts with start's fields
// and, unless it is a signing Session, both Nodes complete it.
func proposalRows(t *testing.T, start *coslv1.SessionStart) ([]cosl.Row, session.ID) {
	t.Helper()
	alice, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(
		group.RosterNode{Public: alice.Public, DisplayName: "alice", TsnetHostname: "defios-alice"},
		group.RosterNode{Public: bob.Public, DisplayName: "bob", TsnetHostname: "defios-bob"},
	)
	store, err := cosl.Open(t.TempDir() + "/cosl.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fsm := cosl.NewFSM(store, roster)
	var seq uint64
	apply := func(n *identity.Node, typ uint32, payload proto.Message) {
		t.Helper()
		signed, err := (cosl.Author{Node: n, GroupID: roster.GroupID()}).Sign(typ, nil, payload)
		if err != nil {
			t.Fatal(err)
		}
		seq++
		if res, err := fsm.Apply(seq, 1, signed.Bytes); err != nil || !res.Inserted {
			t.Fatalf("apply: %v %+v", err, res)
		}
	}
	id := session.ID{0xaa}
	start.SessionId, start.Curve, start.T, start.N = id[:], "ecdsa", 2, 2
	start.Participants = [][]byte{roster.NodeIDs()[0][:], roster.NodeIDs()[1][:]}
	apply(alice, cosl.TypeSessionStart, start)
	if len(start.SignRequestId) == 0 {
		done := &coslv1.SessionComplete{SessionId: id[:], Curve: "ecdsa", PublicKey: []byte{2, 1, 1}}
		apply(alice, cosl.TypeSessionComplete, done)
		apply(bob, cosl.TypeSessionComplete, done)
	}
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	return rows, id
}

// A valid proposal is read in the NFC form an alias is stored in, by the
// listings and by whoever reads the minted Key.
func TestAValidProposalIsReadOffTheStart(t *testing.T) {
	rows, id := proposalRows(t, &coslv1.SessionStart{ProposedAlias: []byte("Café")})
	const want = "Café"
	list, err := session.List(rows)
	if err != nil || len(list) != 1 || list[0].ProposedAlias != want {
		t.Fatalf("List = %+v, %v", list, err)
	}
	detail, err := session.Inspect(rows, id)
	if err != nil || detail.ProposedAlias != want {
		t.Fatalf("Inspect = %+v, %v", detail, err)
	}
	minted, err := session.CompletedSessions(rows)
	if err != nil || len(minted) != 1 || minted[0].ProposedAlias != want || minted[0].ProposalInvalid {
		t.Fatalf("CompletedSessions = %+v, %v", minted, err)
	}
}

// A proposal comes from another Node, so one that could not be an alias is
// absent from everything shown, and the mint only learns that it was bad.
func TestAnInvalidProposalIsTreatedAsAbsent(t *testing.T) {
	for _, bad := range []string{"\x1b[31mcold", "\xffcold", " cold", strings.Repeat("x", 64)} {
		rows, id := proposalRows(t, &coslv1.SessionStart{ProposedAlias: []byte(bad)})
		list, err := session.List(rows)
		if err != nil || len(list) != 1 || list[0].ProposedAlias != "" {
			t.Fatalf("%q: List = %+v, %v", bad, list, err)
		}
		detail, err := session.Inspect(rows, id)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "proposed_alias") {
			t.Fatalf("%q: inspect JSON shows it: %s", bad, raw)
		}
		minted, err := session.CompletedSessions(rows)
		if err != nil || len(minted) != 1 || minted[0].ProposedAlias != "" || !minted[0].ProposalInvalid {
			t.Fatalf("%q: CompletedSessions = %+v, %v", bad, minted, err)
		}
	}
}

// With no proposal there is nothing to show and nothing to warn of.
func TestNoProposalIsNotAnInvalidOne(t *testing.T) {
	rows, _ := proposalRows(t, &coslv1.SessionStart{})
	minted, err := session.CompletedSessions(rows)
	if err != nil || len(minted) != 1 || minted[0].ProposedAlias != "" || minted[0].ProposalInvalid {
		t.Fatalf("CompletedSessions = %+v, %v", minted, err)
	}
}

// A signing Session mints no Key, so a proposal on one means nothing.
func TestASigningSessionShowsNoProposal(t *testing.T) {
	rows, _ := proposalRows(t, &coslv1.SessionStart{ProposedAlias: []byte("cold"), SignRequestId: make([]byte, 32)})
	list, err := session.List(rows)
	if err != nil || len(list) != 1 || list[0].ProposedAlias != "" {
		t.Fatalf("List = %+v, %v", list, err)
	}
}
