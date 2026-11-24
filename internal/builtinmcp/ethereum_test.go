package builtinmcp_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"defios.com/internal/builtinmcp"
	"defios.com/internal/evmabi"
)

const (
	alice = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
	bob   = "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359"
	max   = "115792089237316195423570985008687907853269984665640564039457584007913129639935"
)

func ethTx(t *testing.T) *mcp.ClientSession { return connect(t, "ethereum/tx") }

// connect connects a client to a fresh built-in server named ref.
func connect(t *testing.T, ref string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	serverT, clientT := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- builtinmcp.Serve(ctx, ref, serverT) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		cancel()
		<-done
	})
	return cs
}

// call calls tool with args and gives its text and whether it failed.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String(), res.IsError
}

func ok(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	text, failed := call(t, cs, tool, args)
	if failed {
		t.Fatalf("%s %v failed: %s", tool, args, text)
	}
	return text
}

func says(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("missing %q in:\n%s", w, text)
		}
	}
}

func calldata(t *testing.T, sig string, args ...string) []byte {
	t.Helper()
	b, err := evmabi.Call(sig, args)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestEthereumTxOffersItsTools(t *testing.T) {
	res, err := ethTx(t).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"address", "decode_calldata", "encode_calldata", "units"}) {
		t.Fatalf("tools %v", names)
	}
}

// Every standard call is recognised by its selector alone, which is the
// one each standard publishes.
func TestDecodeCalldataRecognisesTheStandards(t *testing.T) {
	cs := ethTx(t)
	tuple := `["0x0000000000000000000000000000000000000001", 1, 2, 3]`
	for _, c := range []struct {
		selector, sig string
		args          []string
		standard      string
	}{
		{"a9059cbb", "transfer(address,uint256)", []string{alice, "1"}, "ERC-20"},
		{"39509351", "increaseAllowance(address,uint256)", []string{alice, "1"}, "ERC-20"},
		{"a457c2d7", "decreaseAllowance(address,uint256)", []string{alice, "1"}, "ERC-20"},
		{"42842e0e", "safeTransferFrom(address,address,uint256)", []string{alice, bob, "1"}, "ERC-721"},
		{"b88d4fde", "safeTransferFrom(address,address,uint256,bytes)", []string{alice, bob, "1", "0x"}, "ERC-721"},
		{"a22cb465", "setApprovalForAll(address,bool)", []string{alice, "false"}, "ERC-1155"},
		{"f242432a", "safeTransferFrom(address,address,uint256,uint256,bytes)", []string{alice, bob, "1", "2", "0x"}, "ERC-1155"},
		{"2eb2c2d6", "safeBatchTransferFrom(address,address,uint256[],uint256[],bytes)", []string{alice, bob, "[1]", "[2]", "0x"}, "ERC-1155"},
		{"2b67b570", "permit(address,((address,uint160,uint48,uint48),address,uint256),bytes)", []string{alice, `[` + tuple + `,"` + bob + `",4]`, "0x"}, "Permit2"},
		{"2a2d80d1", "permit(address,((address,uint160,uint48,uint48)[],address,uint256),bytes)", []string{alice, `[[` + tuple + `],"` + bob + `",4]`, "0x"}, "Permit2"},
		{"30f28b7a", "permitTransferFrom(((address,uint256),uint256,uint256),(address,uint256),address,bytes)", []string{`[["` + alice + `",1],2,3]`, `["` + bob + `",1]`, alice, "0x"}, "Permit2"},
		{"edd9444b", "permitTransferFrom(((address,uint256)[],uint256,uint256),(address,uint256)[],address,bytes)", []string{`[[["` + alice + `",1]],2,3]`, `[["` + bob + `",1]]`, alice, "0x"}, "Permit2"},
		{"36c78516", "transferFrom(address,address,uint160,address)", []string{alice, bob, "1", alice}, "Permit2"},
		{"0d58b1db", "transferFrom((address,address,uint160,address)[])", []string{`[["` + alice + `","` + bob + `",1,"` + alice + `"]]`}, "Permit2"},
		{"87517c45", "approve(address,address,uint160,uint48)", []string{alice, bob, "1", "2"}, "Permit2"},
		{"cc53287f", "lockdown((address,address)[])", []string{`[["` + alice + `","` + bob + `"]]`}, "Permit2"},
	} {
		data := calldata(t, c.sig, c.args...)
		if got := hex.EncodeToString(data[:4]); got != c.selector {
			t.Errorf("%s has selector %s, want %s", c.sig, got, c.selector)
		}
		text := ok(t, cs, "decode_calldata", map[string]any{"data": b64(data)})
		says(t, text, c.standard, c.sig[:strings.IndexByte(c.sig, '(')]+"(", "0x"+c.selector)
		if strings.Contains(text, "ambiguous") {
			t.Errorf("%s read as ambiguous:\n%s", c.sig, text)
		}
	}
}

