package builtinmcp_test

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"defios.com/internal/base58"
)

const (
	systemProgram = "11111111111111111111111111111111"
	tokenProgram  = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token2022     = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	ataProgram    = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	usdc          = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	// wallet's USDC associated token account: a PDA, off the curve.
	walletUSDC = "DJcjpsHnWXSucjUpourygEN3mkcQwSHG6d5b2AzLSfSn"
)

var (
	wallet = solKey(0) // 4zvwRjXUKGfvwnParsHAS3HuSVzV5cA4McphgmoCtajS
	other  = solKey(1)
	third  = solKey(2)
)

// solKey is the public key of an Ed25519 seed of n bytes: on the curve.
func solKey(n byte) string {
	seed := make([]byte, 32)
	seed[0] = n
	return base58.Encode(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
}

func solTx(t *testing.T) *mcp.ClientSession { return connect(t, "solana/tx") }

type acct struct {
	key              string
	signer, writable bool
}

func ix(program string, data []byte, accounts ...acct) map[string]any {
	var as []any
	for _, a := range accounts {
		as = append(as, map[string]any{"pubkey": a.key, "signer": a.signer, "writable": a.writable})
	}
	return map[string]any{"program_id": program, "accounts": as, "data": b64(data)}
}

func s(k string) acct  { return acct{k, true, false} }
func w(k string) acct  { return acct{k, false, true} }
func sw(k string) acct { return acct{k, true, true} }
func r(k string) acct  { return acct{k, false, false} }

func le32(n uint32) []byte { return binary.LittleEndian.AppendUint32(nil, n) }
func le64(n uint64) []byte { return binary.LittleEndian.AppendUint64(nil, n) }
func raw(k string) []byte {
	b, err := base58.DecodeN(k, 32)
	if err != nil {
		panic(err)
	}
	return b
}
func cat(parts ...[]byte) []byte { return slices.Concat(parts...) }

// bincodeStr is a Rust String as bincode writes it: u64 length, bytes.
func bincodeStr(s string) []byte { return cat(le64(uint64(len(s))), []byte(s)) }

func decodeIx(t *testing.T, cs *mcp.ClientSession, instruction map[string]any) string {
	t.Helper()
	return ok(t, cs, "decode_instruction", map[string]any{"instruction": instruction})
}

func TestSolanaTxOffersItsTools(t *testing.T) {
	res, err := solTx(t).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"address", "decode_instruction", "decode_transaction", "units"}) {
		t.Fatalf("tools %v", names)
	}
}

