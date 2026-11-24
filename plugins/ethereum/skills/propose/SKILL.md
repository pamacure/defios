---
name: propose
description: How to propose an Ethereum Sign Request from what the talk agreed.
---
# Proposing an Ethereum Sign Request

Propose only what the Conversation agreed: the chain, the recipients and
the amounts named there.

1. **Write each transaction.** It names one of your Node's Networks by
   alias, as `key_address` lists them, and holds an `ethereum` intent with
   these fields and no others:
   - `to`: the recipient's address in base64, from `ethereum__tx__address`.
     For a token, `to` is the token contract, and the recipient is an
     argument of the call.
   - `value`: the ether sent, in decimal wei, from `ethereum__tx__units`;
     `"0"` for a token call.
   - `data`, for a contract call: base64 from
     `ethereum__tx__encode_calldata`, given the function's signature and its
     arguments. A token amount is in the token's base units, from
     `ethereum__tx__units` with its decimals. A plain transfer has no
     `data`.

   List the transactions in the order they must execute: an approval before
   the call that spends it.
2. **Fill them** with `chain_fill`.
3. **Check what it filled,** as a reviewer will: the chain ID is the chain
   the talk meant, the fees are plausible for the chain, and the gas limit
   is near what each call needs (on Ethereum and OP-stack chains, 21000 for
   a plain transfer and under 100000 for a token transfer or approval;
   Arbitrum's runs several times higher, as it pays for L1 data). Decode
   your own `data` with `ethereum__tx__decode_calldata` and confirm it is
   the call the talk agreed. When a filled field fails a check, say so in
   the talk in place of proposing.
4. **Propose** with `sign_propose`, giving the transactions `chain_fill`
   returned and your own reason.
