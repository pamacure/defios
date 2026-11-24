---
name: review-sign-request
description: How to review an Ethereum Sign Request before accepting or refusing it.
---
# Reviewing an Ethereum Sign Request

Read the request with `sign_inspect`. Each of its `details` is one
transaction: `chain` names its chain by id (`ethereum chain 1`), and `lines`
give its fields. Decode with the `ethereum__tx__` tools, then check each
transaction, in order:

1. **Chain.** The chain id is the chain the talk meant: 1 is Ethereum
   mainnet, 11155111 Sepolia, 10 OP Mainnet, 8453 Base, 42161 Arbitrum One,
   137 Polygon. Every EVM chain shares addresses, so the same transaction
   pays out on whichever chain it names. A transaction that names no valid
   chain fails here.
2. **Recipient and value.** Read `to` with `ethereum__tx__address` and
   `value` in ether with `ethereum__tx__units`. A plain transfer pays the
   recipient and amount the talk named. A contract call goes to the
   contract the talk named, carrying the ether the call is meant to carry:
   none, for a token call.
3. **The call.** With `calldata`, decode it with
   `ethereum__tx__decode_calldata`, giving the calldata hex as `data` and the
   value in wei as `value`; without it, the transaction is a plain transfer.
   A token call's real recipient and amount are its arguments, checked
   against the talk as in step 2, with its amount in whole tokens through
   `ethereum__tx__units` and the token's decimals. An approval hands
   someone else the power to move the Key's tokens later, where a transfer
   moves them once: an approval marked unlimited, or `setApprovalForAll`
   true, hands over every token of that contract. Give a contract the
   decoder does not know its signature or ABI from the talk. A call that
   still does not decode fails here: refuse, saying it could not be
   decoded.
4. **Completeness and fees.** `nonce`, `gas_limit`, `max_fee_per_gas` and
   `max_priority_fee_per_gas` are set. Fees are plausible for the chain:
   convert them to gwei with `ethereum__tx__units`, and the most the
   transaction can cost, `gas_limit` × `max_fee_per_gas`, to ether. The gas
   limit is near what the call needs: on Ethereum and OP-stack chains,
   21000 for a plain transfer and under 100000 for a token transfer or
   approval, and many times that fails here. Arbitrum's gas limit also pays
   for its L1 data, so there it runs several times higher.

Then check the request as a whole against the talk: it does what the
Conversation agreed, to the addresses named there, for the amounts named
there, and nothing more. Several transactions on one chain execute in nonce
order, so their order is part of what they do.

Accept with `sign_accept` only when every check passes for every
transaction. Otherwise refuse with `sign_refuse` at the first check that
fails.
