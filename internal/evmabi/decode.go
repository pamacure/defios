package evmabi

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/sha3"
)

// Value is one decoded argument, or one element or component of one.
type Value struct {
	// Name is the parameter's or component's name, "" where the signature
	// gives none and for an array's elements.
	Name string
	// Type is its canonical type, as in Func.Inputs.
	Type string
	// Text is an elementary value as Call takes it, an address in its
	// EIP-55 form; "" for an array or tuple.
	Text string
	// Elems are an array's elements or a tuple's components.
	Elems []Value
	list  bool
}

// List is whether v is an array or tuple, holding Elems.
func (v Value) List() bool { return v.list }

// Arg is v as Call takes it: Text, or a JSON array of the elements'.
func (v Value) Arg() string {
	if !v.list {
		return v.Text
	}
	parts := make([]string, len(v.Elems))
	for i, e := range v.Elems {
		if e.list {
			parts[i] = e.Arg()
			continue
		}
		b, _ := json.Marshal(e.Text)
		parts[i] = string(b)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// Decode reads data, call data starting with f's selector, as f's
// arguments. Data that does not hold them is refused: an offset or length
// past its end, or a word its type does not allow, such as an address
// with high bytes set. Bytes after the arguments are not looked at.
func (f Func) Decode(data []byte) ([]Value, error) {
	if len(data) < 4 || !bytes.Equal(data[:4], f.Selector()) {
		return nil, fmt.Errorf("the call data does not start with %s's selector 0x%x", f.Signature(), f.Selector())
	}
	// Offsets may point many values at the same bytes; a budget a few
	// times the data's size bounds the work that can make.
	d := &decoder{budget: 4*len(data) + 1024}
	return d.seq(f.types, f.Names, data[4:])
}

type decoder struct{ budget int }

// seq reads values of ts laid out from the start of b, which their
// offsets count from.
func (d *decoder) seq(ts []*typ, names []string, b []byte) ([]Value, error) {
	out := make([]Value, 0, len(ts))
	pos := 0
	for i, t := range ts {
		at := b[min(pos, len(b)):]
		if t.dynamic() {
			off, err := d.offset(b, pos)
			if err != nil {
				return nil, err
			}
			at = b[off:]
			pos += 32
		} else {
			pos += t.headSize()
		}
		v, err := d.value(t, at)
		if err != nil {
			return nil, err
		}
		if names != nil {
			v.Name = names[i]
		}
		out = append(out, v)
	}
	return out, nil
}

// word is the 32 bytes at pos of b.
func (d *decoder) word(b []byte, pos int) ([]byte, error) {
	if pos+32 > len(b) {
		return nil, errors.New("the call data is too short for its arguments")
	}
	return d.charge(b[pos:pos+32], 32)
}

func (d *decoder) charge(w []byte, n int) ([]byte, error) {
	if d.budget -= n; d.budget < 0 {
		return nil, errors.New("the call data is too large to decode: its offsets point at the same bytes over and over")
	}
	return w, nil
}

// offset is the word at pos of b as an offset into b.
func (d *decoder) offset(b []byte, pos int) (int, error) {
	w, err := d.word(b, pos)
	if err != nil {
		return 0, err
	}
	n := new(big.Int).SetBytes(w)
	if !n.IsInt64() || n.Int64() > int64(len(b)) {
		return 0, fmt.Errorf("an offset of %s points past the end of the call data", n)
	}
	return int(n.Int64()), nil
}

// length is the word at the start of b as a count of items each per
// bytes long that must fit in the rest of b.
func (d *decoder) length(b []byte, per int) (int, error) {
	w, err := d.word(b, 0)
	if err != nil {
		return 0, err
	}
	n := new(big.Int).SetBytes(w)
	if !n.IsInt64() || n.Int64() > int64((len(b)-32)/per) {
		return 0, fmt.Errorf("a length of %s runs past the end of the call data", n)
	}
	return int(n.Int64()), nil
}

func (d *decoder) value(t *typ, b []byte) (Value, error) {
	v := Value{Type: t.String()}
	switch t.kind {
	case kTuple:
		elems, err := d.seq(t.comps, t.names, b)
		v.Elems, v.list = elems, true
		return v, err
	case kArray:
		n := t.length
		if n < 0 {
			per := 32
			if !t.elem.dynamic() {
				per = t.elem.headSize()
			}
			var err error
			if n, err = d.length(b, per); err != nil {
				return v, err
			}
			b = b[32:]
		}
		ts := make([]*typ, n)
		for i := range ts {
			ts[i] = t.elem
		}
		elems, err := d.seq(ts, nil, b)
		v.Elems, v.list = elems, true
		return v, err
	case kBytes, kString:
		n, err := d.length(b, 1)
		if err != nil {
			return v, err
		}
		raw, err := d.charge(b[32:32+n], n)
		if err != nil {
			return v, err
		}
		if t.kind == kString {
			if !utf8.Valid(raw) {
				return v, errors.New("a string argument is not UTF-8")
			}
			v.Text = string(raw)
		} else {
			v.Text = "0x" + hex.EncodeToString(raw)
		}
		return v, nil
	}
	w, err := d.word(b, 0)
	if err != nil {
		return v, err
	}
	switch t.kind {
	case kAddress:
		if !zero(w[:12]) {
			return v, fmt.Errorf("an address word 0x%x has its high 12 bytes set", w)
		}
		v.Text = Checksum(w[12:])
	case kBool:
		if !zero(w[:31]) || w[31] > 1 {
			return v, fmt.Errorf("a bool word 0x%x is not 0 or 1", w)
		}
		v.Text = fmt.Sprint(w[31] == 1)
	case kUint, kInt:
		n := new(big.Int).SetBytes(w)
		lo, hi := big.NewInt(0), new(big.Int).Lsh(big.NewInt(1), uint(t.size))
		if t.kind == kInt {
			if w[0]&0x80 != 0 {
				n.Sub(n, new(big.Int).Lsh(big.NewInt(1), 256))
			}
			hi.Rsh(hi, 1)
			lo.Neg(hi)
		}
		if n.Cmp(lo) < 0 || n.Cmp(hi) >= 0 {
			return v, fmt.Errorf("a word 0x%x is out of range for a %s", w, t)
		}
		v.Text = n.String()
	case kFixedBytes:
		if !zero(w[t.size:]) {
			return v, fmt.Errorf("a %s word 0x%x has its padding set", t, w)
		}
		v.Text = "0x" + hex.EncodeToString(w[:t.size])
	}
	return v, nil
}

func zero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// Checksum is a 20-byte address in its EIP-55 mixed-case form.
func Checksum(addr []byte) string {
	lower := hex.EncodeToString(addr)
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(lower))
	sum := h.Sum(nil)
	out := []byte(lower)
	for i, c := range out {
		nibble := sum[i/2] >> 4
		if i%2 == 1 {
			nibble = sum[i/2] & 0xf
		}
		if c >= 'a' && nibble >= 8 {
			out[i] = c - 'a' + 'A'
		}
	}
	return "0x" + string(out)
}

type abiParam struct {
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Components []abiParam `json:"components"`
}

type abiEntry struct {
	Type   string     `json:"type"`
	Name   string     `json:"name"`
	Inputs []abiParam `json:"inputs"`
}

// FromABI reads the functions of a contract's JSON ABI: its array of
// entries, a compiler artifact holding it as "abi", or one entry.
func FromABI(src []byte) ([]Func, error) {
	var entries []abiEntry
	src = bytes.TrimSpace(src)
	if len(src) > 0 && src[0] == '[' {
		if err := json.Unmarshal(src, &entries); err != nil {
			return nil, fmt.Errorf("the ABI is not a JSON array of entries: %w", err)
		}
	} else {
		var obj struct {
			ABI []abiEntry `json:"abi"`
			abiEntry
		}
		if err := json.Unmarshal(src, &obj); err != nil {
			return nil, fmt.Errorf("the ABI is not JSON: %w", err)
		}
		switch {
		case obj.ABI != nil:
			entries = obj.ABI
		case obj.Name != "":
			entries = []abiEntry{obj.abiEntry}
		default:
			return nil, errors.New(`the ABI is neither an array of entries, an object holding one as "abi", nor one entry`)
		}
	}
	var out []Func
	for _, e := range entries {
		if e.Type != "function" && e.Type != "" {
			continue
		}
		params := make([]string, len(e.Inputs))
		for i, p := range e.Inputs {
			params[i] = p.String()
		}
		f, err := Parse(e.Name + "(" + strings.Join(params, ", ") + ")")
		if err != nil {
			return nil, fmt.Errorf("the ABI's function %s: %w", e.Name, err)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, errors.New("the ABI has no functions")
	}
	return out, nil
}

// String is p as a signature writes it: its type, then its name.
func (p abiParam) String() string {
	t := p.Type
	if rest, ok := strings.CutPrefix(t, "tuple"); ok {
		comps := make([]string, len(p.Components))
		for i, c := range p.Components {
			comps[i] = c.String()
		}
		t = "(" + strings.Join(comps, ", ") + ")" + rest
	}
	return strings.TrimSpace(t + " " + p.Name)
}
