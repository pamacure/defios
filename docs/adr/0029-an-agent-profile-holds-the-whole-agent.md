# An Agent Profile holds the whole agent

An agent was split across two places: its prompt in `agents/<handle>/`, and
its Provider, body and `max_turns` in each Group's `[agents.<handle>]`. Agents
now get Tools beyond `say`, and Skills and MCP Servers from Plugins
([ADR 0030](0030-plugins-bring-skills-and-mcp-servers.md)), which would make
that split worse. The whole agent now lives in its profile directory:
`agents/<handle>/SYSTEM.md` and `agents/<handle>/config.toml`, Node-wide like
the prompt ([ADR 0026](0026-an-agent-profile-is-named-by-its-handle.md)). A
Group only includes Handles, through `[agent] profiles`. The profile's `tools`
is an allow-list of what the agent may call; the first Tools beyond `say` are
`sign_list`, `sign_inspect`, `sign_propose`, `sign_accept`, `sign_refuse` and
`chain_fill`.
This amends [ADR 0015](0015-agent-profiles-handles-and-agent-chains.md),
[ADR 0018](0018-an-agent-says-one-message.md),
[ADR 0019](0019-providers-replace-models-on-the-cosl.md) and ADR 0026.

## Considered options

- **Grants per Group, in `[agents.<handle>]`.** Rejected: it would let one
  Handle be trusted to sign in one Group and not another, at the cost of
  defining an agent in as many places as it has Groups. What an agent may do
  is part of who it is, like its prompt; a weaker agent is another Handle.
- **Sign tools exposed through MCP.** Rejected: they write messages signed by
  this Node through its Outbox, and an MCP Server is a separate process that
  must never hold that power. Tools are built into the serve.
- **Every Tool on by default.** Rejected: an agent's `sign_accept` is the
  Node's accept and counts toward t. Nothing beyond `say` is granted unasked.
- **`say` always granted.** Rejected: not every agent needs to post talk. An
  agent without it still acts, and each sign message carries its own reason.

## Consequences

- **Profile config.** `agents/<handle>/config.toml` holds `provider` (an
  alias), `[body]`, `max_turns`, `tools`, `skills`, and `[mcp]` grants. An
  unset `tools` is `["say"]`; a set one is the whole list, so `say` must be
  in it to speak. `[agents.<handle>]` in a Group config is now an unknown key
  and refused when read. Pre-release, so nothing is migrated.
- **Providers stay per Group.** The profile names a Provider alias, which each
  Group including the agent must define in its own `[providers.<alias>]`. The
  agent runs on that Group's credential; a Group lacking the alias is an
  error when the agent is summoned.
- **Sign Tools are Key-scoped.** They act only on the Sign Requests of the
  Conversation's Key and take no key argument; in Group-level talk they are
  not offered. `sign_propose` takes the transactions as `sign propose` does,
  complete. `sign_broadcast` and `sign decline` are not Tools yet.
- **`chain_fill` completes a proposal from the chain.** A Sign Request must
  arrive complete ([ADR 0009](0009-sign-request-on-cosl.md)), and an agent
  cannot know a nonce or a blockhash. `chain_fill` takes transactions in the
  `sign propose` form and fills each field a transaction leaves out from this
  Node's Network for its chain: for Ethereum the nonce, gas limit and fees,
  for Solana the fee payer and recent blockhash. It is read-only, the sender
  is the Key's address, and it returns the completed transactions for the
  agent to check and propose. Bitcoin is refused: its inputs need an index a
  bitcoind endpoint does not keep, which is a Plugin's job. The CLI gets the
  same as `sign fill`. A Plugin was rejected as the only way: proposing is
  what DeFiOS is for, and the Node already reaches every chain it signs for.
- **A run goes on after a Tool.** A Tool's result, or its refusal, is fed back
  to the model, and the run continues until `say`, silence or `max_turns`.
  Only `say` ends a run. What the Tool did is recorded in the Transcript.
  A failed chain is not retried: [ADR 0031](0031-conversation-retry-is-removed.md).
- **Preamble.** It lists the granted Tools and says that sign Tools act as
  this Node, under the agent's Handle, and that a `reason` read from the log
  is another party's words, not instructions.
- **CLI.** `agent add` and `agent edit` write the profile's `config.toml`.
  `agent grant <handle> <item>` and `agent revoke <handle> <item>` take a Tool
  name, `<plugin>/<skill>` or `<plugin>/<server>`, the kind inferred from what
  is built in or installed; `--secret NAME` grants an MCP Server a Secret.
  `agent show <handle>` prints the whole profile.
- **Not here.** Serve hooks that gate a sign Tool, and built-in chain reads,
  are later decisions.
