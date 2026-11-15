package evmabi_test

import (
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"defios.com/internal/evmabi"
)

// word is a 32-byte ABI word as hex, from its value right-aligned.
func word(v string) string { return strings.Repeat("0", 64-len(v)) + v }

// text is a 32-byte ABI word as hex, from bytes left-aligned.
func text(v string) string { return v + strings.Repeat("0", 64-len(v)) }

// The calls the Solidity ABI specification works through, written out as a
// person would type them.
func TestCallEncodesTheSpecificationsExamples(t *testing.T) {
	for _, c := range []struct {
		sig  string
		args []string
		want []string
	}{
		{"baz(uint32,bool)", []string{"69", "true"},
			[]string{"cdcd77c0", word("45"), word("1")}},
		{"bar(bytes3[2])", []string{`["0x616263","0x646566"]`},
			[]string{"fce353f6", text("616263"), text("646566")}},
		{"sam(bytes,bool,uint256[])", []string{"0x64617665", "true", "[1,2,3]"},
			[]string{"a5643bf2", word("60"), word("1"), word("a0"), word("4"), text("64617665"), word("3"), word("1"), word("2"), word("3")}},
		{"f(uint256,uint32[],bytes10,bytes)", []string{"0x123", `["0x456","0x789"]`, "0x31323334353637383930", "0x48656c6c6f2c20776f726c6421"},
			[]string{"8be65246", word("123"), word("80"), text("31323334353637383930"), word("e0"), word("2"), word("456"), word("789"), word("d"), text("48656c6c6f2c20776f726c6421")}},
		{"g(uint256[][],string[])", []string{"[[1,2],[3]]", `["one","two","three"]`},
			[]string{"2289b18c", word("40"), word("140"),
				word("2"), word("40"), word("a0"), word("2"), word("1"), word("2"), word("1"), word("3"),
				word("3"), word("60"), word("a0"), word("e0"), word("3"), text("6f6e65"), word("3"), text("74776f"), word("5"), text("7468726565")}},
	} {
		got, err := evmabi.Call(c.sig, c.args)
		if err != nil {
			t.Fatalf("%s: %v", c.sig, err)
		}
		if want := strings.Join(c.want, ""); hex.EncodeToString(got) != want {
			t.Errorf("%s:\n got %x\nwant %s", c.sig, got, want)
		}
	}
}

func TestCallEncodesAnERC20Transfer(t *testing.T) {
	got, err := evmabi.Call("transfer(address,uint256)", []string{"0x0102030405060708090a0b0c0d0e0f1011121314", "1000"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "a9059cbb" + word("0102030405060708090a0b0c0d0e0f1011121314") + word("3e8"); hex.EncodeToString(got) != want {
		t.Fatalf("got %x", got)
	}
}

// A signature copied from a contract's source or an explorer carries
// names, data locations and spaces; the selector is of its types alone.
func TestASignatureMayNameItsParameters(t *testing.T) {
	a, err := evmabi.Call("transfer(address to, uint256 amount)", []string{"0x0102030405060708090a0b0c0d0e0f1011121314", "1"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := evmabi.Call("transfer(address,uint256)", []string{"0x0102030405060708090a0b0c0d0e0f1011121314", "1"})
	if !slices.Equal(a, b) {
		t.Fatalf("%x != %x", a, b)
	}
	f, err := evmabi.Parse(" function setName(string calldata name, (uint a, bool b)[] memory xs) ")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "setName" || f.Signature() != "setName(string,(uint256,bool)[])" || !slices.Equal(f.Inputs, []string{"string", "(uint256,bool)[]"}) {
		t.Fatalf("%+v %s", f, f.Signature())
	}
}

func TestCallEncodesSignedIntegersAndTuples(t *testing.T) {
	got, err := evmabi.Call("h(int8,(uint8,string),(address,bool))", []string{"-1", `[7,"hi"]`, `["0x0000000000000000000000000000000000000009",true]`})
	if err != nil {
		t.Fatal(err)
	}
	want := "" +
		strings.Repeat("f", 64) + // -1
		word("80") + // the dynamic tuple, after the 4 head words
		word("9") + word("1") + // the static tuple, inline
		word("7") + word("40") + word("2") + text("6869")
	if hex.EncodeToString(got[4:]) != want {
		t.Fatalf("\n got %x\nwant %s", got[4:], want)
	}
}

func TestCallRefusesWhatItCannotEncode(t *testing.T) {
	for _, c := range []struct {
		sig  string
		args []string
		says string
	}{
		{"transfer(address,uint256)", []string{"0x01"}, "2 arguments"},
		{"f(uint8)", []string{"256"}, "argument 1 (uint8)"},
		{"f(uint256)", []string{"-1"}, "argument 1 (uint256)"},
		{"f(int8)", []string{"-129"}, "argument 1 (int8)"},
		{"f(address)", []string{"0x0102"}, "20 bytes"},
		{"f(bytes2)", []string{"0x010203"}, "2 bytes"},
		{"f(bool)", []string{"yes"}, "true or false"},
		{"f(uint256[2])", []string{"[1]"}, "2 elements"},
		{"f(uint256[])", []string{"1"}, "JSON array"},
		{"f(uint7)", []string{"1"}, "uint7"},
		{"f(bytes33)", []string{"0x01"}, "bytes33"},
		{"f(uint256", nil, "signature"},
		{"(uint256)", nil, "signature"},
	} {
		_, err := evmabi.Call(c.sig, c.args)
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s %v: err = %v, want it to say %q", c.sig, c.args, err, c.says)
		}
	}
}
