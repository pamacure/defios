package key

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// MaxAliasLen is a Key alias's limit in code points. The shortest other way
// to name a Key is the 64-character Conversation ID, so at 63 no alias can be
// mistaken for one and no character needs banning. Raising it would need a
// new rule (ADR 0033).
const MaxAliasLen = 63

// NormalizeAlias is s in the NFC form a Key alias is stored in, or an error
// if s cannot be one: 1 to MaxAliasLen code points after NFC, with no control
// characters and no leading or trailing whitespace (ADR 0033). A format
// character is allowed, because a joined emoji holds one.
func NormalizeAlias(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("key alias %q is not valid UTF-8", s)
	}
	a := norm.NFC.String(s)
	n := utf8.RuneCountInString(a)
	if n < 1 || n > MaxAliasLen {
		return "", fmt.Errorf("key alias %q must be 1 to %d characters, not %d", s, MaxAliasLen, n)
	}
	if strings.ContainsFunc(a, unicode.IsControl) {
		return "", fmt.Errorf("key alias %q holds a control character", s)
	}
	if strings.TrimSpace(a) != a {
		return "", fmt.Errorf("key alias %q has leading or trailing whitespace", s)
	}
	return a, nil
}

// SameAlias reports whether a and b are one Key alias: they compare
// case-insensitively, by full Unicode case folding, in any normal form.
func SameAlias(a, b string) bool { return foldAlias(a) == foldAlias(b) }

func foldAlias(s string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(s)))
}
