package coslbench_test

import (
	"bytes"
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/cosl/coslbench"
	"defios.com/internal/key"
	"defios.com/internal/session"
)

// A built replica reads as the Group it describes: every Key minted, the
// signing Key's Conversation where its talk is, and a signature that
// verifies under that Key.
func TestBuiltReplicaReadsAsItsShape(t *testing.T) {
	shape := coslbench.Shapes[0]
	r := coslbench.Build(t, shape)
	store, err := cosl.Open(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := key.List(rows)
	if err != nil || len(keys) != shape.ECDSAKeys+shape.EdDSAKeys {
		t.Fatalf("%d Keys, %v; want %d", len(keys), err, shape.ECDSAKeys+shape.EdDSAKeys)
	}
	if id := key.ConversationID("ecdsa", r.SignKey); !bytes.Equal(id[:], r.Conversation) || !bytes.Equal(keys[0].PublicKey, r.SignKey) {
		t.Fatalf("signing Key %x / Conversation %x do not match key's", keys[0].PublicKey, r.Conversation)
	}
	if !session.VerifyECDSA(r.SignKey, r.Digest, r.Signature) {
		t.Fatal("the fixture signature does not verify")
	}
	sums, err := session.List(rows, session.WithSignInfo(func([32]byte) (session.SignInfo, error) {
		return session.SignInfo{Curve: "ecdsa", PublicKey: r.SignKey, T: 2, Digests: [][]byte{r.Digest[:]}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	complete := 0
	for _, s := range sums {
		if s.Status == "complete" {
			complete++
		} else {
			t.Errorf("Session %s is %s, want complete", s.SessionID, s.Status)
		}
	}
	if want := shape.ECDSAKeys + shape.EdDSAKeys + shape.Signings; complete != want {
		t.Fatalf("%d complete Sessions, want %d", complete, want)
	}
	scoped, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}, ConversationID: r.Conversation})
	if err != nil || len(scoped) != shape.Talk/2 {
		t.Fatalf("%d messages in the signing Key's Conversation, %v; want %d", len(scoped), err, shape.Talk/2)
	}
	completes, _ := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeSessionComplete}})
	if want := 2 * (shape.ECDSAKeys + shape.EdDSAKeys + shape.Signings); len(completes) != want {
		t.Fatalf("%d session.complete rows, want %d", len(completes), want)
	}
}
