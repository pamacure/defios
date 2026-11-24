---
name: propose
description: How to propose a Solana Sign Request from what the talk agreed.
---
# Proposing a Solana Sign Request

Propose only what the Conversation agreed: the cluster, the recipients and
the amounts named there.

1. **Write each transaction.** It names one of your Node's Networks by
   alias, as `key_address` lists them, and holds a `solana` intent with
   `instructions` and nothing else: leave `feePayer`, `recentBlockhash`,
   `cluster` and `genesisHash` to `chain_fill`. Each instruction has:
   - `programId`: the program, base58.
   - `data`: base64 from `solana__tx__encode_instruction`, given the
     program, the instruction and its arguments. An amount is in lamports
     or the token's base units, from `solana__tx__units` (with the token's
     decimals). It also names the instruction's accounts, in order.
   - `accounts`: those accounts, in that order, each a `pubkey` with
     `signer` and `writable` set. Only the Key, which pays the fee, can
     sign.

   The common ones, the Key's address as `key_address` gives it:
   - **Send SOL:** System Program `transfer`; the source is the Key, signer
     and writable, the destination writable.
   - **Send a token:** `transferChecked` of the mint's token program
     (SPL Token, or Token-2022 if the mint is one), with the amount and
     the token's decimals. Accounts: the Key's token account (writable),
     the mint, the recipient's token account (writable), and the Key
     (signer). Get both token accounts from
     `solana__tx__associated_token_address`, with the wallet, the mint and
     the token program. If the recipient's may not exist yet, first
     Associated Token Account `createIdempotent`: the Key as funding
     account (signer, writable), the recipient's token account (writable),
     then the recipient's wallet, the mint, the System Program and the
     token program, all read-only.

   Put the instructions in the order they must run: creating a token
   account before sending to it.
2. **Check what you wrote,** as a reviewer will: decode each instruction
   with `solana__tx__decode_instruction` and confirm it is the one the talk
   agreed, to the accounts named there, with nothing marked SENSITIVE that
   the talk did not ask for.
3. **Fill it** with `chain_fill`. It sets the fee payer to the Key, the
   cluster and genesis hash to the Network's, and reads a recent blockhash, which expires in about 60–90 seconds. From
   here, do nothing else until you propose.
4. **Check what it filled:** the cluster is the one the talk meant and the
   fee payer is the Key's address. When one is not, say so in the talk in
   place of proposing.
5. **Propose** with `sign_propose` straight away, giving the `draft`
   `chain_fill` returned and your own reason: the Draft is proposed exactly
   as filled, with nothing copied.
