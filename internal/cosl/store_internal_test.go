package cosl

import (
	"path/filepath"
	"testing"
)

// PRAGMAs are per connection, so only the Store's own pool can show they
// took effect (docs/spec/mvp.md §5.5).
func TestStoreConnectionsCarrySpecPragmas(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var journal string
	var synchronous, busy int
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Errorf("journal_mode = %q (%v), want wal", journal, err)
	}
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Errorf("synchronous = %d (%v), want 2 (FULL)", synchronous, err)
	}
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil || busy != 5000 {
		t.Errorf("busy_timeout = %d (%v), want 5000", busy, err)
	}
}
