package builtinmcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"defios.com/internal/network"
	"defios.com/internal/signreq"
	coslv1 "defios.com/proto/cosl/v1"
)

// btcMaxFeeRate is the fee rate, in sat/vB, above which decode_transaction
// flags a fee as likely a mistake.
const btcMaxFeeRate = 1000

// btcMaxSats is every bitcoin there will ever be, in sats.
const btcMaxSats = 21_000_000 * 100_000_000

// bitcoinTx is the bitcoin Plugin's tx MCP Server: offline, decoding an
// intent's inputs, outputs and fee, and reading addresses and converting
// units (ADR 0042). It looks up no UTXO.
func bitcoinTx() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "bitcoin/tx", Version: "0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name: "decode_transaction",
		Description: "Decode a Bitcoin intent as sign_inspect returns it (protobuf-JSON: inputs with txid, vout, amount_sats and script_pubkey base64; outputs with address and amount_sats; network): " +
			"each input's script type and the address it spends from, each output's address type, the total in and out, the fee (refused if negative), " +
			"and an estimated vsize and fee rate in sat/vB, whose assumptions it states. " +
			"With key_address (the Key's address, from key_address), outputs paying it are marked change and inputs not spending from it FOREIGN. " +
			fmt.Sprintf("Flags an output to another network's address, a dust output, and a fee rate above %d sat/vB. ", btcMaxFeeRate) +
			"It looks up no UTXO: input amounts and scripts are as the intent states them.",
	}, decodeBitcoinTransaction)
	mcp.AddTool(s, &mcp.Tool{
		Name: "address",
		Description: "Validate a Bitcoin address and give its type (P2PKH, P2SH, P2WPKH, P2WSH or P2TR), the network(s) it belongs to, and the script it pays. " +
			"With network, also say whether it is an address of that network.",
	}, bitcoinAddress)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "units",
		Description: "Convert an amount exactly between sats and btc. The amount is decimal, with an optional fraction.",
	}, bitcoinUnits)
	return s
}

type btcTransactionIn struct {
	Intent     any    `json:"intent" jsonschema:"a Bitcoin intent as sign_inspect returns it: protobuf-JSON, script_pubkey base64"`
	KeyAddress string `json:"key_address,omitempty" jsonschema:"the Key's address on the intent's network, as key_address gives it"`
}

// btcScript is a script type decode_transaction knows: how an input
// spending it is sized, and an output paying it's dust threshold.
type btcScript struct {
	name string
	// scriptSig and witness are the assumed bytes of an input spending it.
	scriptSig, witness int
	// dust is Bitcoin Core's dust threshold at its 3 sat/vB dust relay fee.
	dust uint64
}

var (
	btcP2PKH  = btcScript{"P2PKH", 107, 0, 546}
	btcP2SH   = btcScript{"P2SH", 23, 108, 540}
	btcP2WPKH = btcScript{"P2WPKH", 0, 108, 294}
	btcP2WSH  = btcScript{"P2WSH", 0, 254, 330}
	btcP2TR   = btcScript{"P2TR", 0, 66, 330}
)

// btcSizeAssumptions are what the vsize estimate takes each input to carry.
const btcSizeAssumptions = "vsize assumes, per input: P2WPKH a 72-byte signature and 33-byte key in its witness; " +
	"P2PKH the same in its scriptSig; P2SH a nested P2WPKH; P2WSH a 2-of-3 multisig; P2TR a key-path spend with a 64-byte signature. " +
	"Signatures are often a byte shorter, so the true vsize may be slightly less and the fee rate slightly more"

// btcScripts are the script types decode_transaction knows, by name.
var btcScripts = map[string]btcScript{}

func init() {
	for _, s := range []btcScript{btcP2PKH, btcP2SH, btcP2WPKH, btcP2WSH, btcP2TR} {
		btcScripts[s.name] = s
	}
}

// btcClassify is the script type of script, and whether it is one known.
func btcClassify(script []byte) (btcScript, bool) {
	s, ok := btcScripts[signreq.ScriptType(script)]
	return s, ok
}

// btcNetwork refuses a name that is no bitcoin network.
func btcNetwork(what, net string) error {
	if !slices.Contains(network.BitcoinNetworks(), net) {
		return fmt.Errorf("%s is %q, not one of %s", what, net, strings.Join(network.BitcoinNetworks(), ", "))
	}
	return nil
}

