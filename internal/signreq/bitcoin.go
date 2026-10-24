package signreq

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/ripemd160"

	"defios.com/internal/base58"
	coslv1 "defios.com/proto/cosl/v1"
)

// sighashAll is the only sighash type a Digest uses.
const sighashAll = 1

// bitcoinSequence is every input's nSequence: final-but-for-RBF (BIP-125), and
// below 0xffffffff so a non-zero lock_time is enforced.
const bitcoinSequence = 0xfffffffd

// BitcoinDigests is the BIP-143 SIGHASH_ALL sighash of each input of in, in
// input order: one Digest per signature the transaction needs. Every input
// must spend a P2WPKH output (its script_pubkey is `0014<hash160>`), which is
// the one script type a Key maps to; net is the Network's bitcoin network and
// decides how output addresses read. Pure and deterministic.
func BitcoinDigests(net string, in *coslv1.BitcoinIntent) ([][32]byte, error) {
	tx, err := newBitcoinTx(net, in)
	if err != nil {
		return nil, err
	}
	var prevouts, sequences, outputs bytes.Buffer
	for _, x := range tx.inputs {
		prevouts.Write(x.outpoint)
		binary.Write(&sequences, binary.LittleEndian, uint32(bitcoinSequence))
	}
	for _, o := range tx.outputs {
		outputs.Write(o)
	}
	hashPrevouts, hashSequence, hashOutputs := doubleSHA(prevouts.Bytes()), doubleSHA(sequences.Bytes()), doubleSHA(outputs.Bytes())

	out := make([][32]byte, len(tx.inputs))
	for i, x := range tx.inputs {
		var b bytes.Buffer
		binary.Write(&b, binary.LittleEndian, int32(2))
		b.Write(hashPrevouts[:])
		b.Write(hashSequence[:])
		b.Write(x.outpoint)
		// scriptCode of a P2WPKH input is the P2PKH script of its key hash.
		b.Write([]byte{0x19, 0x76, 0xa9, 0x14})
		b.Write(x.keyHash)
		b.Write([]byte{0x88, 0xac})
		binary.Write(&b, binary.LittleEndian, x.amount)
		binary.Write(&b, binary.LittleEndian, uint32(bitcoinSequence))
		b.Write(hashOutputs[:])
		binary.Write(&b, binary.LittleEndian, in.LockTime)
		binary.Write(&b, binary.LittleEndian, uint32(sighashAll))
		out[i] = doubleSHA(b.Bytes())
	}
	return out, nil
}

// bitcoinTx is the parts of a transaction a sighash commits to, serialised.
type bitcoinTx struct {
	inputs  []bitcoinInput
	outputs [][]byte // amount (LE) || compact-size script length || script
}

type bitcoinInput struct {
	outpoint []byte // txid in internal byte order, then vout (LE)
	keyHash  []byte // the P2WPKH key hash the input's script_pubkey names
	amount   uint64
}

func newBitcoinTx(net string, in *coslv1.BitcoinIntent) (bitcoinTx, error) {
	var tx bitcoinTx
	for i, x := range in.Inputs {
		txid, err := hex.DecodeString(x.Txid)
		if err != nil || len(txid) != 32 {
			return tx, fmt.Errorf("input %d: txid must be 64 hex characters", i)
		}
		if len(x.ScriptPubkey) != 22 || x.ScriptPubkey[0] != 0x00 || x.ScriptPubkey[1] != 0x14 {
			return tx, fmt.Errorf("input %d: script_pubkey must be P2WPKH (0014<20-byte key hash>)", i)
		}
		for l, r := 0, len(txid)-1; l < r; l, r = l+1, r-1 { // display order -> internal order
			txid[l], txid[r] = txid[r], txid[l]
		}
		op := binary.LittleEndian.AppendUint32(txid, x.Vout)
		tx.inputs = append(tx.inputs, bitcoinInput{op, x.ScriptPubkey[2:], x.AmountSats})
	}
	for i, o := range in.Outputs {
		script, err := OutputScript(net, o.Address)
		if err != nil {
			return tx, fmt.Errorf("output %d: %v", i, err)
		}
		b := binary.LittleEndian.AppendUint64(nil, o.AmountSats)
		b = append(b, compactSize(len(script))...)
		tx.outputs = append(tx.outputs, append(b, script...))
	}
	return tx, nil
}

