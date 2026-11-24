package builtinmcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"

	"defios.com/internal/evmabi"
	coslv1 "defios.com/proto/cosl/v1"
)

// ethereumTx is the ethereum Plugin's tx MCP Server: offline, decoding call
// data by a given signature or ABI or as a token standard's, and converting
// addresses and units (ADR 0042).
func ethereumTx() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "ethereum/tx", Version: "0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name: "decode_calldata",
		Description: "Decode an Ethereum transaction's call data into its function and named, typed arguments. " +
			"Give the intent as sign_propose takes it (protobuf-JSON, bytes base64), or its data (0x hex or base64) and optionally value. " +
			"With a signature or ABI, it decodes as that function. With neither, it recognises ERC-20, ERC-721, ERC-1155 and Permit2 calls by selector; " +
			"a selector shared by standards gives every reading, and an unknown one is said to be unknown. " +
			"It knows no protocol's own contracts (routers etc.): give their signature or ABI. Approvals of the maximum amount, and setApprovalForAll(true), are marked unlimited.",
	}, decodeCalldata)
	mcp.AddTool(s, &mcp.Tool{
		Name: "encode_calldata",
		Description: "Encode a contract call's data from the function's signature and its arguments, for an Ethereum intent's data: " +
			"base64, as an intent holds it, and 0x hex. An address or bytes is 0x hex, an integer decimal or 0x hex, a bool true or false, " +
			"a string itself; an array or tuple is a JSON array of its elements. Amounts are in the token's base units: convert them with units first.",
	}, encodeCalldata)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "address",
		Description: "Read an Ethereum address, 0x hex in any case or 20 bytes base64, and give its EIP-55 checksummed form, its base64 form, and whether a mixed-case input's checksum is valid.",
	}, address)
	mcp.AddTool(s, &mcp.Tool{
		Name: "units",
		Description: "Convert an amount exactly between wei, gwei and ether, or a token's base units (base) and whole units (whole) given its decimals. " +
			"The amount is decimal, with an optional fraction.",
	}, units)
	return s
}

type decodeIn struct {
	Intent    any    `json:"intent,omitempty" jsonschema:"an Ethereum intent as sign_propose takes it: protobuf-JSON, to and data base64, value decimal wei"`
	Data      string `json:"data,omitempty" jsonschema:"call data, 0x hex or base64, when no intent is given"`
	Value     string `json:"value,omitempty" jsonschema:"wei sent with the call, decimal, when no intent is given"`
	Signature string `json:"signature,omitempty" jsonschema:"the function's signature, such as transfer(address to, uint256 amount)"`
	ABI       any    `json:"abi,omitempty" jsonschema:"the contract's JSON ABI, as JSON or a string holding it"`
}

func decodeCalldata(_ context.Context, _ *mcp.CallToolRequest, in decodeIn) (*mcp.CallToolResult, any, error) {
	data, value, to, err := in.call()
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	if len(data) == 0 {
		fmt.Fprintf(&b, "no call data: a plain transfer of %s wei", orZero(value))
		if to != nil {
			fmt.Fprintf(&b, " to %s", evmabi.Checksum(to))
		}
		b.WriteString(", calling no function\n")
		return text(b.String()), nil, nil
	}
	if len(data) < 4 {
		return nil, nil, fmt.Errorf("call data of %d bytes is shorter than a 4-byte selector", len(data))
	}
	if value != "" && value != "0" {
		fmt.Fprintf(&b, "sends %s wei with the call\n", value)
	}
	sel := data[:4]
	var fs []evmabi.Func
	switch {
	case in.Signature != "" && in.ABI != nil:
		return nil, nil, errors.New("give a signature or an ABI, not both")
	case in.Signature != "":
		f, err := evmabi.Parse(in.Signature)
		if err != nil {
			return nil, nil, err
		}
		fs = []evmabi.Func{f}
	case in.ABI != nil:
		all, err := abiFuncs(in.ABI)
		if err != nil {
			return nil, nil, err
		}
		for _, f := range all {
			if bytes.Equal(f.Selector(), sel) {
				fs = append(fs, f)
			}
		}
		if len(fs) == 0 {
			return nil, nil, fmt.Errorf("the ABI has no function with selector 0x%x", sel)
		}
	default:
		readings := standardsFor(sel)
		if len(readings) == 0 {
			fmt.Fprintf(&b, "selector 0x%x is unknown: it is none of the token standards' calls known here. Give its signature or the contract's ABI to decode it.\n", sel)
			return text(b.String()), nil, nil
		}
		if len(readings) > 1 {
			fmt.Fprintf(&b, "selector 0x%x is ambiguous without the contract: it reads as each of these, and which applies depends on what the contract is\n", sel)
		} else {
			fmt.Fprintf(&b, "selector 0x%x\n", sel)
		}
		for _, r := range readings {
			vals, err := r.fn.Decode(data)
			if err != nil {
				fmt.Fprintf(&b, "\n%s %s: does not decode: %v\n", r.standard, signature(r.fn), err)
				continue
			}
			fmt.Fprintf(&b, "\n%s %s\n", r.standard, signature(r.fn))
			render(&b, r.fn, vals, data)
		}
		return text(b.String()), nil, nil
	}
	if !bytes.Equal(fs[0].Selector(), sel) {
		return nil, nil, fmt.Errorf("the call data's selector is 0x%x, not %s's 0x%x", sel, fs[0].Signature(), fs[0].Selector())
	}
	fmt.Fprintf(&b, "selector 0x%x\n", sel)
	for _, f := range fs {
		vals, err := f.Decode(data)
		if err != nil {
			return nil, nil, fmt.Errorf("the call data does not decode as %s: %w", f.Signature(), err)
		}
		fmt.Fprintf(&b, "\n%s\n", signature(f))
		render(&b, f, vals, data)
	}
	return text(b.String()), nil, nil
}