func TestDecodeCalldataNamesTheArguments(t *testing.T) {
	text := ok(t, ethTx(t), "decode_calldata", map[string]any{"data": "0x" + hex.EncodeToString(calldata(t, "transfer(address,uint256)", strings.ToLower(alice), "1000"))})
	says(t, text, "ERC-20 transfer(address to, uint256 amount)", "to (address): "+alice, "amount (uint256): 1000")
	if strings.Contains(text, "unlimited") {
		t.Errorf("a transfer read as an approval:\n%s", text)
	}
}

// transferFrom and approve read the same in ERC-20 and ERC-721; without the
// contract, both readings are given.
func TestDecodeCalldataGivesEveryReadingOfASharedSelector(t *testing.T) {
	cs := ethTx(t)
	text := ok(t, cs, "decode_calldata", map[string]any{"data": b64(calldata(t, "transferFrom(address,address,uint256)", alice, bob, "7"))})
	says(t, text, "ambiguous", "ERC-20 transferFrom(address from, address to, uint256 amount)", "amount (uint256): 7",
		"ERC-721 transferFrom(address from, address to, uint256 tokenId)", "tokenId (uint256): 7")

	text = ok(t, cs, "decode_calldata", map[string]any{"data": b64(calldata(t, "approve(address,uint256)", bob, max))})
	says(t, text, "ambiguous", "ERC-20 approve(address spender, uint256 amount)", "ERC-721 approve(address to, uint256 tokenId)")
	if strings.Count(text, "unlimited") != 1 {
		t.Errorf("want only the ERC-20 reading marked unlimited:\n%s", text)
	}
}

func TestDecodeCalldataFlagsUnlimitedApprovals(t *testing.T) {
	cs := ethTx(t)
	for _, c := range []struct {
		sig       string
		args      []string
		unlimited bool
	}{
		{"approve(address,uint256)", []string{bob, max}, true},
		{"approve(address,uint256)", []string{bob, "1000"}, false},
		{"setApprovalForAll(address,bool)", []string{bob, "true"}, true},
		{"setApprovalForAll(address,bool)", []string{bob, "false"}, false},
		{"approve(address,address,uint160,uint48)", []string{alice, bob, "1461501637330902918203684832716283019655932542975", "0"}, true},
		{"approve(address,address,uint160,uint48)", []string{alice, bob, "1", "0"}, false},
	} {
		text := ok(t, cs, "decode_calldata", map[string]any{"data": b64(calldata(t, c.sig, c.args...))})
		if strings.Contains(text, "unlimited") != c.unlimited {
			t.Errorf("%s %v: unlimited = %v, want %v:\n%s", c.sig, c.args, !c.unlimited, c.unlimited, text)
		}
	}
	// with a signature that names the amount otherwise, or not at all
	data := b64(calldata(t, "approve(address,uint256)", bob, max))
	for _, sig := range []string{"approve(address,uint256)", "approve(address guy, uint256 wad)"} {
		says(t, ok(t, cs, "decode_calldata", map[string]any{"data": data, "signature": sig}), "unlimited")
	}
	says(t, ok(t, cs, "decode_calldata", map[string]any{"data": b64(calldata(t, "setApprovalForAll(address,bool)", bob, "true")), "signature": "setApprovalForAll(address,bool)"}), "unlimited")
}

