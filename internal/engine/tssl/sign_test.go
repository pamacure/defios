package tssl

import (
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"defios.com/internal/session"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

// route delivers every engine's output to the others, the way the Session
// layer does over the COSL.
func route(t *testing.T, engines map[[32]byte]session.Engine) {
	t.Helper()
	for id, e := range engines {
		go func() {
			for o := range e.Out() {
				for to, peer := range engines {
					if to == id || (o.To != nil && *o.To != to) {
						continue
					}
					if err := peer.Receive(id, o.To == nil, o.Data); err != nil {
						t.Errorf("receive: %v", err)
					}
				}
			}
		}()
	}
}

func finished(t *testing.T, e session.Engine) session.Result {
	t.Helper()
	select {
	case res := <-e.Done():
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		return res
	case <-time.After(3 * time.Minute):
		t.Fatal("engine did not finish")
	}
	return session.Result{}
}

// A 2-of-3 Key signs two Digests with only two of its three Nodes; each
// signature is r||s||v and recovers to the Key's public key.
func TestSignerProducesRecoverableSignaturesFromTwoOfThree(t *testing.T) {
	if testing.Short() {
		t.Skip("generates Paillier preparams")
	}
	ids := [][32]byte{node(0, 1), node(0, 2), node(0, 3)}
	nonce := [32]byte{7}

	pre := make([][]byte, 3)
	var wg sync.WaitGroup
	for i := range pre {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if pre[i], err = KeepOrGenerate(nil, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	keygen := map[[32]byte]session.Engine{}
	for _, id := range ids {
		keygen[id] = NewECDSA(id)
	}
	for i, id := range ids {
		if err := keygen[id].Start(session.Params{
			SessionID: [32]byte{1}, Curve: "ecdsa", T: 2, N: 3, Participants: ids, Nonce: nonce, Preparams: pre[i],
		}); err != nil {
			t.Fatal(err)
		}
	}
	route(t, keygen)
	shares := map[[32]byte]session.Result{}
	for _, id := range ids {
		shares[id] = finished(t, keygen[id])
	}
	pub := shares[ids[0]].PublicKey

	signers := ids[:2]
	one, two := sha256.Sum256([]byte("one")), sha256.Sum256([]byte("two"))
	digests := [][]byte{one[:], two[:]}
	sign := map[[32]byte]session.Engine{}
	for _, id := range signers {
		sign[id] = NewSigner(id)
	}
	for _, id := range signers {
		if err := sign[id].Start(session.Params{
			SessionID: [32]byte{2}, Curve: "ecdsa", T: 2, N: 3, Participants: signers, Nonce: [32]byte{9},
			Digests: digests, SaveData: shares[id].SaveData,
		}); err != nil {
			t.Fatal(err)
		}
	}
	route(t, sign)
	for _, id := range signers {
		res := finished(t, sign[id])
		if len(res.Signatures) != len(digests) {
			t.Fatalf("got %d signatures, want %d", len(res.Signatures), len(digests))
		}
		for i, sig := range res.Signatures {
			if len(sig) != 65 {
				t.Fatalf("signature %d is %d bytes", i, len(sig))
			}
			compact := append([]byte{27 + 4 + sig[64]}, sig[:64]...)
			got, _, err := ecdsa.RecoverCompact(compact, digests[i])
			if err != nil {
				t.Fatal(err)
			}
			if string(got.SerializeCompressed()) != string(pub) {
				t.Fatalf("signature %d recovers to %x, want %x", i, got.SerializeCompressed(), pub)
			}
		}
	}
}

func TestSignerRefusesAMissingShare(t *testing.T) {
	for _, curve := range []string{"ecdsa", "eddsa"} {
		s := NewSigner(node(0, 1))
		if err := s.Start(session.Params{Curve: curve, T: 2, Participants: [][32]byte{node(0, 1), node(0, 2)}, Digests: [][]byte{{1}}}); err == nil {
			t.Fatalf("a missing %s share should be refused", curve)
		}
	}
	if err := NewSigner(node(0, 1)).Start(session.Params{Curve: "ed448", T: 2, Digests: [][]byte{{1}}}); err == nil {
		t.Fatal("an unknown curve should be refused")
	}
}

// A 2-of-3 EdDSA Key signs two messages of different lengths with two of its
// Nodes; each 64-byte signature verifies as RFC 8032 against the Key.
func TestSignerProducesEd25519SignaturesFromTwoOfThree(t *testing.T) {
	ids := [][32]byte{node(0, 1), node(0, 2), node(0, 3)}
	keygen := map[[32]byte]session.Engine{}
	for _, id := range ids {
		keygen[id] = NewEdDSA(id)
	}
	for _, id := range ids {
		if err := keygen[id].Start(session.Params{SessionID: [32]byte{1}, Curve: "eddsa", T: 2, N: 3, Participants: ids, Nonce: [32]byte{7}}); err != nil {
			t.Fatal(err)
		}
	}
	route(t, keygen)
	shares := map[[32]byte]session.Result{}
	for _, id := range ids {
		shares[id] = finished(t, keygen[id])
	}
	pub := shares[ids[0]].PublicKey

	signers := ids[1:]
	// The second message is long and the first byte of the first is 0x01 like a
	// Solana message; a leading 0x00 must survive too.
	msgs := [][]byte{append([]byte{1}, make([]byte, 200)...), append([]byte{0, 0, 5}, "zero-led"...)}
	sign := map[[32]byte]session.Engine{}
	for _, id := range signers {
		sign[id] = NewSigner(id)
		if err := sign[id].Start(session.Params{
			SessionID: [32]byte{2}, Curve: "eddsa", T: 2, N: 3, Participants: signers, Nonce: [32]byte{9},
			Digests: msgs, SaveData: shares[id].SaveData,
		}); err != nil {
			t.Fatal(err)
		}
	}
	route(t, sign)
	for _, id := range signers {
		res := finished(t, sign[id])
		if len(res.Signatures) != len(msgs) {
			t.Fatalf("got %d signatures, want %d", len(res.Signatures), len(msgs))
		}
		for i, sig := range res.Signatures {
			if !session.VerifyEd25519(pub, msgs[i], sig) {
				t.Fatalf("signature %d does not verify against the Key", i)
			}
		}
	}
}
