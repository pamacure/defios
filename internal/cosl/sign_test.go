package cosl_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func sampleEnvelope() *coslv1.Envelope {
	return &coslv1.Envelope{
		Version:   1,
		GroupId:   bytes.Repeat([]byte{0x60}, 32),
		MsgType:   cosl.TypeAppFreeform,
		Sender:    bytes.Repeat([]byte{0x0a}, 32),
		MsgId:     bytes.Repeat([]byte{0x0b}, 32),
		CreatedAt: 1700000000_000000000,
		Payload:   []byte("payload"),
	}
}

func TestSignProduces64ByteSignatureThatVerifies(t *testing.T) {
	pub, priv := newKey(t)
	env := sampleEnvelope()
	signed, err := cosl.Sign(env, priv)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Sig) != ed25519.SignatureSize {
		t.Fatalf("sig is %d bytes, want %d", len(env.Sig), ed25519.SignatureSize)
	}
	var back coslv1.Envelope
	if err := proto.Unmarshal(signed, &back); err != nil {
		t.Fatal(err)
	}
	if err := cosl.Verify(&back, pub); err != nil {
		t.Fatalf("Verify after round trip: %v", err)
	}
}

func TestVerifyRejectsTamperedField(t *testing.T) {
	pub, priv := newKey(t)
	env := sampleEnvelope()
	if _, err := cosl.Sign(env, priv); err != nil {
		t.Fatal(err)
	}
	env.Payload = []byte("Payload")
	if err := cosl.Verify(env, pub); err == nil {
		t.Fatal("Verify accepted a tampered payload")
	}
}

func TestVerifyRejectsWrongKeyAndMalformedSig(t *testing.T) {
	_, priv := newKey(t)
	other, _ := newKey(t)
	env := sampleEnvelope()
	if _, err := cosl.Sign(env, priv); err != nil {
		t.Fatal(err)
	}
	if err := cosl.Verify(env, other); err == nil {
		t.Fatal("Verify accepted a signature under another Node's key")
	}
	env.Sig = env.Sig[:63]
	if err := cosl.Verify(env, other); err == nil {
		t.Fatal("Verify accepted a 63-byte sig")
	}
}

func TestSignatureCoversMsgIDAndIsDeterministic(t *testing.T) {
	_, priv := newKey(t)
	a, b := sampleEnvelope(), sampleEnvelope()
	sa, _ := cosl.Sign(a, priv)
	sb, _ := cosl.Sign(b, priv)
	if !bytes.Equal(sa, sb) {
		t.Fatal("same envelope signed twice gave different bytes")
	}
	b.MsgId[0] ^= 1
	sb2, _ := cosl.Sign(b, priv)
	if bytes.Equal(a.Sig, b.Sig) || bytes.Equal(sa, sb2) {
		t.Fatal("changing msg_id did not change the signature")
	}
}

func TestRegistryNames(t *testing.T) {
	cases := map[uint32]string{
		1: "membership.join", 2: "session.start", 3: "session.abort",
		4: "session.complete", 5: "tss.session", 6: "app.freeform",
	}
	for id, name := range cases {
		if got := cosl.TypeName(id); got != name {
			t.Errorf("TypeName(%d) = %q, want %q", id, got, name)
		}
		if got, ok := cosl.TypeByName(name); !ok || got != id {
			t.Errorf("TypeByName(%q) = %d,%v want %d", name, got, ok, id)
		}
	}
	if got := cosl.TypeName(99); got != "unknown(99)" {
		t.Errorf("TypeName(99) = %q", got)
	}
	if _, ok := cosl.TypeByName("nope"); ok {
		t.Error("TypeByName accepted an unknown name")
	}
}
