package signreq

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/network"
	coslv1 "defios.com/proto/cosl/v1"
)

// The vectors below were produced independently with btcd's txscript
// (CalcWitnessSigHash, RFC 6979 signatures) over the same intent, for the key
// whose secret is 1.
const (
	btcPubkey = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	btcSPK    = "0014751e76e8199196d454941c45d1b3a323f1433bd6" // P2WPKH of btcPubkey
)

// btcIntent spends two of the Key's P2WPKH outputs to a P2WPKH, a P2PKH, a P2SH
// and a P2TR output; the 10000 sats left over are the fee.
func btcIntent() *coslv1.BitcoinIntent {
	spk, _ := hex.DecodeString(btcSPK)
	return &coslv1.BitcoinIntent{
		Inputs: []*coslv1.BitcoinInput{
			{Txid: "aa" + strings.Repeat("00", 31), Vout: 0, AmountSats: 100000, ScriptPubkey: spk},
			{Txid: "bb" + strings.Repeat("00", 31), Vout: 1, AmountSats: 50000, ScriptPubkey: spk},
		},
		Outputs: []*coslv1.BitcoinOutput{
			{Address: "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", AmountSats: 70000},
			{Address: "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2", AmountSats: 30000},
			{Address: "3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy", AmountSats: 20000},
			{Address: "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0", AmountSats: 20000},
		},
	}
}

func TestBitcoinDigestsAreOneBIP143SighashPerInput(t *testing.T) {
	got, err := BitcoinDigests("mainnet", btcIntent())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"2d7df99e3bee51c4a1dc2aa95b02389f981ad811ab69b609eeae898d339eeb17",
		"1d92577caf8f09b1bc8e4dfe1241a733bf7626fd4e8ef28ab8a8e7dea95196b0",
	}
	if len(got) != len(want) {
		t.Fatalf("%d digests, want %d", len(got), len(want))
	}
	for i := range want {
		if hex.EncodeToString(got[i][:]) != want[i] {
			t.Errorf("digest %d\n got %x\nwant %s", i, got[i], want[i])
		}
	}
}