// call is the call data, value and recipient of in's intent, or of its
// bare fields.
func (in decodeIn) call() (data []byte, value string, to []byte, err error) {
	if in.Intent == nil {
		data, err = bytesArg(in.Data)
		if err != nil {
			return nil, "", nil, fmt.Errorf("data: %w", err)
		}
		return data, in.Value, nil, nil
	}
	if in.Data != "" || in.Value != "" {
		return nil, "", nil, errors.New("give an intent or its data and value, not both")
	}
	raw, err := json.Marshal(in.Intent)
	if err != nil {
		return nil, "", nil, err
	}
	var intent coslv1.EthereumIntent
	if err := protojson.Unmarshal(raw, &intent); err != nil {
		return nil, "", nil, fmt.Errorf("the intent is not an Ethereum intent as sign_propose takes it: %w", err)
	}
	return intent.Data, intent.Value, intent.To, nil
}

// bytesArg reads 0x hex, or else base64 as protobuf-JSON writes bytes.
func bytesArg(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if h, ok := strings.CutPrefix(strings.ToLower(s), "0x"); ok {
		b, err := hex.DecodeString(h)
		if err != nil {
			return nil, errors.New("not valid 0x hex")
		}
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("neither 0x hex nor base64")
}

func abiFuncs(abi any) ([]evmabi.Func, error) {
	if s, ok := abi.(string); ok {
		return evmabi.FromABI([]byte(s))
	}
	raw, err := json.Marshal(abi)
	if err != nil {
		return nil, err
	}
	return evmabi.FromABI(raw)
}

// signature is f with its parameters' names: transfer(address to, uint256 amount).
func signature(f evmabi.Func) string {
	parts := make([]string, len(f.Inputs))
	for i, t := range f.Inputs {
		parts[i] = strings.TrimSpace(t + " " + f.Names[i])
	}
	return f.Name + "(" + strings.Join(parts, ", ") + ")"
}

// render writes f's decoded arguments, one per line, marking an unlimited
// approval and saying when data is not exactly the encoding of them.
func render(b *strings.Builder, f evmabi.Func, vals []evmabi.Value, data []byte) {
	approval := approvals[f.Name]
	for i, v := range vals {
		writeValue(b, v, fmt.Sprintf("argument %d", i+1), "  ", approval)
	}
	if f.Name == "setApprovalForAll" && len(vals) == 2 && vals[1].Text == "true" {
		fmt.Fprintf(b, "  unlimited approval: every token of the contract, now and later, for operator %s\n", vals[0].Text)
	}
	args := make([]string, len(vals))
	for i, v := range vals {
		args[i] = v.Arg()
	}
	if again, err := evmabi.Call(f.Signature(), args); err != nil || !bytes.Equal(again, data) {
		fmt.Fprintf(b, "  note: the call data is %d bytes, not exactly these arguments' %d-byte encoding: it carries extra or rearranged bytes\n", len(data), len(again))
	}
}

// approvals are the functions that grant an allowance: an integer of
// theirs at the most its type holds is an unlimited one, unless its name
// says it is no amount. An unnamed one is said to be unlimited if it is
// the amount, so a signature without names loses no warning.
var approvals = map[string]bool{"approve": true, "increaseAllowance": true, "permit": true}

// notAmounts are the names of an approval's integers that hold no amount.
var notAmounts = map[string]bool{
	"tokenId": true, "id": true, "nonce": true, "expiration": true, "deadline": true, "sigDeadline": true,
}

func writeValue(b *strings.Builder, v evmabi.Value, fallback, indent string, approval bool) {
	label := v.Name
	if label == "" {
		label = fallback
	}
	if !v.List() {
		fmt.Fprintf(b, "%s%s (%s): %s", indent, label, v.Type, v.Text)
		if approval && !notAmounts[v.Name] && isMax(v) {
			fmt.Fprintf(b, " — unlimited approval: the most a %s holds", v.Type)
			if v.Name == "" {
				b.WriteString(", if this is the amount")
			}
		}
		b.WriteString("\n")
		return
	}
	fmt.Fprintf(b, "%s%s (%s):", indent, label, v.Type)
	if len(v.Elems) == 0 {
		b.WriteString(" empty")
	}
	b.WriteString("\n")
	for i, e := range v.Elems {
		writeValue(b, e, fmt.Sprintf("[%d]", i), indent+"  ", approval)
	}
}

func isMax(v evmabi.Value) bool {
	bits, ok := strings.CutPrefix(v.Type, "uint")
	if !ok {
		return false
	}
	n, ok := new(big.Int).SetString(v.Text, 10)
	if !ok {
		return false
	}
	size, err := strconv.Atoi(bits)
	if err != nil {
		return false
	}
	return n.Cmp(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(size)), big.NewInt(1))) == 0
}