func TestDecodeInstructionReadsTheSystemProgram(t *testing.T) {
	cs := solTx(t)
	for _, c := range []struct {
		name string
		ix   map[string]any
		want []string
	}{
		{"transfer", ix(systemProgram, cat(le32(2), le64(1_500_000_000)), sw(wallet), w(other)),
			[]string{"System Program: transfer", "lamports: 1500000000 (1.5 SOL)",
				"source: " + wallet + " (signer, writable)", "destination: " + other + " (writable)"}},
		{"createAccount", ix(systemProgram, cat(le32(0), le64(2039280), le64(165), raw(tokenProgram)), sw(wallet), sw(other)),
			[]string{"System Program: createAccount", "lamports: 2039280", "space: 165", "owner: " + tokenProgram,
				"funding account: " + wallet, "new account: " + other + " (signer, writable)"}},
		{"assign", ix(systemProgram, cat(le32(1), raw(tokenProgram)), sw(other)),
			[]string{"System Program: assign", "owner: " + tokenProgram, "assigned account: " + other}},
		{"allocate", ix(systemProgram, cat(le32(8), le64(200)), sw(other)),
			[]string{"System Program: allocate", "space: 200", "allocated account: " + other}},
		{"advanceNonceAccount", ix(systemProgram, le32(4), w(other), r("SysvarRecentB1ockHashes11111111111111111111"), s(wallet)),
			[]string{"System Program: advanceNonceAccount", "nonce account: " + other, "nonce authority: " + wallet + " (signer)"}},
		{"createAccountWithSeed", ix(systemProgram, cat(le32(3), raw(wallet), bincodeStr("vault"), le64(1), le64(2), raw(tokenProgram)), sw(wallet), w(other)),
			[]string{"System Program: createAccountWithSeed", "base: " + wallet, `seed: "vault"`, "lamports: 1", "space: 2", "owner: " + tokenProgram,
				"funding account: " + wallet, "created account: " + other}},
		{"allocateWithSeed", ix(systemProgram, cat(le32(9), raw(wallet), bincodeStr("a"), le64(10), raw(tokenProgram)), w(other), s(wallet)),
			[]string{"System Program: allocateWithSeed", `seed: "a"`, "space: 10", "allocated account: " + other, "base account: " + wallet}},
		{"assignWithSeed", ix(systemProgram, cat(le32(10), raw(wallet), bincodeStr("b"), raw(tokenProgram)), w(other), s(wallet)),
			[]string{"System Program: assignWithSeed", `seed: "b"`, "owner: " + tokenProgram, "assigned account: " + other}},
		{"transferWithSeed", ix(systemProgram, cat(le32(11), le64(5), bincodeStr("c"), raw(tokenProgram)), w(other), s(wallet), w(third)),
			[]string{"System Program: transferWithSeed", "lamports: 5", `from seed: "c"`, "from owner: " + tokenProgram,
				"source: " + other, "base account: " + wallet, "destination: " + third}},
	} {
		t.Run(c.name, func(t *testing.T) { says(t, decodeIx(t, cs, c.ix), c.want...) })
	}
}

