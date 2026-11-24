package builtinmcp_test

import (
	"context"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"defios.com/internal/signreq"
)

// BIP-173 and BIP-350's test vectors, and their scripts.
const (
	mainP2WPKH = "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"
	testP2WPKH = "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	mainP2WSH  = "bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3"
	mainP2TR   = "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0"
	mainP2PKH  = "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"
	mainP2SH   = "3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy"
	testP2PKH  = "mipcBbFg9gMiCh81Kj8tqqdgoZub1ZJRfn"

	p2wpkhScript = "0014751e76e8199196d454941c45d1b3a323f1433bd6"
	// another key hash, which the Key does not hold
	foreignScript = "00140000000000000000000000000000000000000001"
)

func btcTx(t *testing.T) *mcp.ClientSession { return connect(t, "bitcoin/tx") }

func scriptB64(h string) string {
	b, err := hex.DecodeString(h)
	if err != nil {
		panic(err)
	}
	return b64(b)
}

func btcIn(script string, sats uint64, vout int) map[string]any {
	return map[string]any{
		"txid":          "4a5e1e4baab89f3a32518a88c31bc87f618f76673e2cc77ab2127b7afdeda33b",
		"vout":          vout,
		"amount_sats":   sats,
		"script_pubkey": scriptB64(script),
	}
}

func btcOut(address string, sats uint64) map[string]any {
	return map[string]any{"address": address, "amount_sats": sats}
}

func btcIntent(net string, ins []any, outs []any) map[string]any {
	return map[string]any{"network": net, "inputs": ins, "outputs": outs}
}

func TestBitcoinTxOffersItsTools(t *testing.T) {
	res, err := btcTx(t).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"address", "decode_transaction", "units"}) {
		t.Fatalf("tools %v", names)
	}
}

// One P2WPKH input paying a P2WPKH and a P2TR output: 125 bytes without
// witness, 110 of witness (marker, flag, and a 72-byte signature and
// 33-byte key), so 610 weight units, 152.5 rounded up to 153 vB. A fee of
// 1530 sats is 10 sat/vB.
func TestDecodeBitcoinTransactionGivesTheFeeAndFeeRate(t *testing.T) {
	text := ok(t, btcTx(t), "decode_transaction", map[string]any{"intent": btcIntent("mainnet",
		[]any{btcIn(p2wpkhScript, 100_000, 0)},
		[]any{btcOut(mainP2WPKH, 50_000), btcOut(mainP2TR, 48_470)})})
	says(t, text,
		"P2WPKH", mainP2WPKH, "P2TR",
		"total in: 100000 sats", "total out: 98470 sats", "fee: 1530 sats",
		"153 vB", "10 sat/vB")
}

// With key_address, the output paying it is change and an input not
// spending from it is foreign.
func TestDecodeBitcoinTransactionMarksChangeAndForeignInputs(t *testing.T) {
	text := ok(t, btcTx(t), "decode_transaction", map[string]any{
		"key_address": mainP2WPKH,
		"intent": btcIntent("mainnet",
			[]any{btcIn(p2wpkhScript, 60_000, 0), btcIn(foreignScript, 40_000, 1)},
			[]any{btcOut(mainP2TR, 70_000), btcOut(mainP2WPKH, 29_000)}),
	})
	in1, out := text[strings.Index(text, "input 1:"):strings.Index(text, "outputs")], text[strings.Index(text, "outputs"):]
	says(t, in1, "FOREIGN")
	if strings.Count(text, "FOREIGN") != 1 {
		t.Errorf("not exactly input 1 is foreign:\n%s", text)
	}
	says(t, out[strings.Index(out, "output 1:"):], "change")
	if strings.Count(text, "change") != 1 {
		t.Errorf("not exactly output 1 is change:\n%s", text)
	}
}

func TestDecodeBitcoinTransactionFlagsAnotherNetworksAddress(t *testing.T) {
	text := ok(t, btcTx(t), "decode_transaction", map[string]any{"intent": btcIntent("mainnet",
		[]any{btcIn(p2wpkhScript, 100_000, 0)},
		[]any{btcOut(testP2WPKH, 50_000), btcOut(mainP2WPKH, 49_000)})})
	says(t, text, "P2WPKH on testnet or signet", "FLAG: an address of testnet or signet, not of mainnet", "estimated fee rate")
}

