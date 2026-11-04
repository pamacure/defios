package cli

import (
	"errors"
	"testing"
)

// A prompt asked twice refuses two entries that differ, as a usage error.
func TestAskTwiceRefusesEntriesThatDiffer(t *testing.T) {
	entries := map[string]string{"New passphrase: ": "one", "Confirm new passphrase: ": "two"}
	ask := func(label string) (string, error) { return entries[label], nil }

	_, err := askTwice(ask, "New passphrase: ", "Confirm new passphrase: ")
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != ExitUsage {
		t.Fatalf("err %v, want a usage error", err)
	}

	entries["Confirm new passphrase: "] = "one"
	if got, err := askTwice(ask, "New passphrase: ", "Confirm new passphrase: "); err != nil || got != "one" {
		t.Fatalf("got %q, %v; want the matching entry", got, err)
	}
}
