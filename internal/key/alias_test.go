package key_test

import (
	"strings"
	"testing"

	"defios.com/internal/key"
)

func TestNormalizeAliasAcceptsWhatADR0033Allows(t *testing.T) {
	composed := strings.Repeat("é", 63)
	for in, want := range map[string]string{
		"treasury":               "treasury",
		"Cold Storage":           "Cold Storage",
		"🦄":                      "🦄",
		"👩‍💻":                    "👩‍💻", // a joined emoji holds a format character
		strings.Repeat("x", 63):  strings.Repeat("x", 63),
		strings.Repeat("é", 63): composed, // 126 code points, 63 after NFC
	} {
		got, err := key.NormalizeAlias(in)
		if err != nil || got != want {
			t.Errorf("NormalizeAlias(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNormalizeAliasRefusesEverythingElse(t *testing.T) {
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
		if got, err := key.NormalizeAlias(in); err == nil {
			t.Errorf("NormalizeAlias(%q) = %q; want a refusal", in, got)
		}
	}
}

func TestSameAliasIgnoresCaseAndNormalForm(t *testing.T) {
	for _, pair := range [][2]string{
		{"Treasury", "tREASURY"},
		{"Straße", "STRASSE"},
		{"ΣΑΣ", "σας"},
		{"café", "CAFÉ"},
	} {
		if !key.SameAlias(pair[0], pair[1]) {
			t.Errorf("SameAlias(%q, %q) = false", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{{"treasury", "treasury2"}, {"cafe", "café"}} {
		if key.SameAlias(pair[0], pair[1]) {
			t.Errorf("SameAlias(%q, %q) = true", pair[0], pair[1])
		}
	}
}
