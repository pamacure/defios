// Package mention finds the Handles a message of talk mentions (ADR 0015).
package mention

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Handles returns the Handles text mentions, in the order they first appear,
// each once. A mention is "@" followed by a Handle ([a-z0-9][a-z0-9_-]*),
// where the "@" is not preceded by a letter, digit or "@" and the Handle is
// not followed by one: so "a@b.com" and "@auditorX" are not mentions. A
// trailing "-" or "_" is punctuation, so "@auditor," and "@auditor--"
// mention auditor. Whether a Handle is defined or included is the caller's
// to check.
func Handles(text string) []string {
	var out []string
	prev := rune(0) // what precedes text[i]; 0 at the start
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == '@' && !wordRune(prev) {
			start := i + 1
			end := start
			for end < len(text) && handleByte(text[end], end == start) {
				end++
			}
			if end > start {
				next, _ := utf8.DecodeRuneInString(text[end:]) // RuneError at the end
				h := strings.TrimRight(text[start:end], "-_")
				if !wordRune(next) && !slices.Contains(out, h) {
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

// wordRune is what may not touch a mention: a letter, a digit, or another
// "@" before it.
func wordRune(r rune) bool {
	return r == '@' || unicode.IsLetter(r) || unicode.IsDigit(r)
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