func TestDecodeInstructionReadsSPLToken(t *testing.T) {
	cs := solTx(t)
	for _, c := range []struct {
		name string
		ix   map[string]any
		want []string
	}{
		{"transfer", ix(tokenProgram, cat([]byte{3}, le64(42)), w(walletUSDC), w(other), s(wallet)),
			[]string{"SPL Token: transfer", "amount: 42 base units", "source: " + walletUSDC, "destination: " + other, "authority: " + wallet + " (signer)"}},
		{"transferChecked", ix(tokenProgram, cat([]byte{12}, le64(1_234_567), []byte{6}), w(walletUSDC), r(usdc), w(other), s(wallet)),
			[]string{"SPL Token: transferChecked", "amount: 1234567 base units (1.234567 at 6 decimals)", "decimals: 6", "mint: " + usdc}},
		{"approve", ix(tokenProgram, cat([]byte{4}, le64(100)), w(walletUSDC), r(other), s(wallet)),
			[]string{"SPL Token: approve", "delegate: " + other, "SENSITIVE", "delegate " + other + " may move up to 100 base units"}},
		{"approveChecked", ix(tokenProgram, cat([]byte{13}, le64(18446744073709551615), []byte{6}), w(walletUSDC), r(usdc), r(other), s(wallet)),
			[]string{"SPL Token: approveChecked", "SENSITIVE", "the most a u64 holds", "may move an unlimited amount"}},
		{"revoke", ix(tokenProgram, []byte{5}, w(walletUSDC), s(wallet)),
			[]string{"SPL Token: revoke", "source: " + walletUSDC, "owner: " + wallet}},
		{"setAuthority", ix(tokenProgram, cat([]byte{6, 2, 1}, raw(other)), w(walletUSDC), s(wallet)),
			[]string{"SPL Token: setAuthority", "authority type: account owner", "new authority: " + other, "SENSITIVE"}},
		{"setAuthority to none", ix(tokenProgram, []byte{6, 0, 0}, w(usdc), s(wallet)),
			[]string{"new authority: none", "SENSITIVE", "removes"}},
		{"mintTo", ix(tokenProgram, cat([]byte{7}, le64(9)), w(usdc), w(walletUSDC), s(wallet)),
			[]string{"SPL Token: mintTo", "amount: 9 base units", "mint: " + usdc, "destination: " + walletUSDC, "mint authority: " + wallet}},
		{"burn", ix(tokenProgram, cat([]byte{8}, le64(9)), w(walletUSDC), w(usdc), s(wallet)),
			[]string{"SPL Token: burn", "account: " + walletUSDC, "mint: " + usdc}},
		{"closeAccount to owner", ix(tokenProgram, []byte{9}, w(walletUSDC), w(wallet), s(wallet)),
			[]string{"SPL Token: closeAccount", "destination: " + wallet}},
		{"freezeAccount", ix(tokenProgram, []byte{10}, w(walletUSDC), r(usdc), s(wallet)),
			[]string{"SPL Token: freezeAccount", "freeze authority: " + wallet}},
		{"thawAccount", ix(tokenProgram, []byte{11}, w(walletUSDC), r(usdc), s(wallet)),
			[]string{"SPL Token: thawAccount"}},
		{"initializeAccount", ix(tokenProgram, []byte{1}, w(walletUSDC), r(usdc), r(wallet), r("SysvarRent111111111111111111111111111111111")),
			[]string{"SPL Token: initializeAccount", "account: " + walletUSDC, "owner: " + wallet}},
		{"initializeAccount2", ix(tokenProgram, cat([]byte{16}, raw(wallet)), w(walletUSDC), r(usdc), r("SysvarRent111111111111111111111111111111111")),
			[]string{"SPL Token: initializeAccount2", "owner: " + wallet}},
		{"initializeAccount3", ix(tokenProgram, cat([]byte{18}, raw(wallet)), w(walletUSDC), r(usdc)),
			[]string{"SPL Token: initializeAccount3", "owner: " + wallet}},
		{"Token-2022 transferChecked", ix(token2022, cat([]byte{12}, le64(5), []byte{0}), w(walletUSDC), r(usdc), w(other), s(wallet)),
			[]string{"SPL Token-2022: transferChecked", "5 base units (5 at 0 decimals)"}},
		{"multisig", ix(tokenProgram, cat([]byte{3}, le64(1)), w(walletUSDC), w(other), r(third), s(wallet), s(other)),
			[]string{"authority: " + third + " (", "multisig signer: " + wallet + " (signer)", "multisig signer: " + other + " (signer)"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := decodeIx(t, cs, c.ix)
			says(t, text, c.want...)
			if !slices.Contains(c.want, "SENSITIVE") && strings.Contains(text, "SENSITIVE") {
				t.Errorf("marked sensitive:\n%s", text)
			}
		})
	}
}

// Closing an account sends its rent to the destination: anyone but the
// owner is someone else taking it.
func TestCloseAccountToAnotherIsSensitive(t *testing.T) {
	text := decodeIx(t, solTx(t), ix(tokenProgram, []byte{9}, w(walletUSDC), w(other), s(wallet)))
	says(t, text, "SENSITIVE", other, "not its owner or close authority "+wallet)
}

func TestDecodeInstructionReadsTheAssociatedTokenAccountProgram(t *testing.T) {
	cs := solTx(t)
	create := []acct{sw(wallet), w(walletUSDC), r(wallet), r(usdc), r(systemProgram), r(tokenProgram)}
	for _, c := range []struct {
		name string
		data []byte
		accs []acct
		want []string
	}{
		{"create, empty data", nil, create, []string{"Associated Token Account: create", "funding account: " + wallet,
			"associated token account: " + walletUSDC, "wallet: " + wallet, "mint: " + usdc, "token program: " + tokenProgram}},
		{"create", []byte{0}, create, []string{"Associated Token Account: create"}},
		{"createIdempotent", []byte{1}, create, []string{"Associated Token Account: createIdempotent"}},
		{"recoverNested", []byte{2}, []acct{w(other), r(usdc), w(third), r(walletUSDC), r(usdc), sw(wallet), r(tokenProgram)},
			[]string{"Associated Token Account: recoverNested", "nested account: " + other, "destination: " + third, "wallet: " + wallet}},
	} {
		t.Run(c.name, func(t *testing.T) { says(t, decodeIx(t, cs, ix(ataProgram, c.data, c.accs...)), c.want...) })
	}
}

