// Package mention finds the Handles a message of talk mentions (ADR 0015).
package mention

import (
	"slices"
	"unicode"
	"unicode/utf8"
)

// Handles returns the Handles text mentions, in the order they first appear,
// each once. A mention is "@" followed by a Handle ([a-z0-9][a-z0-9_-]*),
// where the "@" is not preceded by a letter, digit or "@": so "a@b.com" is
// not one, and "@auditor," mentions auditor. Whether a Handle is defined or
// included is the caller's to check.
func Handles(text string) []string {
	var out []string
	prev := rune(0) // what precedes text[i]; 0 at the start
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == '@' && prev != '@' && !unicode.IsLetter(prev) && !unicode.IsDigit(prev) {
			start := i + 1
			end := start
			for end < len(text) && handleByte(text[end], end == start) {
				end++
			}
			if end > start {
				if h := text[start:end]; !slices.Contains(out, h) {
					out = append(out, h)
				}
				prev, i = rune(text[end-1]), end
				continue
			}
		}
		prev, i = r, i+size
	}
	return out
}

func handleByte(b byte, first bool) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '_' || b == '-':
		return !first
	}
	return false
}
