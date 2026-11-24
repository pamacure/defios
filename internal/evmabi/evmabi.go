// Package evmabi encodes an Ethereum contract call as the Solidity ABI lays
// it out, from the function's signature and its arguments written out as
// text: what a person types to call a contract without its ABI file. It
// decodes call data back into those arguments, by a signature or a JSON
// ABI's functions, and writes addresses in their EIP-55 form.
//
// An argument of an elementary type is its value as text: an address and
// bytes as 0x hex, an integer in decimal or 0x hex, a bool as true or
// false, a string as itself. An array or tuple is a JSON array of its
// elements, each a JSON string, number or bool, or a nested array.
package evmabi

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/crypto/sha3"
)

// Func is a parsed function signature.
type Func struct {
	Name string
	// Inputs are its parameters' types, canonical: uint for uint256, no
	// names or data locations.
	Inputs []string
	// Names are its parameters' names, "" where the signature gives none.
	Names []string
	types []*typ
}

// Signature is f as its selector is taken of: name(type,type).
func (f Func) Signature() string {
	return f.Name + "(" + strings.Join(f.Inputs, ",") + ")"
}

// Params are f's parameters as its signature writes them, each its type
// then its name: "address to", or "address" where it has none.
func (f Func) Params() []string {
	out := make([]string, len(f.Inputs))
	for i, t := range f.Inputs {
		out[i] = strings.TrimSpace(t + " " + f.Names[i])
	}
	return out
}

// Selector is the first 4 bytes of the Keccak-256 of f's Signature.
func (f Func) Selector() []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(f.Signature()))
	return h.Sum(nil)[:4]
}

// Parse reads a signature: transfer(address,uint256), or as a contract's
// source or an explorer writes it, with a leading "function", parameter
// names and data locations, such as transfer(address to, uint256 amount).
func Parse(sig string) (Func, error) {
	s := strings.TrimSpace(sig)
	s = strings.TrimSpace(strings.TrimPrefix(s, "function "))
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") || !balanced(s[open:]) {
		return Func{}, errors.New("a signature is name(type, …), such as transfer(address,uint256)")
	}
	f := Func{Name: strings.TrimSpace(s[:open])}
	if !ident(f.Name) {
		return Func{}, errors.New("the signature needs a function name before its (")
	}
	for _, p := range splitTop(s[open+1 : len(s)-1]) {
		t, name, err := param(p)
		if err != nil {
			return Func{}, err
		}
		f.types = append(f.types, t)
		f.Inputs = append(f.Inputs, t.String())
		f.Names = append(f.Names, name)
	}
	return f, nil
}

// Call is the call data that calls sig with args, one per parameter: its
// selector, then the arguments encoded.
func Call(sig string, args []string) ([]byte, error) {
	f, err := Parse(sig)
	if err != nil {
		return nil, err
	}
	if len(args) != len(f.types) {
		return nil, fmt.Errorf("%s takes %d arguments, not %d", f.Signature(), len(f.types), len(args))
	}
	vals := make([]any, len(args))
	for i, a := range args {
		v, err := input(f.types[i], a)
		if err != nil {
			return nil, fmt.Errorf("argument %d (%s): %w", i+1, f.Inputs[i], err)
		}
		vals[i] = v
	}
	enc, err := encodeSeq(f.types, vals, func(i int) string { return fmt.Sprintf("argument %d (%s)", i+1, f.Inputs[i]) })
	if err != nil {
		return nil, err
	}
	return append(f.Selector(), enc...), nil
}

type kind int

const (
	kAddress kind = iota
	kBool
	kUint
	kInt
	kFixedBytes
	kBytes
	kString
	kArray
	kTuple
)

type typ struct {
	kind   kind
	size   int // bits of a uint or int; bytes of a bytesN
	elem   *typ
	length int // of an array; -1 when dynamic
	comps  []*typ
	names  []string // of a tuple's components, "" where unnamed
}

func (t *typ) String() string {
	switch t.kind {
	case kAddress:
		return "address"
	case kBool:
		return "bool"
	case kUint:
		return "uint" + strconv.Itoa(t.size)
	case kInt:
		return "int" + strconv.Itoa(t.size)
	case kFixedBytes:
		return "bytes" + strconv.Itoa(t.size)
	case kBytes:
		return "bytes"
	case kString:
		return "string"
	case kArray:
		if t.length < 0 {
			return t.elem.String() + "[]"
		}
		return t.elem.String() + "[" + strconv.Itoa(t.length) + "]"
	}
	parts := make([]string, len(t.comps))
	for i, c := range t.comps {
		parts[i] = c.String()
	}
	return "(" + strings.Join(parts, ",") + ")"
}

