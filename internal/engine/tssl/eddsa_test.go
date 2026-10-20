package tssl_test

import (
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/edwards/v2"

	"defios.com/internal/engine/tssl"
	"defios.com/internal/session"
)

// TestEdDSAKeygenAgreesOnCanonicalPublicKey runs a 2-of-2 tss-lib EdDSA
// keygen through KeygenEngine. Both parties must finish on the same
// 32-byte RFC 8032 public key, with no ECDSA preparams.
func TestEdDSAKeygenAgreesOnCanonicalPublicKey(t *testing.T) {
	aliceID := id(0x01)
	bobID := id(0x02)
	var nonce [32]byte
	nonce[31] = 7
	alice := tssl.NewEdDSA(aliceID)
	bob := tssl.NewEdDSA(bobID)
	params := session.Params{
		Curve:        "eddsa",
		T:            2,
		N:            2,
		Participants: [][32]byte{bobID, aliceID},
		Nonce:        nonce,
	}
	if err := alice.Start(params); err != nil {
		t.Fatal(err)
	}
	if err := bob.Start(params); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 8)
	go relay(aliceID, alice, bob, errCh)
	go relay(bobID, bob, alice, errCh)

	a := waitResult(t, alice, errCh)
	b := waitResult(t, bob, errCh)
	if a.Err != nil || b.Err != nil {
		t.Fatalf("alice %v bob %v", a.Err, b.Err)
	}
	if len(a.PublicKey) != 32 || len(b.PublicKey) != 32 {
		t.Fatalf("len alice %d bob %d", len(a.PublicKey), len(b.PublicKey))
	}
	if string(a.PublicKey) != string(b.PublicKey) {
		t.Fatalf("public keys differ: %x %x", a.PublicKey, b.PublicKey)
	}
	if _, err := edwards.ParsePubKey(a.PublicKey); err != nil {
		t.Fatalf("public key is not RFC 8032: %v (%x)", err, a.PublicKey)
	}
	if len(a.SaveData) == 0 || len(b.SaveData) == 0 {
		t.Fatal("share save data is empty")
	}
}

func TestEdDSAAdapterRejectsECDSA(t *testing.T) {
	eng := tssl.NewEdDSA(id(0x01))
	err := eng.Start(session.Params{Curve: "ecdsa", T: 2, N: 2})
	if err == nil {
		t.Fatal("eddsa adapter accepted an ecdsa session")
	}
}

func id(b byte) [32]byte {
	var out [32]byte
	out[31] = b
	return out
}

func relay(from [32]byte, src, dst session.Engine, errCh chan error) {
	for o := range src.Out() {
		broadcast := o.To == nil
		if !broadcast && *o.To == from {
			continue
		}
		msg := append([]byte(nil), o.Data...)
		go func() {
			if err := dst.Receive(from, broadcast, msg); err != nil {
				errCh <- err
			}
		}()
	}
}

func waitResult(t *testing.T, eng session.Engine, errCh <-chan error) session.Result {
	t.Helper()
	select {
	case res := <-eng.Done():
		return res
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(60 * time.Second):
		t.Fatal("keygen timed out")
	}
	return session.Result{}
}