func TestDecodeCalldataReadsEmptyDataAsAPlainTransfer(t *testing.T) {
	cs := ethTx(t)
	says(t, ok(t, cs, "decode_calldata", map[string]any{"data": "", "value": "1000000000000000000"}),
		"plain transfer", "1000000000000000000 wei")
	// as sign_propose takes an intent: protobuf-JSON, bytes base64
	to, _ := hex.DecodeString(bob[2:])
	says(t, ok(t, cs, "decode_calldata", map[string]any{"intent": map[string]any{
		"to": b64(to), "value": "5", "nonce": "3", "gasLimit": "21000", "max_fee_per_gas": "1", "chainId": "1",
	}}), "plain transfer", "5 wei", bob)
}

func TestDecodeCalldataTakesAnIntent(t *testing.T) {
	text := ok(t, ethTx(t), "decode_calldata", map[string]any{"intent": map[string]any{
		"data": b64(calldata(t, "transfer(address,uint256)", alice, "9")), "value": "0", "chainId": "1",
	}})
	says(t, text, "ERC-20 transfer", "amount (uint256): 9")
}

func TestDecodeCalldataNeverGuessesAnUnknownSelector(t *testing.T) {
	text := ok(t, ethTx(t), "decode_calldata", map[string]any{"data": "0xdeadbeef" + strings.Repeat("00", 32)})
	says(t, text, "unknown", "0xdeadbeef")
	if strings.Contains(text, "ERC") || strings.Contains(text, "Permit2") {
		t.Errorf("an unknown selector was read as a standard:\n%s", text)
	}
}

func TestDecodeCalldataWithASignatureOrABI(t *testing.T) {
	cs := ethTx(t)
	data := b64(calldata(t, "swap(uint256,(address,bool)[])", "42", `[["`+alice+`",true]]`))
	says(t, ok(t, cs, "decode_calldata", map[string]any{"data": data, "signature": "function swap(uint256 amountIn, (address who, bool yes)[] legs)"}),
		"swap(uint256 amountIn, (address,bool)[] legs)", "amountIn (uint256): 42", "who (address): "+alice, "yes (bool): true")

	abi := `[{"type":"function","name":"other","inputs":[]},{"type":"function","name":"swap","inputs":[{"name":"amountIn","type":"uint256"},{"name":"legs","type":"tuple[]","components":[{"name":"who","type":"address"},{"name":"yes","type":"bool"}]}]}]`
	says(t, ok(t, cs, "decode_calldata", map[string]any{"data": data, "abi": abi}), "amountIn (uint256): 42", "who (address): "+alice)

	if text, failed := call(t, cs, "decode_calldata", map[string]any{"data": data, "signature": "swap(uint256)"}); !failed || !strings.Contains(text, "selector") {
		t.Errorf("a signature of another selector: %v %s", failed, text)
	}
	if text, failed := call(t, cs, "decode_calldata", map[string]any{"data": data, "abi": `[{"type":"function","name":"other","inputs":[]}]`}); !failed || !strings.Contains(text, "no function") {
		t.Errorf("an ABI without the selector: %v %s", failed, text)
	}
	if _, failed := call(t, cs, "decode_calldata", map[string]any{"data": data, "intent": map[string]any{}}); !failed {
		t.Error("both data and an intent were taken")
	}
	if _, failed := call(t, cs, "decode_calldata", map[string]any{"intent": map[string]any{"calldata": data}}); !failed {
		t.Error("an intent with a misspelt field was taken")
	}
}

