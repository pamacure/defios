package signreq

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"defios.com/internal/evmabi"
)

// Normalize turns transactions in their input form into the form protojson
// reads (ADR 0043), for every path that takes them: sign propose and
// sign fill, as a person or an agent. An Ethereum intent may describe a
// contract call as call, which the Node encodes into data, and may give
// its bytes fields as 0x hex. calls are each transaction's call, parsed,
// nil where it gave none. Other intents are passed through as they are.
func Normalize(txs []TxSpec) (out []TxSpec, calls []*evmabi.Func, err error) {
	out = make([]TxSpec, len(txs))
	calls = make([]*evmabi.Func, len(txs))
	for i, t := range txs {
		out[i] = t
		if len(t.Ethereum) == 0 {
			continue
		}
		out[i].Ethereum, calls[i], err = NormalizeEthereum(t.Ethereum)
		if err != nil {
			return nil, nil, invalidf("transaction %d: %v", i, err)
		}
	}
	return out, calls, nil
}

// ethereumBytes are an Ethereum intent's bytes fields, which may be 0x hex.
var ethereumBytes = []string{"to", "data"}

// NormalizeEthereum is one Ethereum intent's JSON normalized, as Normalize
// does, and the function its call names, nil if it gave no call. JSON that
// is not an object is returned as it is, for protojson to refuse.
func NormalizeEthereum(raw json.RawMessage) (json.RawMessage, *evmabi.Func, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return raw, nil, nil
	}
	for _, name := range ethereumBytes {
		v, ok := fields[name]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			continue // not a string: protojson says what it is
		}
		b, hexed, err := bytesField(s)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %v", name, err)
		}
		if name == "to" {
			if err := checkAddress(s, b, hexed); err != nil {
				return nil, nil, err
			}
		}
		if hexed {
			fields[name], _ = json.Marshal(base64.StdEncoding.EncodeToString(b))
		}
	}
	var f *evmabi.Func
	if c, ok := fields["call"]; ok {
		if _, ok := fields["data"]; ok {
			return nil, nil, errors.New("give call or data, not both")
		}
		data, fn, err := encodeCall(c)
		if err != nil {
			return nil, nil, fmt.Errorf("call: %v", err)
		}
		delete(fields, "call")
		fields["data"], _ = json.Marshal(base64.StdEncoding.EncodeToString(data))
		f = &fn
	}
	out, err := json.Marshal(fields)
	return out, f, err
}

// bytesField reads a bytes field's string: 0x (or 0X) hex, refused if the
// hex is invalid, or else base64 as protojson reads it. 0, x and hex digits
// are all base64 characters, so hex is never tried as base64. b is nil for base64 that
// does not decode, which protojson then refuses.
func bytesField(s string) (b []byte, hexed bool, err error) {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		b, err := hex.DecodeString(s[2:])
		if err != nil {
			return nil, true, fmt.Errorf("%q is not valid 0x hex", s)
		}
		return b, true, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, false, nil
		}
	}
	return nil, false, nil
}

// checkAddress holds to, given as s and read as b, to 20 bytes and, when
// it is mixed-case hex, to its EIP-55 checksum.
func checkAddress(s string, b []byte, hexed bool) error {
	if b == nil {
		return nil
	}
	if len(b) != 20 {
		return fmt.Errorf("to must be 20 bytes, got %d", len(b))
	}
	if h := s[2:]; hexed && h != strings.ToLower(h) && h != strings.ToUpper(h) && s != evmabi.Checksum(b) {
		return fmt.Errorf("to %s fails its EIP-55 checksum (%s): the address may be mistyped", s, evmabi.Checksum(b))
	}
	return nil
}

// encodeCall is the call data of a call: {"signature": ..., "args": [...]},
// its arguments positional, each a string, a bool, or a JSON array for an
// array or tuple. A JSON number is refused: it may already be rounded.
func encodeCall(raw json.RawMessage) ([]byte, evmabi.Func, error) {
	var c struct {
		Signature string            `json:"signature"`
		Args      []json.RawMessage `json:"args"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return nil, evmabi.Func{}, errors.New(`it must be {"signature": "<function signature>", "args": [...]}`)
	}
	f, err := evmabi.Parse(c.Signature)
	if err != nil {
		return nil, evmabi.Func{}, err
	}
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		d := json.NewDecoder(bytes.NewReader(a))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			return nil, evmabi.Func{}, fmt.Errorf("argument %d: %v", i+1, err)
		}
		if hasNumber(v) {
			return nil, evmabi.Func{}, fmt.Errorf("argument %d: give each integer as a string, such as \"1000000\", so none is rounded", i+1)
		}
		if s, ok := v.(string); ok {
			args[i] = s
		} else {
			args[i] = string(a) // a bool or array, as the JSON evmabi reads it
		}
	}
	data, err := evmabi.Call(c.Signature, args)
	if err != nil {
		return nil, evmabi.Func{}, err
	}
	return data, f, nil
}

// hasNumber is whether v, decoded JSON, holds a number anywhere.
func hasNumber(v any) bool {
	switch v := v.(type) {
	case json.Number:
		return true
	case []any:
		for _, e := range v {
			if hasNumber(e) {
				return true
			}
		}
	}
	return false
}