type reading struct {
	standard string
	fn       evmabi.Func
}

// standards are the token standards' calls decode_calldata knows unaided,
// with their parameters as each standard names them.
var standards = func() []reading {
	var out []reading
	for _, s := range []struct{ standard, sig string }{
		{"ERC-20", "transfer(address to, uint256 amount)"},
		{"ERC-20", "transferFrom(address from, address to, uint256 amount)"},
		{"ERC-20", "approve(address spender, uint256 amount)"},
		{"ERC-20", "increaseAllowance(address spender, uint256 addedValue)"},
		{"ERC-20", "decreaseAllowance(address spender, uint256 subtractedValue)"},
		{"ERC-721", "transferFrom(address from, address to, uint256 tokenId)"},
		{"ERC-721", "safeTransferFrom(address from, address to, uint256 tokenId)"},
		{"ERC-721", "safeTransferFrom(address from, address to, uint256 tokenId, bytes data)"},
		{"ERC-721", "approve(address to, uint256 tokenId)"},
		// the same call, meaning the same, in both standards
		{"ERC-721 / ERC-1155", "setApprovalForAll(address operator, bool approved)"},
		{"ERC-1155", "safeTransferFrom(address from, address to, uint256 id, uint256 value, bytes data)"},
		{"ERC-1155", "safeBatchTransferFrom(address from, address to, uint256[] ids, uint256[] values, bytes data)"},
		{"Permit2", "permit(address owner, ((address token, uint160 amount, uint48 expiration, uint48 nonce) details, address spender, uint256 sigDeadline) permitSingle, bytes signature)"},
		{"Permit2", "permit(address owner, ((address token, uint160 amount, uint48 expiration, uint48 nonce)[] details, address spender, uint256 sigDeadline) permitBatch, bytes signature)"},
		{"Permit2", "permitTransferFrom(((address token, uint256 amount) permitted, uint256 nonce, uint256 deadline) permit, (address to, uint256 requestedAmount) transferDetails, address owner, bytes signature)"},
		{"Permit2", "permitTransferFrom(((address token, uint256 amount)[] permitted, uint256 nonce, uint256 deadline) permit, (address to, uint256 requestedAmount)[] transferDetails, address owner, bytes signature)"},
		{"Permit2", "transferFrom(address from, address to, uint160 amount, address token)"},
		{"Permit2", "transferFrom((address from, address to, uint160 amount, address token)[] transferDetails)"},
		{"Permit2", "approve(address token, address spender, uint160 amount, uint48 expiration)"},
		{"Permit2", "lockdown((address token, address spender)[] approvals)"},
	} {
		f, err := evmabi.Parse(s.sig)
		if err != nil {
			panic(err)
		}
		out = append(out, reading{s.standard, f})
	}
	return out
}()