func TestAddress(t *testing.T) {
	cs := ethTx(t)
	raw, _ := hex.DecodeString(alice[2:])
	text := ok(t, cs, "address", map[string]any{"address": strings.ToLower(alice)})
	says(t, text, alice, b64(raw), "no checksum")
	says(t, ok(t, cs, "address", map[string]any{"address": alice}), "checksum valid")
	says(t, ok(t, cs, "address", map[string]any{"address": b64(raw)}), alice)
	bad := "0x5AAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
	says(t, ok(t, cs, "address", map[string]any{"address": bad}), "checksum INVALID", alice)
	if _, failed := call(t, cs, "address", map[string]any{"address": "0x1234"}); !failed {
		t.Error("a short address was taken")
	}
}

func TestUnits(t *testing.T) {
	cs := ethTx(t)
	for _, c := range []struct {
		amount, from, to string
		decimals         any
		want             string
	}{
		{"1.5", "ether", "wei", nil, "1500000000000000000"},
		{"1", "wei", "ether", nil, "0.000000000000000001"},
		{"30", "gwei", "wei", nil, "30000000000"},
		{"1234567", "base", "whole", 6, "1.234567"},
		{"2.5", "whole", "base", 6, "2500000"},
		{max, "wei", "ether", nil, "115792089237316195423570985008687907853269984665640564039457.584007913129639935"},
	} {
		args := map[string]any{"amount": c.amount, "from": c.from, "to": c.to}
		if c.decimals != nil {
			args["decimals"] = c.decimals
		}
		text := ok(t, cs, "units", args)
		if !strings.HasPrefix(text, c.want+" "+c.to) {
			t.Errorf("%s %s in %s = %q, want %s", c.amount, c.from, c.to, text, c.want)
		}
	}
	for _, args := range []map[string]any{
		{"amount": "0.5", "from": "wei", "to": "ether"},
		{"amount": "1.0000000001", "from": "gwei", "to": "wei"},
		{"amount": "1", "from": "whole", "to": "base"},
		{"amount": "1e18", "from": "wei", "to": "ether"},
		{"amount": "-1", "from": "wei", "to": "ether"},
		{"amount": "1", "from": "finney", "to": "ether"},
	} {
		if text, failed := call(t, cs, "units", args); !failed {
			t.Errorf("%v was taken: %s", args, text)
		}
	}
}

// encode_calldata writes the call data a signature and its arguments make,
// in base64 for an intent and in hex, and decode_calldata reads it back.
func TestEncodeCalldata(t *testing.T) {
	cs := ethTx(t)
	text := ok(t, cs, "encode_calldata", map[string]any{
		"signature": "transfer(address to, uint256 amount)",
		"args":      []any{alice, "1000000"},
	})
	// transfer's selector, alice's word and 1000000's
	want, _ := hex.DecodeString("a9059cbb" +
		"0000000000000000000000005aaeb6053f3e94c9b9a09f33669435e7ef1beaed" +
		"00000000000000000000000000000000000000000000000000000000000f4240")
	says(t, text, "base64: "+b64(want), "hex: 0x"+hex.EncodeToString(want))
	says(t, ok(t, cs, "decode_calldata", map[string]any{"data": b64(want)}), "transfer", alice, "1000000")
	// an array is a JSON array of strings
	says(t, ok(t, cs, "encode_calldata", map[string]any{"signature": "f(uint256[] xs)", "args": []any{[]any{"1", "2"}}}), "f(uint256[] xs)", "hex: 0x")
	for _, args := range []map[string]any{
		{"signature": "transfer(address,uint256)", "args": []any{alice}},
		{"signature": "transfer(address,uint256)", "args": []any{"0x1234", "1"}},
		{"signature": "nope", "args": []any{}},
		{"signature": "transfer(address,uint256)", "args": []any{alice, 9007199254740993}},
		{"signature": "f(uint256[])", "args": []any{[]any{1, 2}}},
	} {
		if text, failed := call(t, cs, "encode_calldata", args); !failed {
			t.Errorf("%v was taken: %s", args, text)
		}
	}
}
