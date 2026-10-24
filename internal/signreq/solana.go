package signreq

import (
	"bytes"
	"crypto/ed25519"
	"fmt"

	"defios.com/internal/base58"
	coslv1 "defios.com/proto/cosl/v1"
)

// maxSolanaTx is the largest serialized transaction a cluster accepts.
const maxSolanaTx = 1232

// SolanaMessage is the canonical legacy compiled message of intent in: the
// bytes Ed25519 signs, which is the Digest of a Solana transaction. The fee
// payer is the only signer (the Key signs for exactly one address), accounts
// are ordered signer-writable, signer-readonly, writable, readonly, each by
// first appearance, and a repeated account merges its flags. Pure and
// deterministic, so every Node derives the same bytes.
func SolanaMessage(in *coslv1.SolanaIntent) ([]byte, error) {
	payer, err := solanaKey(in.FeePayer)
	if err != nil {
		return nil, fmt.Errorf("fee_payer: %v", err)
	}
	blockhash, err := base58.DecodeN(in.RecentBlockhash, 32)
	if err != nil {
		return nil, fmt.Errorf("recent_blockhash: %v", err)
	}
	type account struct {
		key              [32]byte
		signer, writable bool
	}
	var accounts []*account
	add := func(k []byte, signer, writable bool) int {
		for i, a := range accounts {
			if bytes.Equal(a.key[:], k) {
				a.signer, a.writable = a.signer || signer, a.writable || writable
				return i
			}
		}
		a := &account{signer: signer, writable: writable}
		copy(a.key[:], k)
		accounts = append(accounts, a)
		return len(accounts) - 1
	}
	add(payer, true, true)
	type compiled struct {
		program  *account
		accounts []*account
		data     []byte
	}
	var ixs []compiled
	refs := func(k []byte, signer, writable bool) *account { return accounts[add(k, signer, writable)] }
	for i, ix := range in.Instructions {
		prog, err := solanaKey(ix.ProgramId)
		if err != nil {
			return nil, fmt.Errorf("instruction %d program_id: %v", i, err)
		}
		c := compiled{data: ix.Data}
		c.program = refs(prog, false, false)
		for _, a := range ix.Accounts {
			k, err := solanaKey(a.Pubkey)
			if err != nil {
				return nil, fmt.Errorf("instruction %d account %q: %v", i, a.Pubkey, err)
			}
			if a.Signer && !bytes.Equal(k, payer) {
				return nil, fmt.Errorf("instruction %d: %s is a signer but only the fee payer can sign", i, a.Pubkey)
			}
			c.accounts = append(c.accounts, refs(k, a.Signer, a.Writable))
		}
		ixs = append(ixs, c)
	}
	// Order by class, keeping first appearance within it.
	class := func(a *account) int {
		switch {
		case a.signer && a.writable:
			return 0
		case a.signer:
			return 1
		case a.writable:
			return 2
		}
		return 3
	}
	var ordered []*account
	var counts [4]int
	for c := 0; c < 4; c++ {
		for _, a := range accounts {
			if class(a) == c {
				ordered = append(ordered, a)
				counts[c]++
			}
		}
	}
	if len(ordered) > 256 {
		return nil, fmt.Errorf("%d accounts; a transaction addresses at most 256", len(ordered))
	}
	index := map[*account]byte{}
	for i, a := range ordered {
		index[a] = byte(i)
	}

	msg := []byte{byte(counts[0] + counts[1]), byte(counts[1]), byte(counts[3])}
	msg = append(msg, shortvec(len(ordered))...)
	for _, a := range ordered {
		msg = append(msg, a.key[:]...)
	}
	msg = append(msg, blockhash...)
	msg = append(msg, shortvec(len(ixs))...)
	for _, c := range ixs {
		msg = append(msg, index[c.program])
		msg = append(msg, shortvec(len(c.accounts))...)
		for _, a := range c.accounts {
			msg = append(msg, index[a])
		}
		msg = append(msg, shortvec(len(c.data))...)
		msg = append(msg, c.data...)
	}
	if size := 1 + ed25519.SignatureSize + len(msg); size > maxSolanaTx {
		return nil, fmt.Errorf("transaction is %d bytes, over the %d-byte limit", size, maxSolanaTx)
	}
	return msg, nil
}

// AssembleSolana is the broadcastable transaction for intent in, signed by sig
// (64 bytes, as a Session produces it). Its id is the signature itself.
func AssembleSolana(in *coslv1.SolanaIntent, sig []byte) (raw []byte, err error) {
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("signature must be %d bytes", ed25519.SignatureSize)
	}
	msg, err := SolanaMessage(in)
	if err != nil {
		return nil, err
	}
	raw = append(shortvec(1), sig...)
	return append(raw, msg...), nil
}

func solanaKey(s string) ([]byte, error) {
	k, err := base58.DecodeN(s, 32)
	if err != nil {
		return nil, fmt.Errorf("%q is not a base58 32-byte key", s)
	}
	return k, nil
}

// shortvec is Solana's compact-u16 length prefix.
func shortvec(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}
