package evmabi_test

import (
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"defios.com/internal/evmabi"
)

func mustParse(t *testing.T, sig string) evmabi.Func {
	t.Helper()
	f, err := evmabi.Parse(sig)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func args(vs []evmabi.Value) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Arg()
	}
	return out
}

// Decoding a call Call encoded gives back its arguments as Call takes
// them, so they encode to the same bytes.
func TestDecodeRoundTripsThroughCall(t *testing.T) {
	for _, c := range []struct {
		sig  string
		args []string
		want []string // as Decode writes them; nil when args already are
	}{
		{"baz(uint32,bool)", []string{"69", "true"}, nil},
		{"bar(bytes3[2])", []string{`["0x616263","0x646566"]`}, nil},
		{"sam(bytes,bool,uint256[])", []string{"0x64617665", "true", "[1,2,3]"},
			[]string{"0x64617665", "true", `["1","2","3"]`}},
		{"f(uint256,uint32[],bytes10,bytes)", []string{"0x123", `["0x456","0x789"]`, "0x31323334353637383930", "0x48656c6c6f2c20776f726c6421"},
			[]string{"291", `["1110","1929"]`, "0x31323334353637383930", "0x48656c6c6f2c20776f726c6421"}},
		{"g(uint256[][],string[])", []string{"[[1,2],[3]]", `["one","two","three"]`},
			[]string{`[["1","2"],["3"]]`, `["one","two","three"]`}},
		{"h(int8,(uint8,string),(address,bool))", []string{"-1", `[7,"hi"]`, `["0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed",true]`},
			[]string{"-1", `["7","hi"]`, `["0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed","true"]`}},
		{"e()", nil, nil},
		{"b(bytes)", []string{"0x"}, nil},
	} {
		data, err := evmabi.Call(c.sig, c.args)
		if err != nil {
			t.Fatalf("%s: %v", c.sig, err)
		}
		vals, err := mustParse(t, c.sig).Decode(data)
		if err != nil {
			t.Fatalf("%s: decode: %v", c.sig, err)
		}
		want := c.want
		if want == nil {
			want = c.args
		}
		if got := args(vals); !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("%s: decoded %q, want %q", c.sig, got, want)
		}
		again, err := evmabi.Call(c.sig, args(vals))
		if err != nil || !slices.Equal(again, data) {
			t.Errorf("%s: re-encoding the decoded arguments gave %x, %v", c.sig, again, err)
		}
	}
}

// The names a signature gives its parameters and its tuples' components
// come back on the decoded values, with their canonical types.
func TestDecodeNamesWhatTheSignatureNames(t *testing.T) {
	sig := "permit(address owner, ((address token, uint160 amount) details, address spender) single, bytes signature)"
	f := mustParse(t, sig)
	if !slices.Equal(f.Names, []string{"owner", "single", "signature"}) {
		t.Fatalf("names %q", f.Names)
	}
	data, err := evmabi.Call(sig, []string{"0x0000000000000000000000000000000000000001", `[["0x0000000000000000000000000000000000000002", 5], "0x0000000000000000000000000000000000000003"]`, "0xabcd"})
	if err != nil {
		t.Fatal(err)
	}
	vals, err := f.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	single := vals[1]
	if single.Name != "single" || single.Type != "((address,uint160),address)" || len(single.Elems) != 2 {
		t.Fatalf("%+v", single)
	}
	details := single.Elems[0]
	if details.Name != "details" || details.Elems[1].Name != "amount" || details.Elems[1].Type != "uint160" || details.Elems[1].Text != "5" {
		t.Fatalf("%+v", details)
	}
	if spender := single.Elems[1]; spender.Name != "spender" || spender.Text != "0x0000000000000000000000000000000000000003" {
		t.Fatalf("%+v", spender)
	}
}

// EIP-55's own test vectors.
func TestChecksumIsEIP55(t *testing.T) {
	for _, want := range []string{
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	} {
		b, _ := hex.DecodeString(strings.ToLower(want[2:]))
		if got := evmabi.Checksum(b); got != want {
			t.Errorf("Checksum = %s, want %s", got, want)
		}
	}
}

