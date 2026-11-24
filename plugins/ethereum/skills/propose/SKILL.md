---
name: propose
description: How to propose an Ethereum Sign Request from what the talk agreed.
---
# Proposing an Ethereum Sign Request

Propose only what the Conversation agreed: the chain, the recipients and
the amounts named there. Never copy call data or a filled transaction by
hand: describe the call, and propose the Draft the Node filled.

1. **Write each transaction.** It names one of your Node's Networks by
   alias, as `key_address` lists them, and holds an `ethereum` intent with
   these fields and no others:
   - `to`: the recipient's address as `0x` hex, as the talk gave it. A
     mixed-case address is checked against its EIP-55 checksum. For a
     token, `to` is the token contract, and the recipient is an argument
     of the call.
   - `value`: the ether sent, in decimal wei, from `ethereum__tx__units`;
     `"0"` for a token call.
   - `call`, for a contract call: the function's signature and its
     arguments, which the Node encodes into the call data:
     ```json
     {"signature": "transfer(address to, uint256 amount)",
      "args": ["0x60d8c2D6A6B6d7e36D1b2f1E3b11a02f3B1309E5", "100000"]}
     ```
     Arguments are positional. Give every integer as a string, an address
     as `0x` hex, and an array or tuple as a JSON array. A token amount is
     in the token's base units, from `ethereum__tx__units` with its
     decimals. A plain transfer has no `call`.

   List the transactions in the order they must execute: an approval before
   the call that spends it.
2. **Fill them** with `chain_fill`. It returns the completed transactions,
   a note on each, and a `draft` id.
3. **Check the notes,** as a reviewer will: the chain is the one the talk
   meant, the fees are plausible for the chain, and the gas limit is near
   what each call needs (on Ethereum and OP-stack chains, 21000 for a plain
   transfer and under 100000 for a token transfer or approval; Arbitrum's
   runs several times higher, as it pays for L1 data). A call's note shows
   it read back from the encoded data, such as
   `transfer(address to = 0x60d8…09E5, uint256 amount = 100000)`: confirm
   it is the call the talk agreed. When a check fails, say so in the talk
   in place of proposing.
4. **Propose** with `sign_propose`, giving the `draft` and your own reason.
   The Draft is proposed exactly as filled. To change anything, write the
   transactions again and fill again. A Draft lasts 10 minutes; if it has
   gone, fill again.
