---
name: review-sign-request
description: How to review a Bitcoin Sign Request before accepting or refusing it.
---
# Reviewing a Bitcoin Sign Request

Read the request with `sign_inspect`. Each of its `details` is one
transaction: `chain` names its network (`bitcoin mainnet`), `lines` read it
out, and `intent` holds it whole. Decode it by passing `intent` unchanged to
`bitcoin__tx__decode_transaction`, with `key_address` set to the Key's
address for this Network, from the `key_address` Tool. It lists Bitcoin
addresses only for a Key your Node uses for Bitcoin; with none, the inputs
and change cannot be checked, so do not accept, and say the Key is not
marked for Bitcoin on your Node. A transaction that
does not decode is refused, saying it could not be decoded. Otherwise check
each transaction, in order:

1. **Network.** The intent's `network`, which `chain` also names, is the
   one the talk meant, and one of your Node's Networks, as `key_address`
   lists them: `mainnet` is real money, `testnet`, `signet` and `regtest`
   are not. Every output's address is of that network: the decoder flags
   one that is not, and such a transaction fails here. Testnet, signet and
   regtest share address forms, so an address alone cannot tell them apart;
   check those against the talk.
2. **Inputs.** Each input spends from the Key's address. The decoder marks
   one that does not FOREIGN: the Key cannot sign for it, and a transaction
   with one fails here. Each input's `txid:vout` and amount are the coins
   the talk or the person named. The decoder looks none of them up: they
   are as the intent states them.
3. **Outputs and change.** Each output not marked change pays a recipient
   the talk named, the amount named there: read it in BTC with
   `bitcoin__tx__units`. Change, if any, pays the Key's address, which the
   decoder marks. An output that is neither fails here, as does one flagged
   dust, which nodes will not relay, or not a valid address. A `lock_time`
   the talk did not agree fails here too: it holds the transaction back
   until that block or time.
4. **Fee.** The fee is the inputs' total less the outputs': whatever is not
   paid out goes to the miner. Read it in BTC, and the decoder's estimated
   fee rate in sat/vB, which may run slightly high. A rate the decoder flags
   fails here, as does a fee out of proportion to what the transaction
   sends: no change output, and inputs worth far more than the outputs, is
   the classic way to lose a whole coin to the miner. A rate the talk
   agreed is the one to check against; without one, from one to a few tens
   of sat/vB is usual.

Then check the request as a whole against the talk: it does what the
Conversation agreed, on the network named there, to the addresses named
there, for the amounts named there, and nothing more. Two transactions that
spend the same `txid:vout` conflict, and only one of them can confirm.

Accept with `sign_accept` only when every check passes for every
transaction. Otherwise refuse with `sign_refuse` at the first check that
fails.
