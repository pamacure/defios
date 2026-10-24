package serve

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"defios.com/internal/session"
)

// A late accept is a strong error on the wire: 409 with its own kind, and the
// client gets the same sentinel back, distinct from "not a participant".
func TestSignerSetFullRoundTrips(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, fmt.Errorf("accept: %w", session.ErrSignerSetFull))
	resp := rec.Result()
	if resp.StatusCode != http.StatusConflict || resp.Header.Get(errorKindHeader) != "signer-set-full" {
		t.Fatalf("status %d kind %q", resp.StatusCode, resp.Header.Get(errorKindHeader))
	}
	err := localError("/sign/accept", resp)
	if !errors.Is(err, session.ErrSignerSetFull) || errors.Is(err, session.ErrNotParticipant) {
		t.Fatalf("client error = %v", err)
	}
}
