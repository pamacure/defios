package builtinmcp

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"unicode/utf8"

	"filippo.io/edwards25519"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/base58"
	coslv1 "defios.com/proto/cosl/v1"
)

// solanaTx is the solana Plugin's tx MCP Server: offline, decoding the
// System, SPL Token and Associated Token Account programs' instructions,
// and reading addresses and converting units (ADR 0042).
func solanaTx() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "solana/tx", Version: "0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name: "decode_instruction",
		Description: "Decode one Solana instruction, given as in a Solana intent (protobuf-JSON: base58 pubkeys, data base64), " +
			"into its program, its instruction and arguments, and its accounts by role with their signer and writable flags. " +
			"It knows the System, SPL Token (and Token-2022's shared instructions) and Associated Token Account programs; " +
			"another program, or an instruction of theirs it does not know, is said to be unknown. " +
			"setAuthority, approve, approveChecked, and closeAccount to a destination other than its owner or close authority are marked SENSITIVE.",
	}, decodeInstruction)
	mcp.AddTool(s, &mcp.Tool{
		Name: "decode_transaction",
		Description: "Decode a Solana intent as sign_propose takes it (protobuf-JSON: base58 pubkeys, data base64): " +
			"its fee payer, each instruction as decode_instruction reads it, and every signer it needs besides the fee payer.",
	}, decodeTransaction)
	mcp.AddTool(s, &mcp.Tool{
		Name: "address",
		Description: "Check a Solana address is base58 of 32 bytes, and say whether it is on the Ed25519 curve: " +
			"a key's address is, a program-derived address (PDA) is not.",
	}, solanaAddress)
	mcp.AddTool(s, &mcp.Tool{
		Name: "units",
		Description: "Convert an amount exactly between lamports and sol, or a token's base units (base) and whole units (whole) given its decimals. " +
			"The amount is decimal, with an optional fraction.",
	}, solanaUnits)
	return s
}

type solInstructionIn struct {
	Instruction any `json:"instruction" jsonschema:"a Solana instruction as in an intent: protobuf-JSON, program_id and account pubkeys base58, data base64"`
}

func decodeInstruction(_ context.Context, _ *mcp.CallToolRequest, in solInstructionIn) (*mcp.CallToolResult, any, error) {
	var ix coslv1.SolanaInstruction
	if err := protoArg(in.Instruction, &ix); err != nil {
		return nil, nil, fmt.Errorf("the instruction is not a Solana instruction as an intent holds it: %w", err)
	}
	var b strings.Builder
	if err := writeInstruction(&b, &ix, ""); err != nil {
		return nil, nil, err
	}
	return text(b.String()), nil, nil
}

type solTransactionIn struct {
	Intent any `json:"intent" jsonschema:"a Solana intent as sign_propose takes it: protobuf-JSON, pubkeys base58, data base64"`
}