func standardsFor(sel []byte) []reading {
	var out []reading
	for _, r := range standards {
		if bytes.Equal(r.fn.Selector(), sel) {
			out = append(out, r)
		}
	}
	return out
}

type encodeIn struct {
	Signature string `json:"signature" jsonschema:"the function's signature, such as transfer(address to, uint256 amount)"`
	Args      []any  `json:"args" jsonschema:"its arguments in order: each a string, or a JSON array for an array or tuple; give integers as strings"`
}

func encodeCalldata(_ context.Context, _ *mcp.CallToolRequest, in encodeIn) (*mcp.CallToolResult, any, error) {
	f, err := evmabi.Parse(in.Signature)
	if err != nil {
		return nil, nil, err
	}
	args := make([]string, len(in.Args))
	for i, a := range in.Args {
		if hasNumber(a) {
			// JSON numbers reach here as float64, which rounds large ones
			return nil, nil, fmt.Errorf("argument %d: give each integer as a string, such as \"1000000\", so none is rounded", i+1)
		}
		switch v := a.(type) {
		case string:
			args[i] = v
		default:
			// a bool or array, as the JSON evmabi reads it
			raw, err := json.Marshal(v)
			if err != nil {
				return nil, nil, fmt.Errorf("argument %d: %w", i+1, err)
			}
			args[i] = string(raw)
		}
	}
	data, err := evmabi.Call(in.Signature, args)
	if err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("%s\nbase64: %s\nhex: 0x%x\n", signature(f), base64.StdEncoding.EncodeToString(data), data)), nil, nil
}

// hasNumber is whether v, decoded JSON, holds a number anywhere.
func hasNumber(v any) bool {
	switch v := v.(type) {
	case float64:
		return true
	case []any:
		return slices.ContainsFunc(v, hasNumber)
	}
	return false
}

type addressIn struct {
	Address string `json:"address" jsonschema:"0x hex in any case, or 20 bytes base64"`
}

func address(_ context.Context, _ *mcp.CallToolRequest, in addressIn) (*mcp.CallToolResult, any, error) {
	s := strings.TrimSpace(in.Address)
	raw, err := bytesArg(s)
	if err != nil || len(raw) != 20 {
		return nil, nil, errors.New("an address is 0x and 40 hex digits, or 20 bytes base64")
	}
	sum := evmabi.Checksum(raw)
	check := "not 0x hex, so no checksum"
	if h, ok := strings.CutPrefix(s, "0x"); ok {
		switch {
		case h == strings.ToLower(h) || h == strings.ToUpper(h):
			check = "single-case, so no checksum to check"
		case s == sum:
			check = "checksum valid"
		default:
			check = "checksum INVALID: the mixed case does not match EIP-55; the address may be mistyped"
		}
	} else if strings.HasPrefix(s, "0X") {
		check = "0X prefix, so no checksum to check"
	}
	return text(fmt.Sprintf("EIP-55: %s\nbase64: %s\n%s\n", sum, base64.StdEncoding.EncodeToString(raw), check)), nil, nil
}

type unitsIn struct {
	Amount   string `json:"amount" jsonschema:"a decimal amount, such as 1.5"`
	From     string `json:"from" jsonschema:"wei, gwei, ether, base or whole"`
	To       string `json:"to" jsonschema:"wei, gwei, ether, base or whole"`
	Decimals *int   `json:"decimals,omitempty" jsonschema:"the token's decimals, needed for whole"`
}

// etherUnits are Ethereum's units, smallest first.
var etherUnits = []unit{{"wei", 0}, {"gwei", 9}, {"ether", 18}}

func units(_ context.Context, _ *mcp.CallToolRequest, in unitsIn) (*mcp.CallToolResult, any, error) {
	return convert(in, etherUnits)
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