// dynamic is whether t is encoded in the tail, behind an offset.
func (t *typ) dynamic() bool {
	switch t.kind {
	case kBytes, kString:
		return true
	case kArray:
		return t.length < 0 || t.elem.dynamic()
	case kTuple:
		for _, c := range t.comps {
			if c.dynamic() {
				return true
			}
		}
	}
	return false
}

// headSize is the bytes a static t takes in place.
func (t *typ) headSize() int {
	switch t.kind {
	case kArray:
		return t.length * t.elem.headSize()
	case kTuple:
		n := 0
		for _, c := range t.comps {
			n += c.headSize()
		}
		return n
	}
	return 32
}

// param is a parameter's type and name, dropping any data location: the
// name is the last word after the type, unless that is a data location.
func param(p string) (*typ, string, error) {
	p = strings.TrimSpace(p)
	rest := ""
	depth := 0
	for i, r := range p {
		switch r {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		}
		if depth == 0 && unicode.IsSpace(r) {
			p, rest = p[:i], p[i:]
			break
		}
	}
	if p == "" {
		return nil, "", errors.New("the signature has an empty parameter")
	}
	t, err := parseType(p)
	if err != nil {
		return nil, "", err
	}
	name := ""
	if words := strings.Fields(rest); len(words) > 0 {
		switch last := words[len(words)-1]; last {
		case "memory", "calldata", "storage", "indexed", "payable":
		default:
			name = last
		}
	}
	return t, name, nil
}

func parseType(s string) (*typ, error) {
	if strings.HasSuffix(s, "]") {
		i := strings.LastIndexByte(s, '[')
		if i <= 0 {
			return nil, fmt.Errorf("%s is not a type the ABI knows", s)
		}
		elem, err := parseType(s[:i])
		if err != nil {
			return nil, err
		}
		t := &typ{kind: kArray, elem: elem, length: -1}
		if n := strings.TrimSpace(s[i+1 : len(s)-1]); n != "" {
			t.length, err = strconv.Atoi(n)
			if err != nil || t.length < 1 {
				return nil, fmt.Errorf("%s is not a type the ABI knows", s)
			}
		}
		return t, nil
	}
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && balanced(s) {
		t := &typ{kind: kTuple}
		for _, p := range splitTop(s[1 : len(s)-1]) {
			c, name, err := param(p)
			if err != nil {
				return nil, err
			}
			t.comps = append(t.comps, c)
			t.names = append(t.names, name)
		}
		if len(t.comps) == 0 {
			return nil, errors.New("a tuple needs at least one component")
		}
		return t, nil
	}
	switch s {
	case "address":
		return &typ{kind: kAddress}, nil
	case "bool":
		return &typ{kind: kBool}, nil
	case "bytes":
		return &typ{kind: kBytes}, nil
	case "string":
		return &typ{kind: kString}, nil
	case "uint":
		return &typ{kind: kUint, size: 256}, nil
	case "int":
		return &typ{kind: kInt, size: 256}, nil
	}
	for _, b := range []struct {
		prefix   string
		kind     kind
		min, max int
		step     int
	}{{"uint", kUint, 8, 256, 8}, {"int", kInt, 8, 256, 8}, {"bytes", kFixedBytes, 1, 32, 1}} {
		if n, ok := strings.CutPrefix(s, b.prefix); ok {
			size, err := strconv.Atoi(n)
			if err == nil && n[0] != '0' && size >= b.min && size <= b.max && size%b.step == 0 {
				return &typ{kind: b.kind, size: size}, nil
			}
		}
	}
	return nil, fmt.Errorf("%s is not a type the ABI knows", s)
}

// splitTop splits s at its commas outside brackets.
func splitTop(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	depth, from := 0, 0
	for i, r := range s {
		switch r {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[from:i])
				from = i + 1
			}
		}
	}
	return append(out, s[from:])
}

// balanced is whether s, starting with (, closes it at its end.
func balanced(s string) bool {
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return false
			}
		}
		if depth < 0 {
			return false
		}
	}
	return depth == 0
}

func ident(s string) bool {
	for i, r := range s {
		if r != '_' && r != '$' && !unicode.IsLetter(r) && (i == 0 || !unicode.IsDigit(r)) {
			return false
		}
	}
	return s != ""
}

// input is the argument a for t: itself for an elementary type, decoded
// from JSON for an array or tuple.
func input(t *typ, a string) (any, error) {
	if t.kind != kArray && t.kind != kTuple {
		return a, nil
	}
	d := json.NewDecoder(strings.NewReader(a))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil || d.More() {
		return nil, errors.New("write it as a JSON array, such as [1, 2]")
	}
	return v, nil
}

