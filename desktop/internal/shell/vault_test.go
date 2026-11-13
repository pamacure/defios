package shell_test

import (
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
)

// The "Back up now" banner shows once a keygen completes, stays across a
// restart, and clears once a Backup is taken from the app.
func TestTheBackupBannerShowsAfterAKeyIsMintedUntilABackup(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	if s.State().BackupDue {
		t.Fatal("a fresh app asks for a Backup before any Key")
	}

	ts := s.NewKey("ecdsa", 2, "treasury")
	if ts.Problem != nil || len(ts.Keygens) != 1 {
		t.Fatalf("new key: %+v", ts)
	}
	r.node.mint(ts.Keygens[0].SessionID)
	eventually(t, s, func(st shell.State) bool { return st.BackupDue })
	if p, _ := r.prefs.Load(); !p.BackupDue {
		t.Fatalf("prefs after a mint: %+v", p)
	}

	// A restart keeps it.
	s.Close()
	s = served(t, r)
	if !s.State().BackupDue {
		t.Fatal("the banner did not survive a restart")
	}

	before := time.Now()
	v := s.BackupTo("/backups/new.backup")
	if v.Problem != nil || v.Done != "backup" {
		t.Fatalf("backup: %+v", v)
	}
	if got := r.node.backups(); len(got) != 1 || got[0] != "/backups/new.backup" {
		t.Fatalf("backups written: %v", got)
	}
	if s.State().BackupDue {
		t.Fatal("the banner stays after a Backup")
	}
	p, _ := r.prefs.Load()
	if p.BackupDue || p.LastBackup == nil || p.LastBackup.Before(before) {
		t.Fatalf("prefs after a Backup: %+v", p)
	}
	if v.LastBackup == nil || !v.LastBackup.Equal(*p.LastBackup) {
		t.Fatalf("the page's last Backup %v, prefs' %v", v.LastBackup, p.LastBackup)
	}
}

// A Backup that fails keeps the banner and says why.
func TestAFailedBackupKeepsTheBanner(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	ts := s.NewKey("ecdsa", 2, "")
	r.node.mint(ts.Keygens[0].SessionID)
	eventually(t, s, func(st shell.State) bool { return st.BackupDue })

	v := s.BackupTo(fakeExistingFile)
	if v.Problem == nil || v.Problem.Code != "file-exists" || v.Where != "backup" || v.Done != "" {
		t.Fatalf("backup over a file: %+v", v)
	}
	if !s.State().BackupDue {
		t.Fatal("a failed Backup cleared the banner")
	}
	if p, _ := r.prefs.Load(); p.LastBackup != nil {
		t.Fatalf("a failed Backup kept a time: %v", p.LastBackup)
	}
}

// A Session that is not a keygen completing, or one aborted, asks for no
// Backup.
func TestOnlyACompletedKeygenAsksForABackup(t *testing.T) {
	r := newRig(t, "alpha")
	s := served(t, r)
	ts := s.NewKey("ecdsa", 2, "")
	s.AbortKeygen(ts.Keygens[0].SessionID, "no")
	r.node.signed()
	time.Sleep(50 * time.Millisecond)
	if s.State().BackupDue {
		t.Fatal("an aborted keygen or a signing Session asked for a Backup")
	}
}

// Reveal with no Group served says why, and shows no value.
func TestRevealWhileLockedSaysWhy(t *testing.T) {
	s := newRig(t, "alpha").start(false)
	if r := s.RevealSecret("RPC_KEY", pw); r.Problem == nil || r.Value != "" {
		t.Fatalf("reveal while locked: %+v", r)
	}
}
