package builtinmcp

import "testing"

// Every instruction decode_instruction reads arguments of, encode_instruction
// can write: a layout with args has fields, and one without has none.
func TestEveryDecodedLayoutCanBeEncoded(t *testing.T) {
	for id, p := range solPrograms {
		for tag, l := range p.layouts {
			if (l.args != nil) != (l.fields != nil) {
				t.Errorf("%s (%s) instruction %d %s: args set %t, fields set %t", p.name, id, tag, l.name, l.args != nil, l.fields != nil)
			}
		}
	}
}
