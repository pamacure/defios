package tssl

import "testing"

func TestExistingPreparamsAreNotRegenerated(t *testing.T) {
	existing := []byte(`{"kept":true}`)
	var logged bool
	got, err := KeepOrGenerate(existing, func(string, ...any) { logged = true })
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(existing) {
		t.Fatalf("preparams = %s", got)
	}
	if logged {
		t.Fatal("regenerated existing preparams")
	}
	if &got[0] == &existing[0] {
		t.Fatal("returned the caller's buffer")
	}
}