func decodeTransaction(_ context.Context, _ *mcp.CallToolRequest, in solTransactionIn) (*mcp.CallToolResult, any, error) {
	var intent coslv1.SolanaIntent
	if err := protoArg(in.Intent, &intent); err != nil {
		return nil, nil, fmt.Errorf("the intent is not a Solana intent as sign_propose takes it: %w", err)
	}
	payer, err := solPubkey(intent.FeePayer)
	if err != nil {
		return nil, nil, fmt.Errorf("fee_payer: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "fee payer: %s\n", payer)
	fmt.Fprintf(&b, "recent blockhash: %s\n", orNone(intent.RecentBlockhash))
	fmt.Fprintf(&b, "cluster: %s\n", orNone(intent.Cluster))
	if intent.GenesisHash != "" {
		fmt.Fprintf(&b, "genesis hash: %s\n", intent.GenesisHash)
	}
	if len(intent.Instructions) == 0 {
		b.WriteString("\nno instructions\n")
	}
	// the signers besides the fee payer, in first appearance, with the
	// instructions that need each
	var signers []string
	needs := map[string][]string{}
	for i, ix := range intent.Instructions {
		fmt.Fprintf(&b, "\ninstruction %d: ", i+1)
		if err := writeInstruction(&b, ix, "  "); err != nil {
			return nil, nil, fmt.Errorf("instruction %d: %w", i+1, err)
		}
		for _, a := range ix.Accounts {
			k := canonical(a.Pubkey)
			if !a.Signer || k == payer {
				continue
			}
			n := fmt.Sprint(i + 1)
			if len(needs[k]) == 0 {
				signers = append(signers, k)
			}
			if l := needs[k]; len(l) == 0 || l[len(l)-1] != n {
				needs[k] = append(l, n)
			}
		}
	}
	b.WriteString("\n")
	if len(signers) == 0 {
		b.WriteString("no signer besides the fee payer\n")
		return text(b.String()), nil, nil
	}
	parts := make([]string, len(signers))
	for i, k := range signers {
		word := "instruction"
		if len(needs[k]) > 1 {
			word = "instructions"
		}
		parts[i] = fmt.Sprintf("%s (%s %s)", k, word, strings.Join(needs[k], ", "))
	}
	fmt.Fprintf(&b, "signers besides the fee payer: %s\n", strings.Join(parts, "; "))
	b.WriteString("the Key signs only as the fee payer, so it cannot sign this transaction as it is\n")
	return text(b.String()), nil, nil
}

// protoArg reads a tool argument, JSON, into m as protobuf-JSON.
func protoArg(v any, m proto.Message) error {
	if v == nil {
		return errors.New("it is missing")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(raw, m)
}

type solAddressIn struct {
	Address string `json:"address" jsonschema:"a base58 Solana address"`
}

func solanaAddress(_ context.Context, _ *mcp.CallToolRequest, in solAddressIn) (*mcp.CallToolResult, any, error) {
	s := strings.TrimSpace(in.Address)
	raw, err := base58.DecodeN(s, 32)
	if err != nil || base58.Encode(raw) != s {
		return nil, nil, errors.New("not a Solana address: an address is base58 of exactly 32 bytes")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "valid: base58 of 32 bytes\nhex: %s\n", hex.EncodeToString(raw))
	if onCurve(raw) {
		b.WriteString("on the Ed25519 curve: a public key, which its private key signs for\n")
	} else {
		b.WriteString("off the Ed25519 curve: no private key signs for it, as with a program-derived address (PDA), which only its program signs for\n")
	}
	return text(b.String()), nil, nil
}

// onCurve is whether key is the encoding of an Ed25519 point, which is
// what Solana asks of a key and denies a PDA.
func onCurve(key []byte) bool {
	_, err := new(edwards25519.Point).SetBytes(key)
	return err == nil
}

type solUnitsIn struct {
	Amount   string `json:"amount" jsonschema:"a decimal amount, such as 1.5"`
	From     string `json:"from" jsonschema:"lamports, sol, base or whole"`
	To       string `json:"to" jsonschema:"lamports, sol, base or whole"`
	Decimals *int   `json:"decimals,omitempty" jsonschema:"the token's decimals, needed for whole"`
}

// solUnits are Solana's units, smallest first.
var solUnits = []unit{{"lamports", 0}, {"sol", 9}}

func solanaUnits(_ context.Context, _ *mcp.CallToolRequest, in solUnitsIn) (*mcp.CallToolResult, any, error) {
	return convert(unitsIn(in), solUnits)
}

// writeInstruction writes ix: a first line naming it, then, indented, its
// arguments, its accounts by role, and what to beware of. It fails only on
// a program ID that is no address.
func writeInstruction(b *strings.Builder, ix *coslv1.SolanaInstruction, indent string) error {
	program, err := solPubkey(ix.ProgramId)
	if err != nil {
		return fmt.Errorf("program_id: %w", err)
	}
	keys := make([]string, len(ix.Accounts))
	for i, a := range ix.Accounts {
		keys[i] = canonical(a.Pubkey)
	}
	p, known := solPrograms[program]
	if !known {
		fmt.Fprintf(b, "program %s is unknown: its instructions are not decoded here\n", program)
		writeRaw(b, ix, indent)
		return nil
	}
	tag, data, err := p.tag(ix.Data)
	if err != nil {
		fmt.Fprintf(b, "%s: does not decode: %v\n", p.name, err)
		writeRaw(b, ix, indent)
		return nil
	}
	l, known := p.layouts[tag]
	if !known {
		fmt.Fprintf(b, "%s: instruction %d is unknown here\n", p.name, tag)
		writeRaw(b, ix, indent)
		return nil
	}
	fmt.Fprintf(b, "%s: %s\n", p.name, l.name)
	r := &solReader{b: data}
	var args []solArg
	if l.args != nil {
		args = l.args(r)
	}
	if r.err != nil {
		fmt.Fprintf(b, "%s  does not decode as %s: %v\n", indent, l.name, r.err)
		writeRaw(b, ix, indent)
		return nil
	}
	if extra := len(r.b); extra > 0 && p.strict {
		fmt.Fprintf(b, "%s  does not decode as %s: %s past its arguments, which the program refuses\n", indent, l.name, plural(extra, "extra byte"))
		writeRaw(b, ix, indent)
		return nil
	}
	for _, a := range args {
		fmt.Fprintf(b, "%s  %s: %s\n", indent, a.name, a.text)
	}
	fmt.Fprintf(b, "%s  accounts:\n", indent)
	for i, a := range ix.Accounts {
		role := l.extra
		if i < len(l.roles) {
			role = l.roles[i]
		}
		if role == "" {
			role = fmt.Sprintf("extra account %d", i+1)
		}
		writeAccount(b, indent+"    ", role, a)
	}
	fewest := l.min
	if fewest == 0 {
		fewest = len(l.roles)
	}
	if len(ix.Accounts) < fewest {
		fmt.Fprintf(b, "%s  note: %s expects at least %d accounts, has %d\n", indent, l.name, fewest, len(ix.Accounts))
	}
	if l.sensitive != nil {
		if why := l.sensitive(args, keys); why != "" {
			fmt.Fprintf(b, "%s  SENSITIVE: %s\n", indent, why)
		}
	}
	if extra := len(r.b); extra > 0 {
		fmt.Fprintf(b, "%s  note: the data carries %s past the arguments, which the program ignores\n", indent, plural(extra, "extra byte"))
	}
	return nil
}

// writeRaw writes the accounts and data of an instruction not decoded.
func writeRaw(b *strings.Builder, ix *coslv1.SolanaInstruction, indent string) {
	fmt.Fprintf(b, "%s  accounts:", indent)
	if len(ix.Accounts) == 0 {
		b.WriteString(" none")
	}
	b.WriteString("\n")
	for i, a := range ix.Accounts {
		writeAccount(b, indent+"    ", fmt.Sprintf("account %d", i+1), a)
	}
	fmt.Fprintf(b, "%s  data (%s): %s\n", indent, plural(len(ix.Data), "byte"), hex.EncodeToString(ix.Data))
}

func writeAccount(b *strings.Builder, indent, role string, a *coslv1.SolanaAccount) {
	var flags []string
	if a.Signer {
		flags = append(flags, "signer")
	}
	if a.Writable {
		flags = append(flags, "writable")
	}
	if len(flags) == 0 {
		flags = []string{"read-only"}
	}
	fmt.Fprintf(b, "%s%s: %s (%s)", indent, role, canonical(a.Pubkey), strings.Join(flags, ", "))
	if _, err := solPubkey(a.Pubkey); err != nil {
		b.WriteString(" — not a valid address")
	}
	b.WriteString("\n")
}

func solPubkey(s string) (string, error) {
	raw, err := base58.DecodeN(strings.TrimSpace(s), 32)
	if err != nil {
		return "", fmt.Errorf("%q is not base58 of 32 bytes", s)
	}
	return base58.Encode(raw), nil
}

// canonical is s as an address in its one base58 form, or s if it is none.
func canonical(s string) string {
	if k, err := solPubkey(s); err == nil {
		return k
	}
	return s
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func orNone(s string) string {
	if s == "" {
		return "none given"
	}
	return s
}

// solProgram is a program whose instructions are decoded: by the tag
// leading their data, each to a layout.
type solProgram struct {
	name    string
	tag     func(data []byte) (tag int, rest []byte, err error)
	layouts map[int]solLayout
	// strict is a program that refuses data past an instruction's
	// arguments, where the others ignore it.
	strict bool
}

// solLayout is one instruction: its arguments, read from the data after
// its tag, and its accounts' roles in order.
type solLayout struct {
	name  string
	roles []string
	// min is the fewest accounts it takes, when fewer than its roles.
	min int
	// extra is the role of accounts past its roles: multisig signers.
	extra string
	args  func(r *solReader) []solArg
	// sensitive says why the instruction is one to check twice, or "".
	sensitive func(args []solArg, keys []string) string
}

type solArg struct {
	name, text string
	n          uint64 // an integer's value
	key        string // a pubkey's, "" for none
}

// solReader reads little-endian fields off data, keeping its first error.
type solReader struct {
	b   []byte
	err error
}

func (r *solReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if len(r.b) < n {
		r.err = fmt.Errorf("the data ends %d bytes short", n-len(r.b))
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *solReader) u8() uint8 {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *solReader) u64() uint64 {
	if b := r.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (r *solReader) pubkey() string {
	if b := r.take(32); b != nil {
		return base58.Encode(b)
	}
	return ""
}

// str is a Rust String as bincode writes it: a u64 length, then UTF-8.
func (r *solReader) str() string {
	n := r.u64()
	if r.err == nil && n > uint64(len(r.b)) {
		r.err = fmt.Errorf("a string of %d bytes is longer than the data", n)
		return ""
	}
	b := r.take(int(n))
	if r.err == nil && !utf8.Valid(b) {
		r.err = errors.New("a string is not UTF-8")
	}
	return string(b)
}

// optPubkey is an SPL COption<Pubkey>: 0 for none, 1 and the key for some.
func (r *solReader) optPubkey() string {
	switch tag := r.u8(); {
	case r.err != nil:
	case tag == 0:
	case tag == 1:
		return r.pubkey()
	default:
		r.err = fmt.Errorf("an optional key's tag is %d, not 0 or 1", tag)
	}
	return ""
}

func lamports(name string, n uint64) solArg {
	sol := new(big.Rat).SetFrac(new(big.Int).SetUint64(n), big.NewInt(1e9))
	return solArg{name: name, text: fmt.Sprintf("%d (%s SOL)", n, decimalString(sol, 9)), n: n}
}

func number(name string, n uint64) solArg {
	return solArg{name: name, text: fmt.Sprint(n), n: n}
}

func key(name, k string) solArg { return solArg{name: name, text: k, key: k} }

func seed(name, s string) solArg { return solArg{name: name, text: fmt.Sprintf("%q", s)} }

// tokens is an amount of a token's base units, and its whole units when
// its decimals are known.
func tokens(n uint64, decimals *uint8) solArg {
	t := fmt.Sprintf("%d base units", n)
	if decimals != nil {
		whole := new(big.Rat).SetFrac(new(big.Int).SetUint64(n), pow10(int(*decimals)).Num())
		t += fmt.Sprintf(" (%s at %d decimals)", decimalString(whole, int(*decimals)), *decimals)
	}
	if n == math.MaxUint64 {
		t += " — the most a u64 holds"
	}
	return solArg{name: "amount", text: t, n: n}
}

func amount(r *solReader) []solArg { return []solArg{tokens(r.u64(), nil)} }

func checkedAmount(r *solReader) []solArg {
	n, d := r.u64(), r.u8()
	return []solArg{tokens(n, &d), number("decimals", uint64(d))}
}

const (
	systemProgramID = "11111111111111111111111111111111"
	tokenProgramID  = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token2022ID     = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	ataProgramID    = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
)

var solPrograms = map[string]solProgram{
	systemProgramID: {name: "System Program", tag: u32Tag, layouts: systemLayouts},
	tokenProgramID:  {name: "SPL Token", tag: u8Tag, layouts: tokenLayouts(splAuthorities[:4])},
	token2022ID:     {name: "SPL Token-2022", tag: u8Tag, layouts: tokenLayouts(splAuthorities)},
	ataProgramID: {name: "Associated Token Account", strict: true, layouts: ataLayouts, tag: func(data []byte) (int, []byte, error) {
		if len(data) == 0 { // the original form, create, has no data
			return 0, nil, nil
		}
		return u8Tag(data)
	}},
}

func u8Tag(data []byte) (int, []byte, error) {
	if len(data) < 1 {
		return 0, nil, errors.New("the data is empty, so names no instruction")
	}
	return int(data[0]), data[1:], nil
}

func u32Tag(data []byte) (int, []byte, error) {
	if len(data) < 4 {
		return 0, nil, fmt.Errorf("the data is %d bytes, too short for the 4-byte instruction tag", len(data))
	}
	return int(binary.LittleEndian.Uint32(data)), data[4:], nil
}

// systemLayouts are the System Program's instructions, bincode-encoded
// after a u32 tag.
var systemLayouts = map[int]solLayout{
	0: {name: "createAccount", roles: []string{"funding account", "new account"},
		args: func(r *solReader) []solArg {
			return []solArg{lamports("lamports", r.u64()), number("space", r.u64()), key("owner", r.pubkey())}
		}},
	1: {name: "assign", roles: []string{"assigned account"},
		args: func(r *solReader) []solArg { return []solArg{key("owner", r.pubkey())} }},
	2: {name: "transfer", roles: []string{"source", "destination"},
		args: func(r *solReader) []solArg { return []solArg{lamports("lamports", r.u64())} }},
	3: {name: "createAccountWithSeed", roles: []string{"funding account", "created account", "base account"}, min: 2,
		args: func(r *solReader) []solArg {
			return []solArg{key("base", r.pubkey()), seed("seed", r.str()), lamports("lamports", r.u64()), number("space", r.u64()), key("owner", r.pubkey())}
		}},
	4: {name: "advanceNonceAccount", roles: []string{"nonce account", "recent blockhashes sysvar", "nonce authority"}},
	8: {name: "allocate", roles: []string{"allocated account"},
		args: func(r *solReader) []solArg { return []solArg{number("space", r.u64())} }},
	9: {name: "allocateWithSeed", roles: []string{"allocated account", "base account"},
		args: func(r *solReader) []solArg {
			return []solArg{key("base", r.pubkey()), seed("seed", r.str()), number("space", r.u64()), key("owner", r.pubkey())}
		}},
	10: {name: "assignWithSeed", roles: []string{"assigned account", "base account"},
		args: func(r *solReader) []solArg {
			return []solArg{key("base", r.pubkey()), seed("seed", r.str()), key("owner", r.pubkey())}
		}},
	11: {name: "transferWithSeed", roles: []string{"source", "base account", "destination"},
		args: func(r *solReader) []solArg {
			return []solArg{lamports("lamports", r.u64()), seed("from seed", r.str()), key("from owner", r.pubkey())}
		}},
}

// splAuthorities are SPL Token's AuthorityType values; Token-2022's
// extensions add those past the first four.
var splAuthorities = []string{
	"mint tokens", "freeze account", "account owner", "close account",
	"transfer fee config", "withheld withdraw", "close mint", "interest rate", "permanent delegate",
	"confidential transfer mint", "transfer hook program id", "confidential transfer fee config",
	"metadata pointer", "group pointer", "group member pointer", "scaled UI amount", "pause",
}

// tokenLayouts are the instructions SPL Token and Token-2022 share, each
// tag a u8. An authority may be a multisig, its signers the accounts after.
func tokenLayouts(authorities []string) map[int]solLayout {
	const signer = "multisig signer"
	approve := func(args []solArg, keys []string) string {
		delegate := 1
		if len(args) > 1 { // approveChecked names the mint before the delegate
			delegate = 2
		}
		if args[0].n == math.MaxUint64 {
			return fmt.Sprintf("delegate %s may move an unlimited amount from %s without the owner signing again",
				at(keys, delegate), at(keys, 0))
		}
		return fmt.Sprintf("delegate %s may move up to %s from %s without the owner signing again",
			at(keys, delegate), args[0].text, at(keys, 0))
	}
	return map[int]solLayout{
		1: {name: "initializeAccount", roles: []string{"account", "mint", "owner", "rent sysvar"}},
		3: {name: "transfer", roles: []string{"source", "destination", "authority"}, extra: signer, args: amount},
		4: {name: "approve", roles: []string{"source", "delegate", "owner"}, extra: signer, args: amount, sensitive: approve},
		5: {name: "revoke", roles: []string{"source", "owner"}, extra: signer},
		6: {name: "setAuthority", roles: []string{"mint or account", "current authority"}, extra: signer,
			args: func(r *solReader) []solArg {
				t := r.u8()
				if r.err == nil && int(t) >= len(authorities) {
					r.err = fmt.Errorf("authority type %d is none this program has", t)
				}
				name := ""
				if r.err == nil {
					name = authorities[t]
				}
				to := r.optPubkey()
				next := key("new authority", to)
				if to == "" {
					next.text = "none"
				}
				return []solArg{{name: "authority type", text: name, n: uint64(t)}, next}
			},
			sensitive: func(args []solArg, keys []string) string {
				if args[1].key == "" {
					return fmt.Sprintf("removes the %s authority of %s for good: no one can hold it again", args[0].text, at(keys, 0))
				}
				return fmt.Sprintf("hands the %s authority of %s to %s", args[0].text, at(keys, 0), args[1].key)
			}},
		7: {name: "mintTo", roles: []string{"mint", "destination", "mint authority"}, extra: signer, args: amount},
		8: {name: "burn", roles: []string{"account", "mint", "owner"}, extra: signer, args: amount},
		9: {name: "closeAccount", roles: []string{"account", "destination", "owner"}, extra: signer,
			sensitive: func(_ []solArg, keys []string) string {
				if len(keys) < 3 || keys[1] == keys[2] {
					return ""
				}
				return fmt.Sprintf("sends the closed account's lamports to %s, not its owner or close authority %s", keys[1], keys[2])
			}},
		10: {name: "freezeAccount", roles: []string{"account", "mint", "freeze authority"}, extra: signer},
		11: {name: "thawAccount", roles: []string{"account", "mint", "freeze authority"}, extra: signer},
		12: {name: "transferChecked", roles: []string{"source", "mint", "destination", "authority"}, extra: signer, args: checkedAmount},
		13: {name: "approveChecked", roles: []string{"source", "mint", "delegate", "owner"}, extra: signer, args: checkedAmount, sensitive: approve},
		14: {name: "mintToChecked", roles: []string{"mint", "destination", "mint authority"}, extra: signer, args: checkedAmount},
		15: {name: "burnChecked", roles: []string{"account", "mint", "owner"}, extra: signer, args: checkedAmount},
		16: {name: "initializeAccount2", roles: []string{"account", "mint", "rent sysvar"},
			args: func(r *solReader) []solArg { return []solArg{key("owner", r.pubkey())} }},
		18: {name: "initializeAccount3", roles: []string{"account", "mint"},
			args: func(r *solReader) []solArg { return []solArg{key("owner", r.pubkey())} }},
	}
}

// at is keys[i], or a placeholder if the instruction lacks it.
func at(keys []string, i int) string {
	if i < len(keys) {
		return keys[i]
	}
	return "(missing account)"
}

var ataCreate = []string{"funding account", "associated token account", "wallet", "mint", "system program", "token program"}

// ataLayouts are the Associated Token Account program's instructions,
// borsh-encoded, so data past them is refused.
var ataLayouts = map[int]solLayout{
	0: {name: "create", roles: ataCreate},
	1: {name: "createIdempotent", roles: ataCreate},
	2: {name: "recoverNested", roles: []string{"nested account", "nested mint", "destination", "owner account", "owner mint", "wallet", "token program"}},
}
