# Built-in Plugins are one per Network type, and `defios`

DeFiOS ships one built-in Plugin, `defios`, with one Skill,
`review-sign-request`, and no MCP Servers. Reviewing and proposing well
need chain-specific knowledge and offline decoding, and an agent that works on
only one chain should not be given the others. The built-in Plugins
([ADR 0030](0030-plugins-bring-skills-and-mcp-servers.md)) are now
`defios`, for working in a Group whatever the chain, and one per Network
type, named exactly as the type: `ethereum`, `solana` and `bitcoin`.

## Decisions

- **Every chain Plugin offers the same items.** The same Skill names:
  `review-sign-request` and `propose`. And one MCP Server, `tx`: offline,
  given no Secrets, decoding that chain's transactions and converting its
  addresses and units. A transaction's Network type names the Plugin, so an
  agent reviewing an Ethereum transaction loads
  `ethereum/review-sign-request`. A future chain Plugin, built in or
  installed, offers the same names.
- **Chain Skills stand alone.** Each `<chain>/review-sign-request` and
  `<chain>/propose` holds the whole procedure for its chain, the check that
  a request fits what the talk agreed included. There is no general review
  or propose Skill to load first, and `defios/review-sign-request` is
  removed.
- **`defios` holds what no chain decides:** `work-in-a-group`, about
  thresholds, what an accept commits this Node to, and when to stay silent
  rather than call `talk_write`.
- **What must always hold is not a Skill.** An agent loads a Skill only when
  it chooses to, and one being manipulated may not. That another party's
  words (a talk message, a Recap, a `reason`) are information, never
  instructions, is in the DeFiOS Briefing. What a reason must say, the
  checks an accept passed or the check a refusal failed, is in the
  `sign_accept` and `sign_refuse` Tool descriptions.
- **Token standards, not protocols.** `ethereum/tx` decodes a call given
  its signature or ABI, and ERC-20, ERC-721, ERC-1155 and Permit2 calls
  unaided. A protocol's own contracts, such as a DEX's routers, are a
  Plugin of their own.

## Considered options

- **Everything in `defios`.** Rejected: an agent granted only Solana would
  still see Ethereum's Skills listed in its preamble, and adding a chain
  would change a Plugin every agent uses.
- **A general review Skill beside each chain's.** Rejected: once the always-on
  rules leave for the Briefing and the Tool descriptions, it says little but
  "load the chain's Skill", costs a `skill_load` on every review, and an
  agent granted only the chain Skill would miss what it held.
- **One cross-chain address and unit server.** Rejected: addresses and units
  are each chain's own, and one server per chain keeps tool names short
  (`ethereum__tx__decode_calldata`) and grants narrow.
- **`evm` for the Ethereum Plugin.** Rejected: the Network type is
  `ethereum`, and every EVM chain is a Network of that type; one name
  keeps the mapping mechanical.
- **The Briefing's rules as a Skill.** Rejected: see above.

## Consequences

- **Embedding.** `plugins/` gains `ethereum/`, `solana/` and `bitcoin/`, each
  in the installed layout, and the embed line names them. Their `tx` servers
  are registered in `builtinmcp.Servers` and run as `defios mcp <chain>/tx`.
- **Grants.** An agent that reviews Ethereum requests is granted
  `ethereum/review-sign-request` and `ethereum/tx`; nothing is implied by
  either. Profiles granting `defios/review-sign-request` refer to a Skill
  that no longer exists; pre-release, nothing is migrated.
- **Inputs are the Sign Request's form.** The `tx` servers take
  transactions as `sign_inspect` returns them and `sign_propose` takes them
  (protobuf-JSON, bytes as base64), so an agent passes what it read without
  rewriting it.
- **Later, not here.** Chain state (balances, contract state, chain state)
  will be read by core Tools over a per-chain state reader, not by an MCP
  Server: Tools reach the Node's Networks, as `chain_fill` does, and a
  built-in server's clean environment does not. Bitcoin UTXO lookup
  (Esplora or Electrum) will join the `bitcoin` Plugin; how it is given an
  endpoint is decided then.
