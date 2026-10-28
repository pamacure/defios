package mention_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"defios.com/internal/mention"
)

// vectors are ADR 0016's conformance vectors, shared with every client that
// finds mentions.
type vectors struct {
	Mentions []struct {
		Text     string   `json:"text"`
		Mentions []string `json:"mentions"`
		Why      string   `json:"why"`
	} `json:"mentions"`
	Handles struct {
		Valid   []string `json:"valid"`
		Invalid []string `json:"invalid"`
	} `json:"handles"`
}

func readVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../docs/mentions/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Mentions) == 0 || len(v.Handles.Valid) == 0 || len(v.Handles.Invalid) == 0 {
		t.Fatal("vectors.json is missing a section")
	}
	return v
}

func TestHandles(t *testing.T) {
	for _, c := range readVectors(t).Mentions {
		got := mention.Handles(c.Text)
		if got == nil {
			got = []string{}
		}
		if !slices.Equal(got, c.Mentions) {
			t.Errorf("Handles(%q) = %q, want %q (%s)", c.Text, got, c.Mentions, c.Why)
		}
	}
}

func TestValidHandle(t *testing.T) {
	v := readVectors(t)
	for _, h := range v.Handles.Valid {
		if !mention.ValidHandle(h) {
			t.Errorf("ValidHandle(%q) = false, want true", h)
		}
	}
	for _, h := range v.Handles.Invalid {
		if mention.ValidHandle(h) {
			t.Errorf("ValidHandle(%q) = true, want false", h)
		}
	}
}