// Call data that does not hold the call it is read as is refused, never
// read past or wrapped.
func TestDecodeRefusesMalformedCallData(t *testing.T) {
	transfer := mustParse(t, "transfer(address,uint256)")
	good, _ := evmabi.Call("transfer(address,uint256)", []string{"0x0102030405060708090a0b0c0d0e0f1011121314", "1"})
	dyn := mustParse(t, "f(bytes)")
	arr := mustParse(t, "f(uint256[])")
	for _, c := range []struct {
		name string
		f    evmabi.Func
		data string
		says string
	}{
		{"another selector", transfer, "095ea7b3" + hex.EncodeToString(good[4:]), "selector"},
		{"no selector", transfer, "a905", "selector"},
		{"short", transfer, hex.EncodeToString(good[:40]), "short"},
		{"dirty address", transfer, "a9059cbb" + "01" + hex.EncodeToString(good[5:]), "address"},
		{"bool of 2", mustParse(t, "f(bool)"), hex.EncodeToString(mustParse(t, "f(bool)").Selector()) + word("2"), "bool"},
		{"uint8 overflow", mustParse(t, "f(uint8)"), hex.EncodeToString(mustParse(t, "f(uint8)").Selector()) + word("100"), "uint8"},
		{"int8 not sign-extended", mustParse(t, "f(int8)"), hex.EncodeToString(mustParse(t, "f(int8)").Selector()) + word("ff"), "int8"},
		{"bytes2 dirty padding", mustParse(t, "f(bytes2)"), hex.EncodeToString(mustParse(t, "f(bytes2)").Selector()) + word("1"), "bytes2"},
		{"offset past the end", dyn, hex.EncodeToString(dyn.Selector()) + word("1000"), "offset"},
		{"length past the end", dyn, hex.EncodeToString(dyn.Selector()) + word("20") + word("ffff"), "length"},
		{"huge array", arr, hex.EncodeToString(arr.Selector()) + word("20") + strings.Repeat("f", 64), "length"},
		// lengths whose size in bytes wraps an int64: 2^62 and 2^58 words
		{"wrapping array", arr, hex.EncodeToString(arr.Selector()) + word("20") + word("4000000000000000"), "length"},
		{"wrapping array, negative", arr, hex.EncodeToString(arr.Selector()) + word("20") + word("400000000000000"), "length"},
	} {
		data, _ := hex.DecodeString(c.data)
		_, err := c.f.Decode(data)
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: err = %v, want it to say %q", c.name, err, c.says)
		}
	}
}

// Offsets that all point at one large value cannot make decoding blow up.
func TestDecodeBoundsRepeatedOffsets(t *testing.T) {
	f := mustParse(t, "f(bytes[][])")
	// one outer array of n inner arrays, each the same array of n bytes
	// values, each the same 2 KiB value.
	n := 64
	var b strings.Builder
	b.WriteString(hex.EncodeToString(f.Selector()))
	b.WriteString(word("20"))                                // outer offset
	b.WriteString(word(hex.EncodeToString([]byte{byte(n)}))) // outer length
	innerAt := 32 * n                                        // relative to after the outer length
	for range n {
		b.WriteString(word(hex.EncodeToString([]byte{byte(innerAt >> 8), byte(innerAt)})))
	}
	b.WriteString(word(hex.EncodeToString([]byte{byte(n)})))
	valueAt := 32 * n
	for range n {
		b.WriteString(word(hex.EncodeToString([]byte{byte(valueAt >> 8), byte(valueAt)})))
	}
	b.WriteString(word("800"))
	b.WriteString(strings.Repeat("ab", 2048))
	data, _ := hex.DecodeString(b.String())
	if _, err := f.Decode(data); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want too large", err)
	}
}

// An ABI's functions read as signatures with their parameters' and
// components' names.
func TestFromABI(t *testing.T) {
	abi := `[
		{"type":"event","name":"Transfer","inputs":[]},
		{"type":"function","name":"transfer","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}]},
		{"type":"function","name":"batch","inputs":[{"name":"items","type":"tuple[]","components":[{"name":"who","type":"address"},{"name":"n","type":"uint"}]}]}
	]`
	for _, src := range []string{abi, `{"abi":` + abi + `}`} {
		fs, err := evmabi.FromABI([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if len(fs) != 2 || fs[0].Signature() != "transfer(address,uint256)" || !slices.Equal(fs[0].Names, []string{"to", "amount"}) {
			t.Fatalf("%+v", fs)
		}
		if fs[1].Signature() != "batch((address,uint256)[])" {
			t.Fatalf("%s", fs[1].Signature())
		}
		data, _ := evmabi.Call(fs[1].Signature(), []string{`[["0x0000000000000000000000000000000000000001", 2]]`})
		vals, err := fs[1].Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		if n := vals[0].Elems[0].Elems[1]; n.Name != "n" || n.Text != "2" {
			t.Fatalf("%+v", n)
		}
	}
	if _, err := evmabi.FromABI([]byte(`{"nope":1}`)); err == nil {
		t.Fatal("an object with no abi read as one")
	}
}
