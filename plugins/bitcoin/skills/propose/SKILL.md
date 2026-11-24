---
name: propose
description: How to propose a Bitcoin Sign Request from what the talk agreed.
---
# Proposing a Bitcoin Sign Request

Propose only what the Conversation agreed: the network, the recipients and
the amounts named there.

`chain_fill` refuses Bitcoin, and nothing here looks up which coins the Key
holds. So build only from coins the talk or the person gave: each one's
`txid`, `vout` and amount, an output paying the Key's address on this
Network. Never invent one. When none were given, or they do not cover the
amounts and the fee, say in the talk what is needed in place of proposing.

1. **Write each transaction.** It names one of your Node's Networks by
   alias, as `key_address` lists them, and holds a `bitcoin` intent with
   `network`, `inputs` and `outputs`. `network` is the bitcoin network the
   talk meant (`mainnet`, `testnet`, `signet` or `regtest`); the decoder
   needs it, and `sign_propose` refuses one that is not the Network's.
   Leave out `lockTime` unless the talk agreed one.
   - Each input has `txid` (64 hex), `vout`, `amountSats`, and
     `scriptPubkey`: the Key's script, the `script_pubkey` that
     `bitcoin__tx__address` gives for the Key's address, from the
     `key_address` Tool. The Key spends only from its own address.
   - Each output has `address` and `amountSats`: one per recipient the talk
     named, in sats from `bitcoin__tx__units`.
   - Change goes back to the Key's address: the inputs' total, less the
     outputs, less the fee. Leave it out only when the decoder flags it
     dust; it then goes to the fee.
2. **Choose the fee** deliberately: a rate in sat/vB the talk or the person
   gave, else a modest one you name in your reason. Decode a draft, change
   output included, with `bitcoin__tx__decode_transaction`, giving the Key's
   address as `key_address`, and take its estimated vsize: the fee is the
   rate times the vsize, in whole sats. Set the change from it.
3. **Check what you wrote,** as a reviewer will: decode it again with
   `key_address`. No input is FOREIGN, nothing is flagged, each recipient
   gets the amount the talk named, the change is marked, and the fee and its
   rate are the ones you chose.
4. **Propose** with `sign_propose`, giving the transactions and your own
   reason.
