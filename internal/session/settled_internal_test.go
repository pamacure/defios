package session

import (
	"slices"
	"testing"

	"defios.com/internal/cosl/coslbench"
)

// settledService is alice's Session layer over the small benchmark replica,
// whose ten signing Sessions each completed with a valid signature. key is
// the public key its SignInfo lookup hands out: flipping a byte of it makes
// every signature fail to verify from then on, and flipping it back makes
// them verify again.
func settledService(t *testing.T) (svc *Service, key []byte) {
	t.Helper()
	svc, _, _, _ = benchService(t, coslbench.Shapes[0])
	info, err := svc.cfg.SignInfo([32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	info.PublicKey = slices.Clone(info.PublicKey)
	svc.cfg.SignInfo = func([32]byte) (SignInfo, error) { return info, nil }
	return svc, info.PublicKey
}

func completedSignings(t *testing.T, svc *Service) int {
	t.Helper()
	recs, err := svc.records()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range recs {
		if r.isSign && r.status == Complete {
			n++
		}
	}
	return n
}

// Whether a session.complete's signatures verify cannot change once the row
// is committed, so a wake after the first never checks them again: a key
// that no longer matches leaves a completed Session complete.
func TestCompletedSigningSessionIsNotReCheckedOnLaterWakes(t *testing.T) {
	svc, key := settledService(t)
	if got := completedSignings(t, svc); got != 10 {
		t.Fatalf("first wake: %d completed signing Sessions, want 10", got)
	}
	key[5] ^= 1
	if got := completedSignings(t, svc); got != 10 {
		t.Fatalf("later wake: %d completed signing Sessions, want 10", got)
	}
}

// A complete whose signatures do not verify never completes its Session, and
// is not checked again either.
func TestCompleteThatDoesNotVerifyIsNotReChecked(t *testing.T) {
	svc, key := settledService(t)
	key[5] ^= 1
	if got := completedSignings(t, svc); got != 0 {
		t.Fatalf("first wake: %d completed signing Sessions, want 0", got)
	}
	key[5] ^= 1
	if got := completedSignings(t, svc); got != 0 {
		t.Fatalf("later wake: %d completed signing Sessions, want 0", got)
	}
}