func TestDecodeInstructionNamesWhatItDoesNotKnow(t *testing.T) {
	cs := solTx(t)
	says(t, decodeIx(t, cs, ix(third, []byte{1, 2, 3}, w(other), s(wallet))),
		"program "+third+" is unknown", "account 1: "+other+" (writable)", "account 2: "+wallet+" (signer)", "data (3 bytes): 010203")
	says(t, decodeIx(t, cs, ix(systemProgram, le32(12), w(other))), "System Program", "instruction 12 is unknown")
	says(t, decodeIx(t, cs, ix(tokenProgram, []byte{40}, w(other))), "SPL Token", "instruction 40 is unknown")
	says(t, decodeIx(t, cs, ix(tokenProgram, []byte{3, 1}, w(other))), "SPL Token: transfer", "does not decode")
	says(t, decodeIx(t, cs, ix(tokenProgram, cat([]byte{3}, le64(1), []byte{7}), w(walletUSDC), w(other), s(wallet))),
		"1 extra byte")
	says(t, decodeIx(t, cs, ix(tokenProgram, cat([]byte{3}, le64(1)), w(walletUSDC))),
		"expects at least 3 accounts, has 1")
	if text, failed := call(t, cs, "decode_instruction", map[string]any{"instruction": ix("nope", nil)}); !failed {
		t.Errorf("a bad program id was taken: %s", text)
	}
}

func TestDecodeTransactionNamesTheFeePayerAndOtherSigners(t *testing.T) {
	cs := solTx(t)
	intent := map[string]any{
		"fee_payer":        wallet,
		"recent_blockhash": other,
		"cluster":          "devnet",
		"instructions": []any{
			ix(systemProgram, cat(le32(2), le64(1)), sw(wallet), w(other)),
			ix(tokenProgram, cat([]byte{3}, le64(1)), w(walletUSDC), w(other), s(third)),
		},
	}
	text := ok(t, cs, "decode_transaction", map[string]any{"intent": intent})
	says(t, text, "fee payer: "+wallet, "cluster: devnet",
		"instruction 1: System Program: transfer", "instruction 2: SPL Token: transfer",
		"signers besides the fee payer: "+third+" (instruction 2)")

	intent["instructions"] = []any{ix(systemProgram, cat(le32(2), le64(1)), sw(wallet), w(other))}
	says(t, ok(t, cs, "decode_transaction", map[string]any{"intent": intent}), "no signer besides the fee payer")
}

func TestSolanaAddressSaysWhetherItIsOnTheCurve(t *testing.T) {
	cs := solTx(t)
	says(t, ok(t, cs, "address", map[string]any{"address": wallet}), "valid", "on the Ed25519 curve")
	says(t, ok(t, cs, "address", map[string]any{"address": walletUSDC}), "valid", "off the Ed25519 curve", "program-derived address")
	for _, bad := range []string{"0OIl", "1111", wallet + "1", "not base58!"} {
		if text, failed := call(t, cs, "address", map[string]any{"address": bad}); !failed {
			t.Errorf("%q was taken: %s", bad, text)
		}
	}
}

func TestSolanaUnitsConvertExactly(t *testing.T) {
	cs := solTx(t)
	for _, c := range []struct {
		amount, from, to string
		decimals         any
		want             string
	}{
		{"1.5", "sol", "lamports", nil, "1500000000"},
		{"1", "lamports", "sol", nil, "0.000000001"},
		{"1234567", "base", "whole", 6, "1.234567"},
		{"2.5", "whole", "base", 9, "2500000000"},
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
		{"amount": "0.5", "from": "lamports", "to": "sol"},
		{"amount": "1", "from": "whole", "to": "base"},
		{"amount": "1", "from": "wei", "to": "sol"},
	} {
		if text, failed := call(t, cs, "units", args); !failed {
			t.Errorf("%v was taken: %s", args, text)
		}
	}
}