// compactSize is Bitcoin's variable-length integer, for lengths and counts.
func compactSize(n int) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		return binary.LittleEndian.AppendUint16([]byte{0xfd}, uint16(n))
	default:
		return binary.LittleEndian.AppendUint32([]byte{0xfe}, uint32(n))
	}
}

// doubleSHA is SHA-256 applied twice, Bitcoin's hash for sighashes and txids.
func doubleSHA(b []byte) [32]byte {
	first := sha256.Sum256(b)
	return sha256.Sum256(first[:])
}

// bitcoinHRP is the bech32 human-readable part of each bitcoin network.
var bitcoinHRP = map[string]string{"mainnet": "bc", "testnet": "tb", "signet": "tb", "regtest": "bcrt"}

// OutputScript is the scriptPubKey an address on the bitcoin network net pays
// to: P2PKH and P2SH (base58check), P2WPKH and P2WSH (bech32), P2TR (bech32m).
func OutputScript(net, address string) ([]byte, error) {
	hrp, ok := bitcoinHRP[net]
	if !ok {
		return nil, fmt.Errorf("unknown bitcoin network %q", net)
	}
	if strings.HasPrefix(strings.ToLower(address), hrp+"1") {
		return segwitScript(hrp, address)
	}
	raw, err := base58.DecodeN(address, 25)
	if err != nil {
		return nil, fmt.Errorf("address %q is not a %s address", address, net)
	}
	sum := doubleSHA(raw[:21])
	if !bytes.Equal(sum[:4], raw[21:]) {
		return nil, fmt.Errorf("address %q has a bad checksum", address)
	}
	p2pkh, p2sh := byte(0x00), byte(0x05)
	if net != "mainnet" {
		p2pkh, p2sh = 0x6f, 0xc4
	}
	switch raw[0] {
	case p2pkh:
		return append(append([]byte{0x76, 0xa9, 0x14}, raw[1:21]...), 0x88, 0xac), nil
	case p2sh:
		return append(append([]byte{0xa9, 0x14}, raw[1:21]...), 0x87), nil
	}
	return nil, fmt.Errorf("address %q is not a %s address", address, net)
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := range gen {
			if top>>i&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

// segwitScript decodes a bech32/bech32m segwit address under hrp (BIP-173, BIP-350).
func segwitScript(hrp, address string) ([]byte, error) {
	bad := fmt.Errorf("address %q is not a valid segwit address", address)
	if address != strings.ToLower(address) && address != strings.ToUpper(address) {
		return nil, bad
	}
	address = strings.ToLower(address)
	sep := strings.LastIndexByte(address, '1')
	if sep != len(hrp) || address[:sep] != hrp || len(address)-sep-1 < 7 || len(address) > 90 {
		return nil, bad
	}
	values := []byte{}
	for _, c := range address[:sep] {
		values = append(values, byte(c>>5))
	}
	values = append(values, 0)
	for _, c := range address[:sep] {
		values = append(values, byte(c&31))
	}
	data := make([]byte, 0, len(address)-sep-1)
	for _, c := range address[sep+1:] {
		d := strings.IndexRune(bech32Charset, c)
		if d < 0 {
			return nil, bad
		}
		data = append(data, byte(d))
	}
	polymod := bech32Polymod(append(values, data...))
	version, payload := data[0], data[1:len(data)-6]
	var program []byte
	var acc, bits uint32
	for _, v := range payload {
		acc, bits = acc<<5|uint32(v), bits+5
		for ; bits >= 8; bits -= 8 {
			program = append(program, byte(acc>>(bits-8)))
		}
	}
	if bits >= 5 || acc<<(8-bits)&0xff != 0 { // padding must be < 5 bits, all zero
		return nil, bad
	}
	switch {
	case version == 0 && polymod == 1 && (len(program) == 20 || len(program) == 32):
		return append([]byte{0x00, byte(len(program))}, program...), nil
	case version == 1 && polymod == 0x2bc830a3 && len(program) == 32:
		return append([]byte{0x51, 0x20}, program...), nil
	}
	return nil, errors.Join(bad, errors.New("only witness v0 (20 or 32 bytes) and v1 (32 bytes) are supported"))
}

// P2WPKHScript is the scriptPubKey a Key's compressed public key maps to: the
// one script type a Bitcoin Key signs for (witness v0, `0014<hash160>`).
func P2WPKHScript(publicKey []byte) []byte {
	sha := sha256.Sum256(publicKey)
	h := ripemd160.New()
	h.Write(sha[:])
	return h.Sum([]byte{0x00, 0x14})
}

// AssembleBitcoin is the broadcastable segwit transaction for intent in on net,
// signed by sigs (one r||s||v per input in order, as a Session produces them;
// v is not carried), and its txid in the display order explorers use. Each
// input's witness is DER(r,s)||SIGHASH_ALL and the compressed publicKey, which
// every input's script_pubkey must pay. Identical inputs give identical bytes,
// which is what makes a second broadcast harmless.
func AssembleBitcoin(net string, publicKey []byte, in *coslv1.BitcoinIntent, sigs [][]byte) (raw []byte, txid [32]byte, err error) {
	if len(sigs) != len(in.Inputs) {
		return nil, txid, fmt.Errorf("%d signatures for %d inputs", len(sigs), len(in.Inputs))
	}
	tx, err := newBitcoinTx(net, in)
	if err != nil {
		return nil, txid, err
	}
	want := P2WPKHScript(publicKey)
	for i, x := range in.Inputs {
		if !bytes.Equal(x.ScriptPubkey, want) {
			return nil, txid, fmt.Errorf("input %d does not pay the Key's P2WPKH address", i)
		}
		if len(sigs[i]) != 65 || sigs[i][64] > 1 {
			return nil, txid, fmt.Errorf("signature %d must be 65 bytes r||s||v with v 0 or 1", i)
		}
	}

	// body is everything but the marker, flag and witnesses: the txid commits to it alone.
	var body bytes.Buffer
	body.Write(compactSize(len(tx.inputs)))
	for _, x := range tx.inputs {
		body.Write(x.outpoint)
		body.WriteByte(0) // empty scriptSig
		binary.Write(&body, binary.LittleEndian, uint32(bitcoinSequence))
	}
	body.Write(compactSize(len(tx.outputs)))
	for _, o := range tx.outputs {
		body.Write(o)
	}
	version := binary.LittleEndian.AppendUint32(nil, 2)
	lockTime := binary.LittleEndian.AppendUint32(nil, in.LockTime)

	sum := doubleSHA(append(append(append([]byte{}, version...), body.Bytes()...), lockTime...))
	for i := range sum {
		txid[i] = sum[len(sum)-1-i]
	}

	raw = append(append(version, 0x00, 0x01), body.Bytes()...)
	for _, sig := range sigs {
		der := append(derInts(sig[:32], sig[32:64]), sighashAll)
		raw = append(raw, 0x02)
		raw = append(raw, compactSize(len(der))...)
		raw = append(raw, der...)
		raw = append(raw, compactSize(len(publicKey))...)
		raw = append(raw, publicKey...)
	}
	return append(raw, lockTime...), txid, nil
}

// derInts is the DER SEQUENCE of two unsigned big-endian integers.
func derInts(r, s []byte) []byte {
	enc := func(b []byte) []byte {
		b = new(big.Int).SetBytes(b).Bytes()
		if len(b) == 0 || b[0] >= 0x80 {
			b = append([]byte{0}, b...)
		}
		return append([]byte{0x02, byte(len(b))}, b...)
	}
	body := append(enc(r), enc(s)...)
	return append([]byte{0x30, byte(len(body))}, body...)
}
