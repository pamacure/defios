package session

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

// pendingFile is the share written to the Vault before session.complete
// is durable. A restart that aborts the Session deletes it.
type pendingFile struct {
	Shares []pendingRow `json:"shares"`
}

type pendingRow struct {
	SessionID string `json:"session_id"`
	Curve     string `json:"curve"`
	PublicKey string `json:"public_key"`
}

func loadPending(path string) (map[ID]shareRef, error) {
	out := map[ID]shareRef{}
	if path == "" {
		return out, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	var file pendingFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("session: pending shares: %w", err)
	}
	for _, row := range file.Shares {
		id, err := ParseID(row.SessionID)
		if err != nil {
			return nil, err
		}
		pk, err := hex.DecodeString(row.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("session: pending share: %w", err)
		}
		out[id] = shareRef{Curve: row.Curve, PublicKey: pk}
	}
	return out, nil
}

func (s *Service) rememberShare(id ID, curve string, publicKey []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[id] = shareRef{Curve: curve, PublicKey: append([]byte(nil), publicKey...)}
	return s.saveHeldLocked()
}

// dropShare deletes the Vault share for a Session that will not mint.
// The marker stays until the delete lands, so a retry still finds it.
func (s *Service) dropShare(id ID) error {
	s.mu.Lock()
	ref, ok := s.held[id]
	store := s.cfg.Shares
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if store != nil {
		if err := store.Delete(ref.Curve, ref.PublicKey); err != nil {
			return fmt.Errorf("deleting share %s: %w", id, err)
		}
	}
	s.forgetShare(id)
	return nil
}

// forgetShare drops the pending marker and leaves the Vault share in place.
// That is the minted Key.
func (s *Service) forgetShare(id ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.held[id]; !ok {
		return
	}
	delete(s.held, id)
	if err := s.saveHeldLocked(); err != nil {
		s.cfg.Logf("session: %v", err)
	}
}

func (s *Service) saveHeldLocked() error {
	if s.cfg.PendingPath == "" {
		return nil
	}
	file := pendingFile{}
	for id, ref := range s.held {
		file.Shares = append(file.Shares, pendingRow{
			SessionID: id.String(),
			Curve:     ref.Curve,
			PublicKey: hex.EncodeToString(ref.PublicKey),
		})
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := os.WriteFile(s.cfg.PendingPath, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}

// mint writes the share, then appends session.complete. Either step is
// retried on the next wake until both have landed; the engine is not
// started again.
func (s *Service) mint(ctx context.Context, r *rec, p *party, res Result) {
	if s.cfg.Shares == nil {
		s.giveUp(ctx, r.id, "session: no vault")
		return
	}
	if len(res.PublicKey) == 0 {
		s.giveUp(ctx, r.id, "session: keygen finished without a public key")
		return
	}
	if err := s.rememberShare(r.id, r.curve, res.PublicKey); err != nil {
		s.failWake(fmt.Errorf("share %s: %w", r.id, err))
		return
	}
	if err := s.cfg.Shares.Put(r.curve, res.PublicKey, res.SaveData); err != nil {
		s.failWake(fmt.Errorf("share %s: %w", r.id, err))
		return
	}
	err := s.signAndAppend(ctx, cosl.TypeSessionComplete, &coslv1.SessionComplete{
		SessionId: append([]byte(nil), r.id[:]...),
		Curve:     r.curve,
		PublicKey: append([]byte(nil), res.PublicKey...),
	})
	if err != nil {
		s.failWake(fmt.Errorf("complete %s: %w", r.id, err))
		return
	}
	p.minted = true
}