func encodeSeq(ts []*typ, vs []any, name func(int) string) ([]byte, error) {
	headLen := 0
	for _, t := range ts {
		if t.dynamic() {
			headLen += 32
		} else {
			headLen += t.headSize()
		}
	}
	var head, tail []byte
	for i, t := range ts {
		enc, err := encode(t, vs[i])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name(i), err)
		}
		if t.dynamic() {
			head = append(head, uintWord(big.NewInt(int64(headLen+len(tail))))...)
			tail = append(tail, enc...)
		} else {
			head = append(head, enc...)
		}
	}
	return append(head, tail...), nil
}

func encode(t *typ, v any) ([]byte, error) {
	switch t.kind {
	case kArray, kTuple:
		list, ok := v.([]any)
		if !ok {
			return nil, errors.New("write it as a JSON array, such as [1, 2]")
		}
		if t.kind == kTuple {
			if len(list) != len(t.comps) {
				return nil, fmt.Errorf("a %s needs %d components, not %d", t, len(t.comps), len(list))
			}
			return encodeSeq(t.comps, list, func(i int) string { return fmt.Sprintf("component %d (%s)", i+1, t.comps[i]) })
		}
		if t.length >= 0 && len(list) != t.length {
			return nil, fmt.Errorf("a %s needs %d elements, not %d", t, t.length, len(list))
		}
		ts := make([]*typ, len(list))
		for i := range ts {
			ts[i] = t.elem
		}
		enc, err := encodeSeq(ts, list, func(i int) string { return fmt.Sprintf("element %d", i+1) })
		if err != nil || t.length >= 0 {
			return enc, err
		}
		return append(uintWord(big.NewInt(int64(len(list)))), enc...), nil
	}
	s, err := scalar(v)
	if err != nil {
		return nil, err
	}
	switch t.kind {
	case kAddress:
		b, err := hexArg(s)
		if err != nil || len(b) != 20 {
			return nil, errors.New("an address is 0x and 20 bytes of hex")
		}
		return leftPad(b), nil
	case kBool:
		switch s {
		case "true":
			return uintWord(big.NewInt(1)), nil
		case "false":
			return uintWord(big.NewInt(0)), nil
		}
		return nil, errors.New("a bool is true or false")
	case kUint, kInt:
		n, ok := new(big.Int).SetString(s, 0)
		if !ok {
			return nil, errors.New("an integer is written in decimal or 0x hex")
		}
		lo, hi := big.NewInt(0), new(big.Int).Lsh(big.NewInt(1), uint(t.size))
		if t.kind == kInt {
			hi.Rsh(hi, 1)
			lo.Neg(hi)
		}
		if n.Cmp(lo) < 0 || n.Cmp(hi) >= 0 {
			return nil, fmt.Errorf("%s is out of range for a %s", s, t)
		}
		if n.Sign() < 0 {
			n.Add(n, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		return uintWord(n), nil
	case kFixedBytes:
		b, err := hexArg(s)
		if err != nil || len(b) != t.size {
			return nil, fmt.Errorf("a %s is 0x and %d bytes of hex", t, t.size)
		}
		return rightPad(b), nil
	case kBytes:
		b, err := hexArg(s)
		if err != nil {
			return nil, errors.New("bytes are written as 0x hex")
		}
		return append(uintWord(big.NewInt(int64(len(b)))), rightPad(b)...), nil
	}
	return append(uintWord(big.NewInt(int64(len(s)))), rightPad([]byte(s))...), nil
}

// scalar is an elementary value as text, from an argument or a JSON
// string, number or bool.
func scalar(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return strings.TrimSpace(v), nil
	case json.Number:
		return v.String(), nil
	case bool:
		return strconv.FormatBool(v), nil
	}
	return "", errors.New("write it as a string, number or bool, not an array")
}

func hexArg(s string) ([]byte, error) {
	h, ok := strings.CutPrefix(s, "0x")
	if !ok {
		h, ok = strings.CutPrefix(s, "0X")
	}
	if !ok {
		return nil, errors.New("not 0x hex")
	}
	return hex.DecodeString(h)
}

func uintWord(n *big.Int) []byte { return n.FillBytes(make([]byte, 32)) }

func leftPad(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }

func rightPad(b []byte) []byte {
	if r := len(b) % 32; r != 0 || len(b) == 0 {
		b = append(bytes.Clone(b), make([]byte, (32-r)%32)...)
	}
	return b
}