func TestDecodeBitcoinTransactionRefusesANegativeFee(t *testing.T) {
	text, failed := call(t, btcTx(t), "decode_transaction", map[string]any{"intent": btcIntent("mainnet",
		[]any{btcIn(p2wpkhScript, 100_000, 0)},
		[]any{btcOut(mainP2WPKH, 100_001)})})
	if !failed {
		t.Fatalf("a negative fee was taken:\n%s", text)
	}
	says(t, text, "negative")
}

func TestDecodeBitcoinTransactionFlagsDustAndAnExcessiveFeeRate(t *testing.T) {
	text := ok(t, btcTx(t), "decode_transaction", map[string]any{"intent": btcIntent("mainnet",
		[]any{btcIn(p2wpkhScript, 1_000_000, 0)},
		[]any{btcOut(mainP2WPKH, 293), btcOut(mainP2TR, 330)})})
	says(t, text, "FLAG: dust: below the 294-sat threshold for P2WPKH", "above 1000 sat/vB")
	if strings.Count(text, "FLAG: dust") != 1 {
		t.Errorf("an output at its dust threshold was flagged:\n%s", text)
	}
}

// Each input says the address it spends from, on the intent's network.
func TestDecodeBitcoinTransactionNamesEachInputsAddress(t *testing.T) {
	cs := btcTx(t)
	for _, c := range []struct{ net, addr, typ string }{
		{"mainnet", mainP2PKH, "P2PKH"},
		{"mainnet", mainP2SH, "P2SH"},
		{"mainnet", mainP2WPKH, "P2WPKH"},
		{"mainnet", mainP2WSH, "P2WSH"},
		{"mainnet", mainP2TR, "P2TR"},
		{"testnet", testP2PKH, "P2PKH"},
		{"testnet", testP2WPKH, "P2WPKH"},
	} {
		script, err := signreq.OutputScript(c.net, c.addr)
		if err != nil {
			t.Fatal(err)
		}
		text := ok(t, cs, "decode_transaction", map[string]any{"intent": btcIntent(c.net,
			[]any{btcIn(hex.EncodeToString(script), 100_000, 0)},
			[]any{btcOut(c.addr, 90_000)})})
		says(t, text, "spends "+c.typ+" from "+c.addr)
	}
}

func TestBitcoinAddressGivesTypeAndNetwork(t *testing.T) {
	cs := btcTx(t)
	says(t, ok(t, cs, "address", map[string]any{"address": mainP2TR}), "valid: P2TR", "network: mainnet\n")
	says(t, ok(t, cs, "address", map[string]any{"address": mainP2SH}), "valid: P2SH", "network: mainnet\n")
	says(t, ok(t, cs, "address", map[string]any{"address": mainP2WSH}), "valid: P2WSH")
	says(t, ok(t, cs, "address", map[string]any{"address": testP2WPKH}), "valid: P2WPKH", "network: testnet or signet\n")
	says(t, ok(t, cs, "address", map[string]any{"address": testP2PKH}), "valid: P2PKH", "testnet or signet or regtest")
	says(t, ok(t, cs, "address", map[string]any{"address": testP2WPKH, "network": "mainnet"}), "NOT an address of mainnet")
	says(t, ok(t, cs, "address", map[string]any{"address": mainP2PKH, "network": "mainnet"}), "an address of mainnet")
	for _, bad := range []string{mainP2WPKH[:len(mainP2WPKH)-1] + "5", "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN3", "hello"} {
		if text, failed := call(t, cs, "address", map[string]any{"address": bad}); !failed {
			t.Errorf("%s was taken: %s", bad, text)
		}
	}
}

func TestBitcoinUnitsConvertExactly(t *testing.T) {
	cs := btcTx(t)
	for _, c := range []struct{ amount, from, to, want string }{
		{"1.5", "BTC", "sats", "150000000"},
		{"1", "sats", "btc", "0.00000001"},
		{"2100000000000000", "sats", "btc", "21000000"},
	} {
		text := ok(t, cs, "units", map[string]any{"amount": c.amount, "from": c.from, "to": c.to})
		if !strings.HasPrefix(text, c.want+" "+c.to) {
			t.Errorf("%s %s in %s = %q, want %s", c.amount, c.from, c.to, text, c.want)
		}
	}
	for _, args := range []map[string]any{
		{"amount": "0.000000001", "from": "btc", "to": "sats"},
		{"amount": "1", "from": "base", "to": "whole"},
		{"amount": "1", "from": "mbtc", "to": "sats"},
	} {
		if text, failed := call(t, cs, "units", args); !failed {
			t.Errorf("%v was taken: %s", args, text)
		}
	}
}