// Valid pairs are from BIP-173 and BIP-350; the base58 ones from btcd.
func TestOutputScript(t *testing.T) {
	good := []struct{ net, address, script string }{
		{"mainnet", "BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4", "0014751e76e8199196d454941c45d1b3a323f1433bd6"},
		{"mainnet", "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", "0014751e76e8199196d454941c45d1b3a323f1433bd6"},
		{"testnet", "tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k7", "00201863143c14c5166804bd19203356da136c985678cd4d27a1b8c6329604903262"},
		{"signet", "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", "0014751e76e8199196d454941c45d1b3a323f1433bd6"},
		{"regtest", "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080", "0014751e76e8199196d454941c45d1b3a323f1433bd6"},
		{"mainnet", "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0", "512079be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"},
		{"mainnet", "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2", "76a91477bff20c60e522dfaa3350c39b030a5d004e839a88ac"},
		{"mainnet", "3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy", "a914b472a266d0bd89c13706a4132ccfb16f7c3b9fcb87"},
		{"testnet", "mipcBbFg9gMiCh81Kj8tqqdgoZub1ZJRfn", "76a914243f1394f44554f4ce3fd68649c19adc483ce92488ac"},
		{"testnet", "2MzQwSSnBHWHqSAqtTVQ6v47XtaisrJa1Vc", "a9144e9f39ca4688ff102128ea4ccda34105324305b087"},
	}
	for _, c := range good {
		got, err := OutputScript(c.net, c.address)
		if err != nil || hex.EncodeToString(got) != c.script {
			t.Errorf("%s %s = %x, %v; want %s", c.net, c.address, got, err, c.script)
		}
	}

	bad := []struct{ name, net, address string }{
		{"mainnet address on testnet", "testnet", "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
		{"testnet address on mainnet", "mainnet", "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"},
		{"base58 mainnet on regtest", "regtest", "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"},
		{"bech32 checksum", "mainnet", "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t5"},
		{"mixed case", "mainnet", "bc1QW508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
		{"v0 in bech32m", "mainnet", "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7k7grplx"},
		{"v1 in bech32", "mainnet", "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqh2y7hd"},
		{"v0 program of 16 bytes", "mainnet", "BC1QR508D6QEJXTDG4Y5R3ZARVARYV98GJ9P"},
		{"future witness version", "mainnet", "bc1zw508d6qejxtdg4y5r3zarvaryvaxxpcs"},
		{"base58 checksum", "mainnet", "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN3"},
		{"not an address", "mainnet", "hello"},
		{"empty", "mainnet", ""},
	}
	for _, c := range bad {
		if got, err := OutputScript(c.net, c.address); err == nil {
			t.Errorf("%s: %q accepted as %x", c.name, c.address, got)
		}
	}
	if _, err := OutputScript("dogecoin", "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"); err == nil {
		t.Error("an unknown bitcoin network was accepted")
	}
}

// r||s of the two DER signatures btcd produced for btcIntent's Digests, with
// the recovery id (unused by a Bitcoin witness) appended.
func btcSigs() [][]byte {
	rs := []string{
		"3d8125b6903785a0a61a2b84cbd88d9979a11043b04829bf0dc7e8c7c802db75" + "0d5725092c5ec0a4f07e3b820525017462921f1fd904334fd7284da7e60d44fb",
		"8be26e2056dfc2aa6db9200840a3a71413dcd81f8efc6684198c4e4943909f78" + "4801e044798f0f11758683af8b9b8d3f323025ce186b1d04b1fd35a2d3caf0f5",
	}
	var out [][]byte
	for _, s := range rs {
		b, _ := hex.DecodeString(s)
		out = append(out, append(b, 0))
	}
	return out
}

func TestAssembleBitcoinMatchesIndependentSigner(t *testing.T) {
	pub, _ := hex.DecodeString(btcPubkey)
	raw, txid, err := AssembleBitcoin("mainnet", pub, btcIntent(), btcSigs())
	if err != nil {
		t.Fatal(err)
	}
	const wantRaw = "0200000000010200000000000000000000000000000000000000000000000000000000000000aa0000000000fdffffff00000000000000000000000000000000000000000000000000000000000000bb0100000000fdffffff047011010000000000160014751e76e8199196d454941c45d1b3a323f1433bd630750000000000001976a91477bff20c60e522dfaa3350c39b030a5d004e839a88ac204e00000000000017a914b472a266d0bd89c13706a4132ccfb16f7c3b9fcb87204e00000000000022512079be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f817980247304402203d8125b6903785a0a61a2b84cbd88d9979a11043b04829bf0dc7e8c7c802db7502200d5725092c5ec0a4f07e3b820525017462921f1fd904334fd7284da7e60d44fb01210279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798024830450221008be26e2056dfc2aa6db9200840a3a71413dcd81f8efc6684198c4e4943909f7802204801e044798f0f11758683af8b9b8d3f323025ce186b1d04b1fd35a2d3caf0f501210279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f8179800000000"
	if got := hex.EncodeToString(raw); got != wantRaw {
		t.Errorf("raw tx\n got %s\nwant %s", got, wantRaw)
	}
	if got := hex.EncodeToString(txid[:]); got != "1d52ae70b504bc96eede010a0e2e7ca6642da14ea89d3d360aa24fbeb7359779" {
		t.Errorf("txid %s", got)
	}
}

func TestAssembleBitcoinRefusesWhatCannotBeSigned(t *testing.T) {
	pub, _ := hex.DecodeString(btcPubkey)
	other := append([]byte{0x03}, pub[1:]...) // not the key the inputs pay
	badV := btcSigs()
	badV[1][64] = 2
	cases := []struct {
		name string
		pub  []byte
		sigs [][]byte
	}{
		{"one signature for two inputs", pub, btcSigs()[:1]},
		{"a 64-byte signature", pub, [][]byte{btcSigs()[0][:64], btcSigs()[1]}},
		{"recovery id 2", pub, badV},
		{"a key the inputs do not pay", other, btcSigs()},
	}
	for _, c := range cases {
		if _, _, err := AssembleBitcoin("mainnet", c.pub, btcIntent(), c.sigs); err == nil {
			t.Errorf("%s was assembled", c.name)
		}
	}
}

// A request mixing Ethereum and Bitcoin (both secp256k1) signs one Digest for
// the first and one per input for the second, so Signed wants three signatures.
func TestSignedCountsADigestPerBitcoinInput(t *testing.T) {
	btc := &coslv1.SignTransaction{NetworkConfigId: btcID[:], Intent: &coslv1.SignTransaction_Bitcoin{Bitcoin: btcIntent()}}
	r := rowOf(t, 1, 0xa1, time.Unix(10_000, 0), make([]byte, 32), req(ethTx(), btc))
	history := map[[32]byte]network.Network{ethID: nets[0], btcID: nets[1]}
	sig := make([]byte, 65)
	for _, n := range []int{2, 4} {
		_, err := Signed([]cosl.Row{r}, r.MsgID, make([][]byte, n), history)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "3 digests") {
			t.Errorf("%d signatures: %v", n, err)
		}
	}
	// Three is the right count: the Key is then looked up, and this log has none.
	if _, err := Signed([]cosl.Row{r}, r.MsgID, [][]byte{sig, sig, sig}, history); !errors.Is(err, key.ErrUnknown) {
		t.Errorf("three signatures: %v", err)
	}
}
