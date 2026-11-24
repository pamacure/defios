---
name: review-sign-request
description: How to review a Solana Sign Request before accepting or refusing it.
---
# Reviewing a Solana Sign Request

Read the request with `sign_inspect`. Each of its `details` is one
transaction: `chain` names its cluster (`solana mainnet-beta`), `lines`
read it out, and `intent` holds it whole. Decode it by passing `intent`
unchanged to `solana__tx__decode_transaction`, then check each
transaction, in order:

1. **Cluster.** The cluster is one of your Node's Networks, as
   `key_address` lists them, and the one the talk meant. Solana addresses
   are the same on every cluster, so the same transaction moves funds on
   whichever cluster it names: `mainnet-beta` is real money, `devnet` and
   `testnet` are not. A genesis hash, when given, names the cluster
   exactly: mainnet-beta's is
   `5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d`, devnet's
   `EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG`. A transaction that
   names no valid cluster fails here.
2. **Fee payer.** The fee payer is the Key's address, as `key_address`
   gives it for this Network.
3. **Programs.** Every instruction's program is one the decoder knows: the
   System Program, SPL Token, Token-2022 or Associated Token Account. An
   unknown program fails here unless the talk named it; its accounts are
   then shown without roles, and still checked in step 4. An instruction of
   a known program that the decoder calls unknown, or that does not decode,
   fails here: refuse, saying it could not be decoded.
4. **Each instruction's accounts.** For each instruction, read its accounts
   by role. Where funds go: a `transfer`'s destination and lamports, in SOL
   through `solana__tx__units`. Where tokens go: a token transfer's
   destination is a token account, not a wallet, so derive the recipient's
   with `solana__tx__associated_token_address` from their wallet and the
   mint, and compare. Read the amount in whole tokens through
   `solana__tx__units` and the token's decimals; `transferChecked` states
   its mint and decimals, plain `transfer` does neither. Any instruction
   marked SENSITIVE hands control or rent away: `setAuthority` gives an
   account or mint to someone else, or removes its authority for good;
   `approve` lets a delegate move the Key's tokens later without asking;
   `closeAccount` to a destination other than its owner sends that
   account's rent to someone else. A SENSITIVE instruction the talk did not
   agree in those words fails here. Also check which accounts are writable:
   an account the transaction did not need to change should not be.
5. **Signers.** The Key signs only as the fee payer, so every signer
   account is the fee payer. Another signer is one the Key cannot provide:
   the decoder lists any, and such a transaction fails here.

Then check the request as a whole against the talk: it does what the
Conversation agreed, on the cluster named there, to the addresses named
there, for the amounts named there, and nothing more. Each transaction's
instructions run in order and all or none of them take effect, so their
order is part of what they do.

Accept with `sign_accept` only when every check passes for every
transaction. Otherwise refuse with `sign_refuse` at the first check that
fails.
