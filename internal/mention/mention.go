// Package mention finds the Handles a message of talk mentions, from its
// text alone (ADR 0016). docs/mentions/vectors.json is the contract.
package mention

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxHandle is the longest a Handle may be.
const maxHandle = 64

// Handles returns the Handles text mentions, in the order each first
// appears, each once. A mention is an "@" not preceded by a word character,
// "@" or "\", then the longest [A-Za-z0-9]+([-_][A-Za-z0-9]+)* after it, not
// followed by a word character, folded to lowercase. So "a@b.com",
// "\@auditor" and "@auditoré" are not mentions, and "@Auditor," and
// "@auditor--" mention auditor. A mention need not be a valid Handle ("@user",
// or one too long); whether it names one included where it is sent is the
// caller's to check.
func Handles(text string) []string {
	var out []string
	prev := rune(0) // what precedes text[i]; 0 at the start
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == '@' && prev != '@' && prev != '\\' && !word(prev) {
			if end := match(text, i+1); end > i+1 {
				if next, _ := utf8.DecodeRuneInString(text[end:]); !word(next) {
					if h := strings.ToLower(text[i+1 : end]); !slices.Contains(out, h) {
						out = append(out, h)
					}
				}
				prev, i = rune(text[end-1]), end
				continue
			}
		}
		prev, i = r, i+size
	}
	return out
}

// ValidHandle reports whether h is a Handle: [a-z0-9]+([-_][a-z0-9]+)*, 1 to
// 64 characters, and not "user", which is how a person's talk is shown.
func ValidHandle(h string) bool {
	return h != "" && h != "user" && len(h) <= maxHandle && match(h, 0) == len(h) && h == strings.ToLower(h)
}

// match is the end of the longest [A-Za-z0-9]+([-_][A-Za-z0-9]+)* in text
// starting at i, or i if there is none.
func match(text string, i int) int {
	end := i
	for end < len(text) && alnum(text[end]) {
		end++
	}
	if end == i {
		return i
	}
	for end+1 < len(text) && (text[end] == '-' || text[end] == '_') && alnum(text[end+1]) {
		end += 2
		for end < len(text) && alnum(text[end]) {
			end++
		}
	}
	return end
}

func alnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// word is a word character: Unicode L, M or N. utf8.RuneError, which
// decoding the end of the text gives, is not one.
func word(r rune) bool {
	return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r))
}