func decodeBitcoinTransaction(_ context.Context, _ *mcp.CallToolRequest, in btcTransactionIn) (*mcp.CallToolResult, any, error) {
	var intent coslv1.BitcoinIntent
	if err := protoArg(in.Intent, &intent); err != nil {
		return nil, nil, fmt.Errorf("the intent is not a Bitcoin intent as sign_inspect returns it: %w", err)
	}
	net := intent.Network
	if err := btcNetwork("the intent's network", net); err != nil {
		return nil, nil, err
	}
	var keyScript []byte
	if k := strings.TrimSpace(in.KeyAddress); k != "" {
		var err error
		if keyScript, err = signreq.OutputScript(net, k); err != nil {
			return nil, nil, fmt.Errorf("key_address: %w", err)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "network: %s\n", net)
	if net != "mainnet" {
		b.WriteString("testnet, signet and regtest share base58 addresses, and testnet and signet bech32 ones, so an output to another of them is not flagged\n")
	}
	if intent.LockTime != 0 {
		fmt.Fprintf(&b, "lock_time: %d\n", intent.LockTime)
	}
	// sizable is whether every input and output has a known size.
	sizable := true
	var spent []btcScript
	var outScripts [][]byte
	var totalIn, totalOut uint64

	fmt.Fprintf(&b, "\n%s\n", plural(len(intent.Inputs), "input"))
	for i, x := range intent.Inputs {
		if x.AmountSats > btcMaxSats || totalIn+x.AmountSats > btcMaxSats {
			return nil, nil, fmt.Errorf("input %d: the inputs total more than 21 million BTC", i)
		}
		totalIn += x.AmountSats
		fmt.Fprintf(&b, "input %d: %s:%d, %d sats\n", i, orNone(x.Txid), x.Vout, x.AmountSats)
		st, known := btcClassify(x.ScriptPubkey)
		if known {
			addr, _ := signreq.ScriptAddress(net, x.ScriptPubkey)
			fmt.Fprintf(&b, "  spends %s from %s\n", st.name, addr)
			spent = append(spent, st)
		} else {
			fmt.Fprintf(&b, "  spends an unknown script type, script_pubkey %x\n", x.ScriptPubkey)
			sizable = false
		}
		if keyScript != nil && !bytes.Equal(x.ScriptPubkey, keyScript) {
			b.WriteString("  FOREIGN: not spent from key_address, so the Key cannot sign for it\n")
		}
	}

	fmt.Fprintf(&b, "\n%s\n", plural(len(intent.Outputs), "output"))
	for i, o := range intent.Outputs {
		if o.AmountSats > btcMaxSats || totalOut+o.AmountSats > btcMaxSats {
			return nil, nil, fmt.Errorf("output %d: the outputs total more than 21 million BTC", i)
		}
		totalOut += o.AmountSats
		fmt.Fprintf(&b, "output %d: %d sats to %s\n", i, o.AmountSats, orNone(o.Address))
		script, err := signreq.OutputScript(net, o.Address)
		if err != nil {
			others := addressNetworks(o.Address)
			if len(others) == 0 {
				fmt.Fprintf(&b, "  FLAG: not a valid address: %v\n", err)
				sizable = false
				continue
			}
			// sized as it reads on its own network
			script, _ = signreq.OutputScript(others[0], o.Address)
			st, _ := btcClassify(script)
			fmt.Fprintf(&b, "  %s on %s\n  FLAG: an address of %s, not of %s: the transaction cannot pay it\n",
				st.name, strings.Join(others, " or "), strings.Join(others, " or "), net)
			outScripts = append(outScripts, script)
			continue
		}
		outScripts = append(outScripts, script)
		st, _ := btcClassify(script)
		fmt.Fprintf(&b, "  %s on %s\n", st.name, net)
		if keyScript != nil && bytes.Equal(script, keyScript) {
			b.WriteString("  change: pays key_address\n")
		}
		if o.AmountSats < st.dust {
			fmt.Fprintf(&b, "  FLAG: dust: below the %d-sat threshold for %s at Bitcoin Core's 3 sat/vB dust relay fee, so nodes will not relay it\n", st.dust, st.name)
		}
	}

	fmt.Fprintf(&b, "\ntotal in: %d sats\ntotal out: %d sats\n", totalIn, totalOut)
	if totalOut > totalIn {
		return nil, nil, fmt.Errorf("the outputs total %d sats, more than the inputs' %d: the fee would be negative (%d sats)", totalOut, totalIn, int64(totalIn)-int64(totalOut))
	}
	fee := totalIn - totalOut
	fmt.Fprintf(&b, "fee: %d sats (%s BTC)\n", fee, decimalString(new(big.Rat).SetFrac64(int64(fee), 100_000_000), 8))
	if !sizable {
		b.WriteString("no vsize or fee rate estimate: an input or output above is of no known script type\n")
		return text(b.String()), nil, nil
	}
	vsize := estimateVsize(spent, outScripts)
	rate := new(big.Rat).SetFrac64(int64(fee), int64(vsize))
	fmt.Fprintf(&b, "estimated vsize: %d vB\nestimated fee rate: %s sat/vB\n", vsize, decimalString(rate, 2))
	if rate.Cmp(big.NewRat(btcMaxFeeRate, 1)) > 0 {
		fmt.Fprintf(&b, "FLAG: the fee rate is above %d sat/vB, far above what any block has needed for long: likely a mistake\n", btcMaxFeeRate)
	}
	b.WriteString(btcSizeAssumptions + "\n")
	return text(b.String()), nil, nil
}

// estimateVsize is the virtual size, in vB, of a version-2 transaction
// spending inputs of the script types spent and paying outputs, each input
// carrying what its type assumes.
func estimateVsize(spent []btcScript, outputs [][]byte) int {
	// version, lock_time and the counts
	base := 4 + 4 + btcVarIntLen(len(spent)) + btcVarIntLen(len(outputs))
	witness := 0
	segwit := false
	for _, s := range spent {
		// outpoint, sequence and the scriptSig
		base += 32 + 4 + 4 + btcVarIntLen(s.scriptSig) + s.scriptSig
		segwit = segwit || s.witness > 0
	}
	for _, o := range outputs {
		base += 8 + btcVarIntLen(len(o)) + len(o)
	}
	if segwit {
		witness = 2 // marker and flag
		for _, s := range spent {
			// an input with no witness still counts its empty stack
			witness += max(s.witness, 1)
		}
	}
	return (base*4 + witness + 3) / 4
}

func btcVarIntLen(n int) int {
	switch {
	case n < 0xfd:
		return 1
	case n <= 0xffff:
		return 3
	}
	return 5
}

// addressNetworks are the bitcoin networks address is an address of.
func addressNetworks(address string) []string {
	var nets []string
	for _, net := range network.BitcoinNetworks() {
		if _, err := signreq.OutputScript(net, address); err == nil {
			nets = append(nets, net)
		}
	}
	return nets
}

type btcAddressIn struct {
	Address string `json:"address" jsonschema:"a Bitcoin address"`
	Network string `json:"network,omitempty" jsonschema:"mainnet, testnet, signet or regtest: the network it should belong to"`
}

func bitcoinAddress(_ context.Context, _ *mcp.CallToolRequest, in btcAddressIn) (*mcp.CallToolResult, any, error) {
	a := strings.TrimSpace(in.Address)
	if in.Network != "" {
		if err := btcNetwork("network", in.Network); err != nil {
			return nil, nil, err
		}
	}
	nets := addressNetworks(a)
	if len(nets) == 0 {
		// the reason the network its prefix names gives, else mainnet's
		net := "mainnet"
		switch lower := strings.ToLower(a); {
		case strings.HasPrefix(lower, "bcrt1"):
			net = "regtest"
		case strings.HasPrefix(lower, "tb1"):
			net = "testnet"
		}
		_, err := signreq.OutputScript(net, a)
		return nil, nil, fmt.Errorf("not a Bitcoin address: %w", err)
	}
	script, _ := signreq.OutputScript(nets[0], a)
	st, _ := btcClassify(script)
	var b strings.Builder
	fmt.Fprintf(&b, "valid: %s\nnetwork: %s\nscript: %x\n", st.name, strings.Join(nets, " or "), script)
	if len(nets) > 1 {
		b.WriteString("those networks share this address form, so the address alone does not say which it is meant for\n")
	}
	if in.Network != "" {
		if slices.Contains(nets, in.Network) {
			fmt.Fprintf(&b, "an address of %s\n", in.Network)
		} else {
			fmt.Fprintf(&b, "NOT an address of %s: a transaction on %s cannot pay it\n", in.Network, in.Network)
		}
	}
	return text(b.String()), nil, nil
}

type btcUnitsIn struct {
	Amount string `json:"amount" jsonschema:"a decimal amount, such as 1.5"`
	From   string `json:"from" jsonschema:"sats or btc"`
	To     string `json:"to" jsonschema:"sats or btc"`
}

// btcUnits are Bitcoin's units, smallest first.
var btcUnits = []unit{{"sats", 0}, {"btc", 8}}

func bitcoinUnits(_ context.Context, _ *mcp.CallToolRequest, in btcUnitsIn) (*mcp.CallToolResult, any, error) {
	from, to := strings.ToLower(in.From), strings.ToLower(in.To)
	for _, u := range []string{from, to} {
		if u != "sats" && u != "btc" {
			return nil, nil, errors.New("the units are sats and btc")
		}
	}
	return convert(unitsIn{Amount: in.Amount, From: from, To: to}, btcUnits)
}
