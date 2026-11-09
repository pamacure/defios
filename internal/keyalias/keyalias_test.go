package keyalias_test

import (
	"strings"
	"testing"

	"defios.com/internal/keyalias"
)

func TestNormalizeAcceptsWhatADR0033Allows(t *testing.T) {
	composed := strings.Repeat("é", 63)
	for in, want := range map[string]string{
		"treasury":               "treasury",
		"Cold Storage":           "Cold Storage",
		"🦄":                      "🦄",
		"👩‍💻":                    "👩‍💻", // a joined emoji holds a format character
		strings.Repeat("x", 63):  strings.Repeat("x", 63),
		strings.Repeat("é", 63): composed, // 126 code points, 63 after NFC
	} {
		got, err := keyalias.Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNormalizeRefusesEverythingElse(t *testing.T) {
	for _, in := range []string{
		"",
		" treasury",
		"treasury ",
		"　treasury",
		"treasury ",
		"a\tb",
		"a\nb",
		"a\x7fb",
		"a\u0085b",
		"a\xffb",
		strings.Repeat("x", 64),
		strings.Repeat("é", 64),
	} {
		if got, err := keyalias.Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q; want a refusal", in, got)
		}
	}
}

func TestSameIgnoresCaseAndNormalForm(t *testing.T) {
	for _, pair := range [][2]string{
		{"Treasury", "tREASURY"},
		{"Straße", "STRASSE"},
		{"ΣΑΣ", "σας"},
		{"café", "CAFÉ"},
	} {
		if !keyalias.Same(pair[0], pair[1]) {
			t.Errorf("Same(%q, %q) = false", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{{"treasury", "treasury2"}, {"cafe", "café"}} {
		if keyalias.Same(pair[0], pair[1]) {
			t.Errorf("Same(%q, %q) = true", pair[0], pair[1])
		}
	}
}
